# Experimental authenticated transfer API

`internal/transferapi` connects the delta, staging, journal and native publisher
over HTTP/1 with TLS 1.3. Local tests use real loopback TLS connections and
temporary client/server files. No production helper listener or daemon client is
deployed. The installed observer still reports `automaticWrites: false`.

## Authentication and policy

`POST /v1/ack` now records authenticated, exclusion-policy-bound client progress in
ranges of up to 32 committed batches. Both sides require writes permission. The
worker sends only its durable completed prefix and separately records confirmed
acknowledgements; fetching a page does not advance progress. Exact retries are
idempotent and caught-up workers send nothing. [Protocol, bounds and tests](acknowledgements.md)
also explain why these client claims alone cannot authorize history reclamation.

Every request requires a verified client certificate whose SHA-256 leaf
fingerprint is in the configured allowlist (one to 64 fingerprints). The mapping
binds it to an opaque client ID; a proposal cannot supply another client's ID.
A certificate signed by the CA but absent from that mapping is rejected.
Recovery is scoped to that client's exact operation ID.

Every request also requires `X-Ananas-Namespace` matching the coordinator's
persistent namespace. It is checked after certificate authentication and before
any operation, including state reads. The client requires this identity in its
configuration and sends it on every request. Reusing a trusted certificate and
endpoint for a different journal therefore cannot silently redirect an upload.
Provisioning that namespace is a separate setup responsibility; feed bootstrap
can omit its JSON namespace but still requires the configured header binding.

The constructor requires a policy-gate callback; nil is refused. It is checked
before an operation and again before commit/recovery. **This callback is not an
OS egress rule.** The real helper/client must supply verified interface/address,
route and namespace binding, certificate provisioning and enforced egress. Tests
use an explicit local gate. `Writes` defaults to false. Passing true in a test
does not establish any production capability gate.

Use the bounded listener and HTTP-server settings together. They cap accepted
connections and header/operation lifetimes, disable TCP keepalive probes and
HTTP/2, and expire idle connections. The wrapper does not itself bind a socket,
select an interface or configure TLS trust. These deployment responsibilities
must not be omitted by a future executable.

## Requests

Binary request metadata uses a four-byte unsigned big-endian length followed by
strict JSON. The decoder validates length before allocation, rejects unknown
fields and trailing JSON, and consumes exactly that frame before delta bytes.
Selection frames are at most 8 KiB; apply frames are at most 128 KiB.

| Endpoint | Behavior |
|---|---|
| `GET /v1/state` | Current coordinator epoch and write-gate setting. |
| `GET /v1/events` | Persistent authenticated, coalesced committed-sequence hints; no paths or payload. |
| `GET /v1/operation?id=...` | This authenticated client's exact durable operation or null, with the journal namespace; does not stage or publish content. |
| `GET /v1/head?path=...` | Committed version and pending-publication flag for one allowed path. |
| `POST /v1/changes` | Framed namespace/policy, cursor, page limit and client exclusions; returns contiguous filtered committed batches without advancing any cursor. |
| `POST /v1/signature` | Framed path/version selection; returns a 64 KiB-block signature for that committed retained file version. |
| `POST /v1/delta` | Framed selection followed by a bounded binary receiver-base signature; streams a delta for that committed retained version. |
| `POST /v1/apply` | Framed epoch, proposal and one delta length per entry, followed by those exact delta streams; stages then commits a bounded batch. |
| `POST /v1/recover` | Framed epoch/operation ID; replays that authenticated client's prepared operation. |

Directories and tombstones have zero delta length. Parent directories must be
committed before children. File streams use 64 KiB blocks. The handler compares
each stream's declared base, target size and block size with approved metadata
before any staging output. It checks the received whole-content digest before
publication. The reconstructed batch-size cap cannot be bypassed with a tiny
stream containing many copy references.

Selections must match a committed version bound to the supplied path. This may
be a retained historical version after later replacement or deletion; reading it
does not change the current head or publish its bytes. Current pending work,
excluded paths, directory/tombstone content requests and cross-path version
lookups are rejected. Version metadata stores the binding atomically at commit,
so lookup does not scan history or enumerate files. Older metadata without that
binding is accepted only when it is the supplied path's current head; unbound
historical access requires explicit migration/reconciliation and fails closed.
The change feed reads committed journal metadata only; there is no polling or
automatic filesystem listing behind it.

