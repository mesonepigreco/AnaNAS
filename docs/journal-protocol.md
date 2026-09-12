# Cooperative journal and publication contract

`internal/journal` is a native-filesystem metadata coordinator for a future NAS
helper/service. It is not wired into the deployed daemon. A native file publisher
now has local integration tests and a scoped native QNAP probe. An authenticated
transfer API is exercised in local TLS tests; deployment, egress enforcement,
directory scheduling/adoption, conflict resolution and full NAS acceptance remain open.
`automaticWrites` remains false. See [publication implementation](publication.md).

## Ownership and ordering

The database lives beside staged objects in an existing private directory
outside both sync roots. The directory and fixed `journal.db` file are opened
without following symlinks; network/FUSE stores and unsafe permissions/link
counts are refused. bbolt exclusively locks the native file for the entire
coordinator lifetime. Another process gets a bounded open failure rather than
stealing a lock by wall-clock age. Each successful open persists a new uint64
epoch. Requests carrying a previous epoch are rejected.

The journal belongs to a configured opaque namespace. It assigns monotonically
increasing logical sequences independently of client clocks. A namespace is
not authentication; the deployed transport must bind it and each client ID to
the authenticated connection and approved share.

All cooperating publication must execute through this owner. The native lock
does not constrain File Station, SMB clients or other programs modifying the
visible tree directly. The scoped native QNAP probe now verifies journal locking
and prepared-publication recovery; independent-client/cross-protocol fencing and
actual NAS failure acceptance remain open. See [native evidence](native-probe.md).

## Commit and recovery

A proposal contains an operation ID, client ID and 1–128 entries. Each entry
contains a relative path, expected head ID and a new immutable version
description (ID, size, whole-content digest, directory flag, or explicit tombstone).
Directories have no file payload/digest and cannot also be tombstones. A delete's
expected committed base supplies its prior type. Encoded
records are limited to 128 KiB. Traversal, `.nas-sync` paths, duplicate/overlapping
paths, duplicate version IDs and malformed versions are rejected before preparing.
The integrating scheduler must apply configured exclusions before content or
journal work. Empty expected head means no recorded version, which is distinct
from an acknowledged tombstone.

Under serialization, the coordinator checks every expected head and reserves
one prepared batch with the next sequence. It persists that record before
invoking `Publisher.Publish`. No database transaction stays open during the
publisher callback. On success, one database transaction installs the version
descriptions, heads, committed record and journal sequence, and clears pending
work. Retrying an identical committed operation returns its original result
without publishing again. Reusing an operation for other input is rejected.

A failed or canceled publisher leaves prepared work and blocks new commits.
After restart, `Recover` replays it under the new owner; the record retains its
original epoch as provenance. An interrupted publisher must therefore be
idempotent. The current API intentionally does not discard prepared transactions
on timeout or provide an unsafe abort that could forget partially applied files.
Handling a permanently conflicting prepared batch needs durable preservation
and explicit resolution in the future publisher/coordinator integration.

Readers receive committed heads plus a pending indicator. Visible files may
already reflect part of pending work; consumers must defer visible-path access
or use retained committed immutable objects. This is not multi-file atomic
visibility to external applications.

Committed version metadata now stores its original relative path alongside the
immutable description in the same transaction as heads/journal commit.
`VersionAt(path, id)` permits bounded exact historical lookup only when that
binding matches. It neither scans history nor touches visible content. Old records
without a stored binding remain readable through their current head; historical
access fails until explicit migration/reconciliation establishes provenance.
No automatic whole-journal migration runs at startup. Tests verify persistence,
cross-path refusal and rejection of prepared-but-uncommitted versions.

## Local upload preparation ownership

Before snapshot creation, the local index can durably reserve one bounded upload
preparation containing candidate IDs and observed source generations. Its builder
checks that the IDs are unused in the local journal and that snapshot/wire names
are vacant. `CheckUnusedVersions` refuses any pending publication or intake and
any committed candidate ID. Its result requires caller serialization of all
replica operations while holding the coordinator lifetime lock; it is not a
cross-goroutine reservation by itself.

