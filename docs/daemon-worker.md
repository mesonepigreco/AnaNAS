# Event-driven replica worker

The local integration now connects actual inotify observation to authenticated
diff transfers through `internal/replica.Worker` and `internal/daemon.RunWithNetwork`.
The production CLI uses the lifecycle runner with a **nil transfer worker**:
the installed service remains a local observer with `automaticWrites: false`.
No configuration switch bypasses the unfinished NAS, quota or egress gates.

Construction now requires a persisted replica cache budget bound to the remote
namespace. Upload preparation reserves snapshot/wire space before creating files;
download intake reserves before content requests. Outbox dispatch and adoption
require a matching upload reservation, including after restart. Explicit abort
of an untransmitted preparation credits space only after verified durable cleanup.
After verified adoption, completion now removes only the encoded spool and credits
its allowance before clearing the receipt/outbox. A restart can finalize using the
saved receipt, without reopening or resending the wire. Snapshot/history charges
remain until safe retention is implemented.
The local TLS/inotify fixture uses 256 MiB/128 entries; this is not a deployed PC
default. [Accounting and tests](cache-budget.md) describe the remaining limits.

## Lifetimes and wakeups

The tested integration now keeps observation and local controls running while
restarting transfer sessions. Each session waits for a kernel subscription and
local-only LAN check before starting transfers. Denied policy waits for events;
failed sessions retry with 1–60 second backoff, without healthy-idle polling.
Every old worker/stream/monitor is joined and pooled sockets discarded before
reuse, without observer startup scans. The installed observer remains on the
monitor-free lifecycle path.
[Network behavior, limits and tests](network-lifecycle.md) describe the remaining
fault-injection and deployment acceptance.

Completed feed progress is now acknowledged through authenticated `POST /v1/ack`
in ranges of at most 32. The index persists confirmation separately from received
and completed cursors. Pending confirmation is retried on a later external wake
or restart; a caught-up worker does nothing. Page receipt never grants ACK permission.
Local work retains its turn between full pages. [Protocol and tests](acknowledgements.md)
describe policy binding, read-only refusal and remaining retention obligations.

One goroutine runs the observer and one runs the transfer worker when supplied.
The lifecycle runner joins both before the caller closes the index, native
journal, root or staging descriptors. Observer errors and status-server failures
cancel the shared lifetime; the original failure is preserved through shutdown.
With a nil worker, the runner does not start any transfer goroutine or NAS work.

When explicitly supplied, the remote client runs one additional authenticated
notification stream with a one-slot hint channel. Committed journal changes wake
the worker through this connection without a polling timer. The stream sends an
initial high-water hint on every connection; the worker fetches from its durable
inbox cursor, so changes made while disconnected are recovered on reconnect.
In `RunWithNetwork`, remote failure joins and retries the transfer session;
the older `RunWithRemote` entry point still returns the error to its caller.
The tested kernel monitor now cancels on
reported route/interface changes. A silent failure without a kernel notification
still needs failure-detection/recovery acceptance; no heartbeat was added.

Observation exposes a new one-slot `Reconciled` hint only after a successful
metadata batch has been persisted. Its existing status stream remains separate:
per-file metadata counters refresh the UI but do not wake the transfer worker.
The persistent dirty index supplies the actual work; notification overflow loses
no path identities. A notification arriving during a pass requests at most one
additional pass from the beginning. The replica worker has no idle polling,
heartbeat, retry timer, per-file goroutine or in-memory path queue. Failed-session
retry belongs to the separate network supervisor described above. A content error
with a still-healthy stream currently waits for another work hint; classification
and bounded retry of those errors remain integration work.

Durable pause/resume changes send a separate coalesced hint. The lifecycle runner
calls `Interrupt`, canceling active operation contexts and requesting a fresh
policy check. Failed persistence and unchanged control requests emit no hint.
Kernel filesystem calls remain subject to their own timeout; cooperative
cancellation is not a claim that an uninterruptible syscall stops immediately.
Content pause permits metadata notification hints; each hint still passes the
worker's durable pause check before content work. Full network suspension requires
canceling the remote stream as part of the deployment's network policy.

## Serialized work

The worker requires explicit write authorization, a configured namespace, private
native state, a ready/non-scanning observer and a caller-provided LAN/egress gate.
It owns all replica operations while running; external code must not concurrently
invoke journal publication, preparation, dispatch, completion or cleanup.

Each pass first completes any durable upload outbox. An interrupted local-only
preparation is aborted through its existing exact-ID recovery protocol before
fresh comparison; it cannot be sent or mistaken for a committed version.
The worker then processes a saved remote inbox or requests at most one bounded
32-record page. Entries matching durable local bases can complete without a
download (including the worker's own uploads); other entries use the existing
puller and verified download completion. Paused/conflicted remote paths stop the
pass. After one page, local work gets a turn before another full page is fetched.

Local selection reads at most 128 dirty index records at a time and caps file
and batch bytes before content access. Paused/conflicted paths remain dirty and
are skipped locally. A parent create and its children use successive proposals;
the cursor does not skip an unprocessed child when deferring it. Push decisions
flow through preparation, dispatch and completion. Comparisons that need conflict
or remote-history reconciliation stop with an attention status and preserve work.
No conflict winner is selected or conflict-content retention claimed implicitly.

Feed requests occur at pass startup, after completing an upload, after draining
a saved inbox, or when another full remote page may exist. Paging through local
dirty metadata alone does not repeatedly fetch an empty remote feed. Once hints
and durable work drain, the worker waits. An error waits for another external
notification; no internal error retry loop runs.

## Evidence and unfinished deployment work

`go test -race ./internal/transferapi -run
TestTLSWorkerObservesUploadsPullsAndPauses` uses real inotify events, the daemon
lifecycle runner, two native temporary roots and loopback TLS. It verifies:

- pause before file creation prevents upload, and durable resume starts it;
- parent-before-child upload after both arrive in one observation batch;
- a 256 KiB random file and one-byte insertion produce matching remote content,
  with encrypted sent-byte growth smaller than the base file;
- a controlled remote commit downloads automatically through the authenticated
  notification stream, without a test-supplied wake-up;
- local deletion becomes an explicit acknowledged remote tombstone;
- after settling, 150 ms of idle observation adds no encrypted stream bytes or
  local metadata reads.

Separate stream tests verify initial/reconnect hints, replay of offline commits,
one stream per authenticated client, concurrent transfer progress, malformed
frame refusal and no added encrypted bytes during a 1.1-second quiet window.
Helper shutdown tests join a quiet stream before reopening native journal state.
The stream now also passes an explicit disposable QNAP fixture; the full worker
is still not installed or verified against the production share. Separate tests cancel a blocked
request on pause, refuse a second worker loop, verify no retry after a failed feed
request, and check lifecycle event routing and resource joins. These are local
correctness/short-idle checks, not ten-minute CPU/RSS or real-NAS acceptance.

Still required before enabling production: native helper deployment and trust
configuration and real-NAS notification validation; observation of
out-of-band NAS edits; OS egress and route-change enforcement; physical storage
limits and committed-history retention; real-NAS acknowledgement validation and
history coalescing; durable conflict candidates and resolution controls; full
directory adoption/deletion and type-change reconciliation; sustained resource
and real failure/recovery acceptance. A directory deletion with indexed children
currently requests reconciliation before attempting publication. Historical replay
whose expected base differs from local history also fails closed. The worker's
phase `idle` means it is waiting, not proof that these open gates are complete.