### Change pages and durable receipt

The feed accepts one to 32 batches per request. Its strict framed request is at
most 16 KiB, with at most 128 exclusion patterns, 4 KiB per pattern and 8 KiB of
combined pattern text. Each returned batch is at most 128 KiB; the page adds at
most 4 KiB of envelope allowance. Client response reads use the requested page
limit. Both the server's and client's exclusions apply before returning entries.
Directory tombstones conservatively use directory exclusion rules. Omitted paths
and version descriptions are replaced by a count; an entirely excluded batch
still has its committed operation ID and sequence. Filtering does not read any
visible file or directory.

The response binds the persistent journal namespace and a digest of the feed
format plus both exclusion lists. Only a sequence-zero bootstrap may omit these
identities. A changed namespace or exclusion policy rejects continued replay;
explicit reconciliation is required before newly included paths can be handled.
Contiguous sequences and committed flags are validated at both ends. A journal
gap or future cursor is an error, not an empty up-to-date result.

`index.ReceiveRemotePage` persists the complete page and receive cursor in one
bbolt transaction. It permits one outstanding page, accepts an identical retry,
and retains local dirty flags. `NextRemoteBatch` returns only the oldest batch.
`CompleteRemoteBatch` checks existing matching durable file bases or conflict
identities for every visible entry, retains an aggregate excluded-entry count,
and advances completion in the same transaction that removes that pending batch.
Conflict identities alone do not retain content: the future engine must preserve
those bytes before registering a conflict. Directory completion now requires an
explicit directory base; an empty-file manifest with the same ID cannot satisfy
it. Directory bases retain generation-sensitive dirty bookkeeping across restart.

Fetching/receiving a page never acknowledges it to the server, performs content
publication or asserts that the pair is synchronized. Remote cursor updates,
replay scheduling/coalescing, policy-change reconciliation and the
scheduler remain unfinished. A single page provides backpressure rather than
an unbounded on-disk job queue.

## Completion, retries and failures

Apply returns a committed journal record and content statistics for newly staged
entries. Retrying identical committed input returns its original record without
staging/publishing again; statistics are absent on that idempotent reply. Reusing
an operation ID with other input is rejected. Client IDs cannot be spoofed through
proposal JSON. Request data already transmitted during a retry is still real
traffic and must be counted by the future client; content statistics are not
wire counters.

A failed publication retains prepared work for scoped recovery. Before content
receipt, the handler now reserves the complete proposal as one durable intake.
The coordinator checks unused candidate names and holds exclusive ownership
through receiving and publication. A failed upload can retain earlier verified
candidates; exact retries may revalidate their size/digest and reuse them, or
discard that intake's abandoned partials. A different proposal/client cannot
adopt them. The transport store must be the journal's pinned native directory.

New delta candidates are checked against the approved target size/digest before
sealing, using the decoder's digest without a second whole-content hash. A wrong
target digest leaves no ready candidate. On retry, already retained candidates
are hashed once through bounded paced reads before reuse. The same bounded wire
spools are still transmitted; the handler checks their headers and consumes the
declared lengths for framing, but uses the verified candidate instead of decoding
those repeated streams. Statistics for reused candidates have zero newly decoded
literal/copy bytes and their target digest; they are not wire-byte savings.

Intake becomes prepared publication atomically after successful receipt. An
unresolved intake blocks other new commits; it is not visible file publication,
has no sequence and does not appear in operation-status reads. Unknown artifacts
from older/unowned staging are refused, with no automatic adoption or cleanup.
The assembled native helper now requires a durable publication budget and returns
507 before delta decoding/staging when a new reservation cannot fit. Its charges
survive retries and commits. PC mode now shares reservations between upload spools
and downloaded publications, and the automatic worker checks them before dispatch.
Physical disk limits, abandoned-intake resolution and orphan/history
cleanup are still required before production writes. See [scope](cache-budget.md).