Successful preparation atomically becomes the index outbox. An interrupted
preparation is not sendable and may be explicitly aborted after unused-ID and
namespace checks, exact private-name cleanup and directory flush. This abort
does not apply to NAS intake, a sendable outbox or prepared/committed publication.
See [upload preparation](transfer-api.md#preparing-selected-local-changes-for-upload)
for bounds, tests and the remaining aggregate-quota/scheduler obligations.

## Upload intake ownership

`StageAndCommit` reserves one complete proposal before receiving content, capped
by the existing 128-entry/128 KiB bounds. It checks expected heads and unused
version IDs, and requires both exact candidate names (`.partial` and `.ready`)
to be absent before a new reservation. It does not enumerate a directory or
adopt unknown pre-existing files. Transport staging must use the same pinned
native directory as the journal; constructor checks compare device/inode identity.

The receive callback runs outside the bbolt transaction but under the owner's
mutex and lifetime database lock. A failed intake persists, blocks other new
commits and can resume only with the complete original proposal and current
epoch. Under that ownership, a retry can discard its abandoned partials and
verify/reuse its retained ready files. Neither file age nor a ready suffix grants
ownership. The callback cannot publish visible files or reenter mutations.

After successful receipt, the same database transaction replaces intake with
prepared publication and reserves the journal sequence. Intake itself does not
appear in `Operation`, change pages or the visible-publication pending flag;
it has not changed visible files. A null operation therefore does not imply no
intake exists. Committed retries return their saved result without receiving
content again. There is no timeout-based intake expiry or arbitrary abort.
An abandoned or permanently invalid intake can block new commits until explicit
resolution is implemented. Aggregate storage quotas remain required.

Local tests kill a process while it has a real partial file, verify that another
process cannot acquire the live lock, reopen after the kill and resume only the
owned proposal. Additional tests verify persistence across reopen, stale epoch
refusal, cross-client identity refusal, unowned artifact preservation and the
atomic intake-to-publication transition. Publisher callbacks in these journal
tests are simulated; separate TLS tests exercise actual native publication.

## Acknowledged upload bases on a PC replica

`AdoptReplica` records an authenticated remote commit as a PC's logical base,
without publishing or inspecting its live files. A file may have changed again
while its captured version uploaded; that newer edit must remain untouched.
This operation is separate from `Publisher.Publish` and must not be used by a
NAS coordinator to accept unverified uploads.

The caller supplies the authenticated namespace/commit and a verifier for retained
candidates, exclusions and limits. Under the coordinator lock, the method checks
heads, unused versions, pending work and the persistent remote-origin binding.
Verification runs outside a database transaction. It then atomically installs
local heads, path-bound versions, the operation and a local journal sequence.
There is no intermediate prepared visible-publication record. The source's
sequence and epoch do not replace local ordering. Identical committed retry
returns the existing result, and a different remote-origin binding is refused.
`BindReplicaOrigin` now lets puller construction pin the configured origin before
work. Matching existing bindings are read-only checks. Changed targets and
previously unbound operation history require explicit reconciliation. Production
configuration must use the authenticated client's namespace; an independently
supplied proposal is not authentication.

`replica.FinishUpload` uses this method only after a matching upload receipt is
durable in the local index. It verifies retained file content and rebuilds block
manifests, then atomically saves the manifests and clears the outbox in the index.
A restart between the journal and index commits rebuilds from the same snapshots
and completes bookkeeping without touching newer live bytes. Snapshots remain
retained; no cleanup is implied. Logical directory bases do not provide native
inode receipts for later safe directory publication/deletion. Adopting those
identities, automatic observer-event reconciliation and conflict resolution remain unfinished.

Download completion now checks an exact committed local operation and current
heads before rebuilding retained manifests. It atomically saves bases and the
received-batch completion cursor in the observer index, preserving every local
record and dirty generation. An inbox receipt alone cannot complete content,
and a newer local edit remains pending. Fully excluded batches need no local
publication. This does not advance a NAS cursor or run automatic reconciliation.

The explicit local comparison now checks matching index/journal base identities
before acknowledging unchanged dirty content. An authenticated remote head with
the same immutable ID and complete description can reuse that verified base's
block manifest. Changed identities still need remote metadata/change replay;
the comparison cannot adopt a new journal head or claim retained conflict bytes.

## Required publisher behavior

Before enabling writes, the real publisher must:

1. Verify retained immutable candidates, expected visible generations and scope.
2. Preserve both candidates on external edits or conflicts, including edit/delete.
3. Apply puts/deletes idempotently with same-filesystem staging, durable file and
   directory flushes, and recovery evidence at each publication boundary.
4. Retain the previous and next content through recovery and unresolved conflicts.
5. Reject symlinks, special files and unexpected mounts; enforce cancellation,
   resource/space limits and LAN capability checks around actual work.

Returning nil from the callback asserts those obligations were completed. The
coordinator cannot verify them from metadata. Journal unit tests deliberately
simulate the callback and do not prove these content guarantees. Separate
`internal/publish` integration tests exercise actual native file operations;
their scope and remaining gaps are documented in `publication.md`.

## Client cursors

`Changes` returns at most 32 contiguous committed batches, up to 4 MiB of encoded
records plus decoded overhead. Missing journal records fail closed; they are
never treated as an up-to-date view. Each client's durable cursor advances one
sequence at a time with expected-cursor checking and idempotent retry.

Acknowledgement is permitted only after every entry is durably applied or
durably recorded as a conflict, exclusion or deferred job. The client and future
authenticated transport must enforce that semantic precondition. Reading a page
does not acknowledge it. No journal/history pruning or client eviction exists
yet; retention and total disk quotas remain required integration work.

The experimental transfer API now exposes filtered, contiguous change pages
bound to this namespace and both exclusion policies. Excluded entries carry a
count without their paths/version descriptions. A new local index inbox stores
at most one page durably before advancing a separate receive cursor. Completion
requires matching applied-base or conflict bookkeeping; receipt alone is not
synchronization. The tested worker now sends authenticated bounded acknowledgements
after durable completion, with a separate confirmed PC cursor. Neither that worker
nor its acknowledgement path is installed in production. See
[acknowledgement protocol](acknowledgements.md) and [transfer API](transfer-api.md)
for limits and replay gaps.

## Local evidence and limits

The native receiver can now persist a publication budget and charge admission in
the intake/preparation transaction. The assembled helper requires it. It refuses
unaccounted existing state, conservatively retains charges through restart and
commit, and adds no polling or payload scans. Replica mode now charges snapshots,
wire spools and downloaded publications together, verifies reservations before
adoption and credits only verified untransmitted-preparation abort. Committed
history reclamation remains unfinished. Verified upload completion now reclaims
only encoded spools while preserving snapshots and the durable receipt/outbox
until finalization; unlink/fsync precedes atomic wire credit and restart is idempotent.
[Cache accounting and added tests](cache-budget.md) give the limits.

`go test -race ./internal/journal ./internal/stage` covers idempotence, atomic
batch validation, conflicting concurrent clients, immutable identities,
explicit tombstones, per-client cursor persistence, gaps, bounds, cancellation,
symlink rejection, owner-epoch changes and recovery after killing a publisher
process. A competing independent process cannot acquire the live journal, then
can acquire it after the owner is killed. Tests use temporary PC directories
and simulated publication; they are not power-loss, QNAP, cross-protocol,
partition/fencing or network-byte acceptance tests.