A download may fail after response headers have been sent. The client must
verify the delta, expected target size/digest **and** the final `X-Ananas-Error`
trailer before granting publication permission. A complete-looking byte stream
alone is insufficient. Mid-operation route loss must also cancel the connection
through the integrated client/OS policy. The library client below supports
cancellation, but production policy and daemon integration remain unfinished.

## Client library

`NewClient` requires a private IPv4 literal and port, explicit source IP and
interface, CA trust, client certificate, server leaf SHA-256 pin, client ID,
remote journal namespace,
file/batch limits and a policy callback. Loopback is allowed explicitly for local
tests with interface `lo`. Connections bind both source and device and refuse
binding failures; no DNS endpoint, environment proxy or redirect is used. TLS
1.3 and HTTP/1 are required. This is not an OS guarantee against a route changing
to a gateway on that interface; actual egress enforcement is still mandatory.

Only one operation may run per client, with no waiting work queue. Each has a
45-second context and a five-second connect/TLS timeout. It retains one active
content/metadata operation; an optional notification stream uses a second
connection. The transport now caps connections at two, with one idle connection.
TCP keepalive probes are disabled; an idle connection expires after 15 seconds.
`Interrupt` cancels active work and closes idle connections. The policy callback
must prevent subsequent work when paused or off-LAN. `Close` rejects new work.

The typed methods validate selections and proposals, bound strict JSON/binary
responses and verify acknowledgements against the submitted operation. Download
checks the declared base and exact selected target size before any staged output,
then verifies the entire stream, selected digest and final failure trailer before
returning success. The caller must keep output unpublished on every error and
supply bounded storage, source/output pacing and local generation validation.
Apply consumes prebuilt bounded delta streams without buffering whole files or
creating per-file workers. No application retry loop is implemented.

Traffic counts bytes read/written on the encrypted TCP stream, including TLS
handshakes and transmitted retry data. It excludes TCP/IP headers and kernel
retransmissions, so it is not packet-level wire accounting. Notifications use one
coalesced slot with no polling or idle heartbeat. These counters are not yet
connected to the deployed dashboard or indicator.

### Explicit local replica publication

`Client.DownloadVersion` implements the `internal/replica` downloader contract.
The puller accepts one already selected, validated proposal and requires an
explicit write flag, policy callback, file/batch limits and local I/O rate.
It checks all paths and expected local heads before downloading, then builds
signatures from receiver-local immutable bases. Download output goes through
`stage.ReceiveVerified`, so size/digest mismatch or a late trailer/gate error
cannot produce a ready candidate. Successfully staged content is committed
through the local journal and native publisher.

The puller permits one active batch, with a 45-second context and no queued
producer, automatic retry, page fetch or polling. Signature reads, reused base
reads and staged writes share a per-file pacing budget. The caller supplies
dependencies for the same private native replica state outside both sync roots,
and binds the selected proposal to the authenticated remote namespace/feed.
There is no production constructor wiring those dependencies yet.

Identical committed retry does no network work. A prepared operation is recovered
only through its exact proposal. A batch that failed while staging can reuse
earlier candidates after bounded local verification; an interrupted partial is
still subject to explicit owner-aware cleanup. External visible edits are
preserved as a publication error, with retained candidates requiring resolution.
Per-batch limits do not enforce aggregate retained-storage quotas.

The puller returns the durable local commit; it does not update the observer's
manifest bases, complete the received inbox, acknowledge the NAS or mark the
application synchronized. Those operations, upload integration, conflict-content
retention/resolution and scheduler coalescing remain unfinished. Neither the
daemon nor any automatic service invokes this puller.

## Local validation and remaining work

The local index now has a one-batch upload outbox (`index.Upload`), capped at
160 KiB of encoded metadata, with up to 128 entries and 8 GiB reconstructed
content. It retains the exact namespace/proposal, observed generations, delta
lengths and spool digests before transmission; content/spools are retained
separately. Preparation reserves room for the later commit receipt.
Preparation checks current generation/type/size/base and observed exclusion,
pause and conflict state. Deletion needs an acknowledged base. Identical prepare
retry preserves the original operation even after a newer local edit.

Operation-status replies are scoped to the certificate's client and allowed
paths. The typed client checks the expected namespace/operation and response
bounds. `replica.RecoverUploadReceipt` makes at most one such metadata request,
or none if the committed receipt is already durable locally. Only a committed
record matching the complete original proposal can be saved as its receipt.
Prepared and unrecorded outcomes remain distinct; neither grants permission to
retry, discard remote staging or clear local work. A null operation can coexist
with durable intake or older unowned staging. Exact intake retries are supported
by apply; resolving unowned artifacts remains unfinished.

`CompleteUpload` requires both the remote receipt and matching durable local
bases before releasing outbox metadata. It does not clear newer dirty generations
or remove content spools. Source snapshotting and spool creation now have explicit
local implementations, but production dispatch wiring, quotas, conflict handling and scheduling
are still not connected to the daemon.
The TLS recovery test deliberately omits saving a successful upload response and
reopens the local index, then recovers the receipt without another upload. It is
a local missing-acknowledgement simulation, not a network-disconnect NAS test.

`content.Root.Snapshot` copies one selected, fingerprint-checked local file in a
single paced source pass. It collects 64 KiB block digests while `stage.Capture`
computes the whole digest and seals the exact-size private candidate. The source
path/root/fingerprint are checked again before sealing. Later edits cannot alter
the captured bytes. The caller supplies state outside both roots and must still
check observed generations, exclusions, policy and aggregate quota.

`replica.EncodeUpload` reads that immutable candidate against an authenticated
base signature and produces `ID.wire.ready` separately from `ID.ready` content.
It verifies the encoded target digest before sealing and refuses an existing
spool. `OpenUploadWire` checks the outbox's size/digest and file fingerprint using
bounded paced reads, rewinds the handle, and exposes at most the recorded number
of bytes at the caller's rate/context. It does not re-encode or silently replace
an old spool. Partial-wire cleanup requires proof that its encoder is no longer
live. Durable quota/reservation and orphan cleanup remain unfinished.

The snapshot upload TLS test performs the actual source snapshot, delta spool,
durable outbox, upload and acknowledgement sequence on temporary PC files. A
one-byte prefix insertion reuses a 256 KiB remote base. An edit deliberately made
after snapshot creation remains dirty when the older captured version completes.
This validates stable transfer input and bookkeeping, not automatic scheduling.

`replica.Pusher.Dispatch` handles exactly one durable outbox with explicit write
permission, size/rate limits and a policy callback. It checks local pause,
exclusion and conflict flags, queries the exact remote operation, then either
saves an existing commit, recovers prepared publication or verifies the retained
wire spools and submits a new apply request. Recovery uses the current remote
epoch and the complete original proposal. A saved local receipt returns without
network work. Before transmission, the dispatcher rechecks the outbox and path
state; a newer observed generation is permitted because the immutable snapshot
remains the selected version and later edits must remain dirty.

One active batch has a 45-second context and no queue, polling or retry loop.
It can hold up to 128 read-only spool handles, closed on success/error/cancellation.
Policy is checked between spool verifications; full selected-path checks run a
constant number of times per batch, avoiding quadratic index work. Each spool
has its own paced verification/stream reader, so short-file bursts and all I/O
passes still require aggregate resource measurement.

Dispatch only saves the remote receipt. It neither updates local replica journal
heads/bases nor clears the outbox, and it is not called by the installed daemon.
An unrecorded remote operation may have durable intake or unowned ready candidates.
A subsequent apply resumes only its exact owned intake, including verified
candidate reuse. It refuses unowned artifacts; dispatch cannot discard or adopt
them. Abandoned-intake resolution, aggregate quotas and bounded automatic retry
scheduling remain required before production use.

Local TLS tests exercise this dispatcher for initial and delta uploads, preserved
newer edits, and recovery after reopening the index with no local payload spools.
Prepared-publication recovery and committed-receipt recovery must not call apply
again. Other cases verify no traffic for disabled, paused, excluded, over-limit,
canceled or policy-denied work, and reject missing/mismatched namespace headers.

`TestTLSPartialUploadRetryReusesOwnedCandidates` truncates a batch's second stream,
then retries and verifies both visible files while preserving the first ready
file's inode and modification time. Corrupting the retained candidate causes
refusal without visible publication. Other tests verify wrong-target-digest
refusal before sealing and rejection of a transport store outside the journal's
directory. A separate journal process-kill test exercises durable partial-file
ownership and restart. These are PC tests, not a real NAS disconnection.

### Completing a confirmed upload locally

`replica.FinishUpload` performs no network request or visible-file publication.
It requires a matching durable outbox receipt, explicit write permission,
namespace, limits and policy gate. Exclusions and pause/conflict state apply.
The staging store must be the local journal's pinned directory. Each retained
file is read once at a shared paced rate to reconstruct 64 KiB block manifests
and verify the expected whole digest, size and stable fingerprint. Live source
bytes are not read. Directory/tombstone manifests require no content reads.

The verified remote versions first become local logical journal bases through
`AdoptReplica`; this acknowledges the uploaded snapshot while preserving any
newer visible edit. A separate atomic index transaction saves all manifests and
releases the outbox. A failed entry rolls back that whole index transaction.
Newer observed generations remain dirty. After a restart between these databases,
the saved journal result is reused and manifests are rebuilt from the retained
snapshots. Budgeted replicas now reclaim only encoded wire spools after verified
local adoption and before clearing the durable receipt/outbox. Snapshots stay
retained for future diffs; restart can complete from the saved receipt without
resending content. Unlink/directory flush precedes atomic cache credit, and a
repeated cleanup cannot credit twice. [Accounting and recovery tests](cache-budget.md)
describe the scope; historical snapshots are not reclaimed yet.

Tests cover corruption refusal without acknowledgement, a restart between the
two databases, atomic rollback on a mismatched manifest and preservation of newer
edits. The snapshot/TLS upload test now also completes its own change-feed entry
using the saved base. A separate TLS test uploads a 256 KiB random file, completes
its local base, then pulls a one-byte insertion using the whole local base; a
newer visible local edit instead remains preserved as a publication conflict.
These are explicit temporary-PC operations. The daemon does not call them, and
directory inode adoption, observer-event reconciliation, production configuration,
conflict resolution, quotas and automatic scheduling remain required.

### Completing downloaded changes

`NewPuller` now requires `PullOptions.Namespace` and persists that origin in the
local journal before any pull. Reusing the same binding only reads metadata;
a changed target is refused. Existing unbound operations require explicit
reconciliation rather than a constructor silently assigning their provenance.
The caller must supply the same namespace as its authenticated transfer client.

`replica.FinishDownload` handles only the oldest pending inbox batch. It checks
the configured namespace, exact committed local operation, current local heads,
pause/conflict/exclusion state, resource bounds and matching private store. It
rebuilds manifests from verified retained candidates at a shared paced rate,
then one index transaction saves every base and advances the completion cursor.
A failed manifest or stale base rolls back the complete transaction. Restart
after publication can retry this bookkeeping without downloading again.

All existing observer records and dirty generations remain unchanged. Newly
downloaded paths can have a saved base before inotify has delivered their metadata;
completion does not invent observed fingerprints. Local comparison must later
distinguish publication-generated dirty events from actual edits. The explicit
comparison below now does that for a selected path; scheduler integration remains
unfinished, so completion is not a claim that
the current live tree has no unsynchronized edits. No live source bytes are read
or overwritten here. Directory/tombstone manifests have no content reads, and a
fully excluded batch completes by its omitted-entry count without publication.
This operation does not acknowledge a cursor to the NAS or remove retained data.

The TLS replica test now completes the inbox after actual file creation, a
one-byte diff update and explicit deletion. Local tests also cover restart
between publication and bookkeeping, preservation of a newer observed edit,
corrupt retained content, atomic rollback, directory creation/deletion and fully
excluded batches. These add no real-QNAP recovery or performance evidence.

### Comparing a selected local change

`replica.Compare` validates one dirty observed path, namespace, index/journal base,
pause/conflict/exclusion flags and metadata before content work. A regular file
gets one paced fixed-block hash pass. Matching acknowledged content clears only
that observed generation, without contacting the NAS or creating snapshot/wire
files. This resolves events generated by a previous download. A newer generation
or changed live fingerprint cannot be acknowledged as the older observation.

Changed local content requires an authenticated remote head. If its immutable
ID and full description still match the local journal base, the cached block
manifest is reused without asking the NAS to rebuild a signature. Otherwise a
bounded authenticated signature supplies remote block metadata. The result
selects push, pull, equal-content replay or conflict through the existing pure
planner. It performs no transfer or publication and does not register conflict
identities before their candidate bytes are retained. Equal remote content with
a new version ID still requires authenticated change replay for provenance.

Directory and missing-path comparisons use exact metadata only. Local absence
requires a missing leaf beneath an accessible, stable parent/root; missing or
inaccessible ancestors, symlinks, special files and child mounts fail closed.
An unacknowledged missing path is absence, not a tombstone. If both sides are
absent, the exact missing generation may be cleared without deleting a file or
creating a base. Later remote creation remains eligible for change replay.

The operation has a 45-second context, a configured file/read-rate bound and no
worker, queue or retry loop. Tests verify zero encrypted-stream bytes while
reconciling an unchanged downloaded file, cached-base signature avoidance,
independent edits classified as conflict, concurrent-generation refusal and
safe missing-leaf handling. The builder below now handles selected push decisions;
durable conflict candidates and automatic invocation remain integration work.

### Preparing selected local changes for upload

`replica.PrepareUpload` now joins the explicit comparison and outbox operations.
It accepts up to 128 selected paths and the same explicit write, namespace,
exclusion, LAN-gate and resource limits as dispatch. It caps selected regular-file
bytes before hashing. Unchanged paths can clear their exact dirty generation
without a NAS request or snapshot; pull/conflict decisions are returned without
choosing a winner. Only push decisions create candidate data. Overlapping parent
and child paths are refused as one proposal; the scheduler must order separate
directory/child operations and explicit type changes.

Before creating any candidate, a bounded `index.UploadPreparation` claims its
operation identity, unused candidate IDs, paths, expected bases, observed
fingerprints and generations. The index rejects another preparation/outbox and
serializes building, direct outbox creation and abort without queuing work.
The local journal lifetime lock and caller serialization of all replica operations
remain required. Vacancy checks cover only the four exact snapshot/wire names;
they never scan the staging directory. The preparation is not sendable.

Each file gets an immutable snapshot with its reserved identity. Its block
manifest must still match the comparison; local metadata, generations, pause and
heads are rechecked. The delta base signature is built from the verified retained
local base, so the builder does not ask the NAS to read that base again. Directory
and tombstone entries contain no snapshot or delta. Once all candidates and
wire spools are ready, a single transaction validates the original identities,
generations, bases and policy and replaces the preparation with the upload outbox.
Later dispatch/completion can retain an intervening newer source generation.

Failure retains the preparation and blocks new work. Explicit
`replica.AbortUploadPreparation` verifies the namespace, pinned store and unused
journal identities before removing only its claimed snapshot/wire names. It
refuses live builders, committed/prepared journal versions and sendable outboxes.
Removal handles a killed producer and the two-link sealing window, validates
regular owned artifacts, flushes the directory and only then forgets provenance.
Cleanup failure keeps the record for a later retry. Source files, observer records,
acknowledged bases and committed retained content are untouched.

Local TLS tests now exercise this complete preparation → dispatch → completion
path for a 256 KiB file, one-byte insertion and explicit deletion. The update's
serialized delta is under 1 KiB and its published bytes match. Tests preserve a
newer edit across upload completion. A real killed-process test stops preparation
after snapshot sealing, verifies the live journal lock, then reopens and aborts
without losing source data or queued work. Other tests cover mixed file/directory
batches, stale source generation, atomic promotion, cleanup failures and unowned
hard/symbolic links. These PC-local tests do not establish NAS runtime acceptance.
The [replica worker](daemon-worker.md) now invokes this pipeline in real observer
and TLS integration tests. There is still no production worker configuration,
aggregate storage quota or automatic retry loop.

`go test -race ./internal/transferapi` uses temporary ECDSA certificates and
loopback listeners. It uploads a base, requests its signature, uploads a one-byte
insertion as a delta, publishes through the journal, and downloads/reconstructs
the changed file from the client's base. It checks one literal byte in each
direction, whole content and retained versions. Other tests cover multiple files
in one commit, directories, recovery, connection limits, missing/unmapped
certificates, wrong client IDs, exclusions, stale epochs and size/header mismatches.
Client tests additionally cover the typed round trip with one-byte diff reuse,
pinned-certificate refusal, no traffic when policy denies access, redirects,
oversized/malformed responses, target-size rejection before output, mismatched
content, failure trailers, concurrent-operation refusal and socket cancellation.
Feed tests cover filtering, all-excluded batches, namespace/policy changes,
future/gapped cursors and TLS-to-durable-inbox receipt. Separate index tests cover
restart, bounded outstanding work, rejected premature completion and preserved
conflict state. Feed fixtures use simulated metadata publication; they do not
assert that those fixture files were transferred.
Historical TLS tests additionally publish a file, replace and delete it, fetch
its original retained bytes, reject an alternate-path lookup and verify that
the current tombstone is unchanged. Directory-base tests exercise restart and
type/generation bookkeeping; they do not publish PC directories automatically.
`TestTLSDownloadPublishesIndependentReplica` exercises the real client/server and
puller across two independent temporary PC roots. It creates a 256 KiB file,
reconstructs a one-byte insertion with one literal byte and 256 KiB local reuse,
compares both visible copies, checks received encrypted-stream bytes below the
original file size, verifies zero new traffic on committed local retry, and
applies explicit deletion. This is local TLS, not NAS execution or full wire
accounting. Additional pull tests cover late download failure, partial staging
reuse, visible-edit preservation and exact-proposal recovery.

The tests above are local HTTP/TLS correctness tests, not NAS execution, proof of
WAN isolation, wire-budget acceptance or performance measurements. See
[resource limits](performance.md). The [native helper executable/configuration](helper-service.md)
now additionally passes a real bounded QNAP TLS fixture as the non-admin account.
It accepts a validated inherited socket, applies direct-route checks, and joins
admitted handlers before closing state. `GET /v1/state` includes
`scope: disposable-test` only for validated disposable write mode; the test client
refuses writes without that scope and a fresh namespace. No production write
activation or installed helper was added. Remaining work includes the deployed daemon
client, permanent non-admin runtime deployment, trust provisioning,
event/change replay, scheduling, aggregate quotas, conflict resolution, explicit
on-demand access, route-loss tests and all outstanding M2/resource gates.

## Remote notification connection

`GET /v1/events` requires the same verified client certificate and configured
namespace as all other endpoints, with no body or query. Explicit API option
`MaxEventStreams` is 0–4 (zero disables streams); only one stream per client ID is
allowed. The helper derives `min(4, maxConnections-1)`, reserving a connection
outside the stream allowance. Stream requests do not occupy the one transfer
operation slot. They remain inside the application shutdown barrier.

The response is `application/x-ananas-events`, a sequence of existing protocol
frames, each capped at 256 JSON bytes plus its four-byte length. Fields are
`namespace`, positive `epoch` and `through` (committed sequence). The initial frame
always carries current high water; subsequent frames must advance it under the
same namespace/epoch. No paths or version IDs are exposed. Excluded-only commits
can advance the sequence, as with the existing feed's omitted-entry counts.

Durable journal completion signals listeners, and actual changes coalesce to at
most one frame per second. Idle streams send no heartbeat and do not reread the
journal. Each frame rechecks the server gate and has a five-second write deadline.
Client `WatchChanges` uses a separate connection, validates bounded frames, paces
hint processing and coalesces delivery into one pending slot. `Interrupt` and
`Close` cancel both transfer and stream contexts. A hint is never a durable cursor
or acknowledgement: reconnect sends an initial hint and the worker reads from its
persisted inbox cursor, including changes missed during disconnection.

`daemon.RunWithRemote` connects that stream to the worker, joins it on shutdown,
and stops with an error if the stream ends. There is no internal reconnect timer.
Content pause permits metadata hints while prohibiting worker content operations.
Silent broken-link detection, route-event cancellation and supervised reconnect
deployment remain open. These choices do not replace OS egress enforcement.
Local TLS/worker/helper tests and the explicit disposable QNAP stream fixture now
pass. See [helper evidence](helper-service.md). Production installation and
route-failure packet acceptance remain open.
