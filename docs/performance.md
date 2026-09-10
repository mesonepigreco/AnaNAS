# Local performance measurements

The 2026-09-10 post-reboot menu correction removes one redundant initial
`LayoutUpdated`; it adds no timer, polling loop or background worker. Contextual
errors reuse the existing status row, and desktop notifications occur only after
a user action fails. The seven panel unit tests retain the existing assertion of
zero desktop signals for 1,000 metadata-only updates. No new compositor resource
measurement is claimed.

The panel label correction replaces repeated structural menu notifications with
property deltas for changed items. Unit tests retain zero emitted signals for
1000 metadata-only changes, and the installed DBus pause/resume check verifies
cached label updates. It adds no polling/timers; existing CPU/memory limits are
unchanged. No new compositor CPU measurement is claimed for this correction.

The later observed-file-deletion update refreshes missing generations during
already requested completed scans, including startup reconciliation. This adds
bounded index invalidation for known absent paths, without adding filesystem
reads, healthy-idle work or polling. Missing records may therefore be compared
again after a root reconciliation. Confirmed native leaf deletion performs
metadata checks plus parent/receipt flushes, with no content transfer. The live
32 KiB deletion/re-creation fixture is recorded in `live-trial.md`; earlier idle
folder samples below predate this update. The later
[file-deletion idle sample](evidence/live-file-deletion-idle-2026-09-09.json)
recorded zero added PC/panel/NAS process ticks and zero encrypted sync bytes over
15 seconds. GNOME Shell used 2.02 CPU seconds (13.466% of one core), without
establishing attribution. This remains short quiet-sample evidence.

Automatic startup now uses a oneshot mount helper and NetworkManager event
dispatcher. The helper exits after a successful mount; there is no healthy-idle
mount process or polling timer. Failed attempts retry every 60 seconds only while
the direct-LAN condition permits an attempt. The installed service finished with
`Result=success` and `ExecMainStatus=0`; see `docs/startup.md` for scope and limits.

## Folder-update idle sample — 2026-09-09

After installing directory capture and verifying the native NAS nested-folder
fixture, `PYTHONDONTWRITEBYTECODE=1 python3 scripts/measure_desktop.py --seconds 15`
recorded zero measurable CPU ticks for the PC daemon and panel. Before/after
reads of exactly `/proc/18391/stat` and `/proc/18403/stat` through the authenticated
setup session also recorded zero launcher/helper ticks. `/api/status` encrypted
upload/download totals each added zero bytes. The helper's `/proc/18403/status`
reported UID 1000/GID 100 and 11,272 KiB RSS. GNOME Shell consumed 0.16 CPU seconds,
or 1.067% of one core, without establishing compositor causation.

This small quiet sample includes the earlier 256 KiB file and the new 96 KiB
nested-file/empty-directory fixture. It excludes SSH verification traffic and
does not establish sustained-load behavior. Directory capture checks identity
and seals a small receipt without listing children; metadata-only observation
and the existing rate/watch/memory limits remain unchanged.
[Raw updated evidence](evidence/live-directory-idle-2026-09-09.json).

## Installed LAN trial idle sample — 2026-09-09

After real transfers and explicit PC/helper restarts, the running trial with one
tracked 256 KiB file was sampled for 15 seconds. Command on the PC:
`PYTHONDONTWRITEBYTECODE=1 python3 scripts/measure_desktop.py --seconds 15`.
The orchestration read `/api/status` before/after and, through the temporary setup
session, exactly `/proc/18108/stat`, `/proc/18122/stat` and the helper's
`Uid`, `Gid`, `VmRSS` fields. The NAS lacked `getconf`, so its CPU is reported in
raw `/proc` tick deltas rather than an assumed clock frequency.

PC daemon and indicator each consumed 0 measurable CPU ticks; the NAS launcher
and helper each added 0 ticks. Daemon encrypted sent/received totals each added
0 bytes. The helper ran as UID 1000/GID 100 and used 9008 KiB RSS. GNOME Shell
used 0.92 CPU seconds (6.133% of one core) during the desktop sample; this does not
establish which applications caused compositor activity. This is a short quiet
sample, not sustained-load, many-file or reboot acceptance. SSH measurement
traffic is excluded from daemon counters. [Raw evidence](evidence/live-idle-2026-09-09.json).

Native observation now defaults to 50 metadata operations/s, 8192 watches and
1.5 s quiet/5 s maximum event coalescing. Its separate capture worker reads only
selected dirty regular files, paced at 2 MiB/s in the installed profile. Equal-size
changed files can require one comparison and one capture pass; capture hashes
also feed immutable staging verification. No healthy-idle polling was added.
The PC service still enforces 10% of one CPU and 128/256 MiB memory thresholds.
The NAS helper uses one Go scheduler thread, nice 15 and a 64 MiB Go soft memory
limit; that soft limit is not a hard process RSS cap.

The one-byte update in a 256 KiB file used 71,437 encrypted bytes uploaded and
67,611 downloaded, including TLS/protocol traffic. This is block reuse, not
one-byte wire granularity. [Commands, final hashes and limits](live-trial.md).

The measurements below describe earlier local/disposable checkpoints.

Measured 2026-09-06, Linux/amd64, Go 1.27.1, Intel Core Ultra 7 270K Plus.
These are local-development results, not NAS transfer acceptance. Test data and
state were disposable; no NAS endpoint was used.

## Local observer

```sh
go build -o /tmp/nas-sync-observer ./cmd/nas-sync
python3 scripts/measure_local.py --binary /tmp/nas-sync-observer --idle-seconds 10 --files 1000
```

| Measurement | Observed |
|---|---:|
| Tracked regular files | 1,000 |
| Idle window | 10.000 s |
| Idle CPU time / CPU percentage | No measurable ticks / 0.0% of one core at `/proc` resolution |
| Idle resident memory | 9,019,392 bytes (~8.6 MiB) |
| Idle disk bytes read/written | 0 / 0 |
| 500 writes to one path, including 2 s settling | 0.01 s process CPU; ~9.0 MiB RSS |

The harness accelerates initial metadata enumeration with `scanOpsPerSecond=10000`;
shipping defaults pace at 50 operations/s. No CPU quota was applied to this run.
Disk counters omit reads served by the page cache. The idle result is not evidence
of literally zero CPU consumption or of passing the planned ten-minute churn budget.

## 100,000-file check

The same harness with `--files 100000 --idle-seconds 10` recorded:

- 61,538,304 bytes RSS (~58.7 MiB), below the proposed 128 MiB idle budget.
- No measurable idle CPU ticks, and zero disk bytes read/written during the window.
- About 0.01 seconds process CPU for a 500-write burst, with RSS unchanged.

This fixture used one directory containing 100,000 regular files. It does **not**
validate kernel memory for 100,000 directory watches or the ten-minute churn budget.
Initial and restart scans remain paced; the default 50 operations/s intentionally
makes a tree this large take much longer to enumerate than the harness profile.

## Hashing

16 MiB in-memory input, fixed 64 KiB blocks, `go test -bench . -benchmem`:

| API | Throughput | Allocated bytes/op | Allocations/op |
|---|---:|---:|---:|
| `Stream` | ~5,189 MB/s | 65,632 | 3 |
| `BlockDigests` | ~5,217 MB/s | 81,984 | 12 |

The first implementation allocated a reader wrapper per block (259 allocations
for this input); moving it outside the loop removed that cost. Streaming retains
one block buffer. Collecting digest APIs retain O(number of blocks) metadata.
These unpaced microbenchmarks measure library throughput, not daemon CPU budgets.
The local observer does not invoke the hash library.

## Network check

```sh
python3 scripts/check_no_network.py --binary /tmp/nas-sync-observer
```

A child-process `strace -f -e trace=network` recorded **zero network syscalls**
during local daemon startup and two seconds idle. This includes no outbound DNS
or SSH requests. The trace requires permission to trace a child process. It covers
only the current local observer; it is not a substitute for WAN packet capture
and route-change tests once NAS transports exist.

## Remaining acceptance work

The 2026-09-08 sync-state additions maintain a separate persistent dirty-path
index and migrate older local indexes once. Earlier observer measurements above
predate that extra index and must be repeated before applying them to deployment.
The new manifest codec caps each file at 131,072 blocks (8 GiB with 64 KiB blocks),
roughly 5 MiB of decoded block metadata. Only the active candidate is loaded;
there is no in-memory manifest map for the entire tree.

The separate local content API paces reads with one block of initial burst per
hash operation, checks cancellation between reads and verifies candidate
fingerprints before/after hashing. Range upload callers reuse one open candidate
and buffer, and must apply the transfer rate and CPU budgets separately.
This API is not scheduled by observer mode and does not yet establish end-to-end
throughput, churn fairness, NAS CPU use or byte savings.

### Sync-state regression check (2026-09-08)

After the dirty-index, client-state and pause-control changes:

```sh
GOCACHE=/tmp/nas-sync-go-cache GOMODCACHE=/tmp/nas-sync-go-mod-cache \
  go build -o /tmp/nas-sync-development ./cmd/nas-sync
python3 scripts/measure_local.py --binary /tmp/nas-sync-development --idle-seconds 10 --files 1000
```

The 1,000-file fixture recorded 14,462,976 bytes RSS (~13.8 MiB), no measurable
idle CPU ticks and zero disk read/write bytes over 10.000 s. The 500-write burst
plus two seconds settling consumed 0.01 s process CPU and reached 14,659,584
bytes RSS (~14.0 MiB). This used the harness's accelerated initial scan, no web
client, no content hashing or transfer, and no CPU quota. Page-cache reads are
not counted. The short idle result does not establish a ten-minute budget.

The loopback dashboard was separately exercised in Chrome against a disposable
local fixture: pause/resume changed durable user intent and both states retained
`automaticWrites: false`. This is a functional check, not a UI resource benchmark.

`python3 scripts/check_no_network.py --binary /tmp/nas-sync-development` also
recorded zero network syscalls during startup and two seconds idle with `webPort=0`.
This traces a child observer only; it does not establish transport egress safety.

Repeat the 100,000-path memory test and run ten-minute idle/churn workloads on
the target machine. Test scan storms, large directory moves and slow/full storage. Use the
systemd example to enforce a measured CPU ceiling if desired. NAS-side CPU, wire
bytes (both directions), offload, SFTP latency and route loss require the real
NAS/deployment described in [e2e-qnap.md](e2e-qnap.md).

### Installed daemon plus desktop panel (2026-09-08)

After deployment, obtain each PID with `systemctl --user show UNIT -p MainPID
--value`, then sample `/proc/PID/stat` and `/proc/PID/io` before and after a
10-second sleep. The check allowed three seconds after panel startup before the
first sample. The empty `~/NASdir` fixture had one directory watch; the dashboard
tab was open, the panel was connected, and transfers were disabled.

| Process | RSS before/after | CPU tick delta | Disk read/write byte delta |
|---|---:|---:|---:|
| Daemon | 12,087,296 bytes (~11.5 MiB) | 0 | 0 / 0 |
| Python/GIO panel | 35,819,520 bytes (~34.2 MiB) | 0 | 0 / 0 |

The interval was 10.0003 seconds. These are RSS and `/proc` counters, not cgroup
memory accounting, kernel watch memory or page-cache I/O. At the time of this
measurement the dashboard made five-second status requests; the panel received pushed
changes with no idle heartbeat. No ten-minute, large-tree or transfer-resource
claim follows from this small fixture. Unit CPU quotas are 10% for the daemon and
5% for the panel, independently; memory high/max are 128/256 MiB and 64/96 MiB.

### GNOME Shell CPU investigation (2026-09-08)

The user reported high GNOME Shell CPU. Fifteen-second samples used differences
in `/proc/PID/stat` user+system ticks, divided by `SC_CLK_TCK` and elapsed time;
100% means one logical CPU. A temporary process-counter script also captured
other process names and CPU counters, but did not inspect their files or input.
The focused reproducible command is now:

```sh
python3 scripts/measure_desktop.py --seconds 15
```

It reads only the current user's shell and the two nas-sync user-service PIDs.
The isolation phases used `systemctl --user stop/start` for the stated components,
and closed only the nas-sync Chrome dashboard for the final checks:

| Phase (before final UI optimizations) | GNOME Shell CPU, one core |
|---|---:|
| Daemon + panel active, dashboard open | 90.31% |
| Panel stopped, daemon and dashboard active | 83.12% |
| Both services stopped, dashboard open | 80.92% |
| Both services restored, dashboard open | 81.59% |
| Both services stopped, dashboard closed | 48.82% |
| Both services restored, dashboard closed | 82.67% |
| Panel stopped again, daemon active, dashboard closed | 86.47% |

The initial baseline measured 0.00% for both application processes at `/proc`
tick resolution. The restore sample included panel startup (0.53% averaged over
15 seconds); the observer remained at 0.00%. Desktop activity and other workloads
were not held constant, so differences between rows are not a precise attribution
of incremental UI cost. High shell CPU persisted with the app components absent;
this does not identify the shell's cause or prove that the app never adds CPU.
Recent user-session GNOME Shell logs had no entries during the inspected window.

Two avoidable sources of UI work were removed: the panel now emits menu/icon/title
changes only when those displayed values actually change, and the dashboard uses
the bounded event stream instead of five-second polling. Hidden tabs disconnect;
unchanged DOM text is not rewritten. A regression test sends 1,000 metadata-only
status changes and asserts zero desktop update signals, then verifies traffic and
pause changes still update the appropriate UI. Chrome pause/resume over the new
stream passed. A later 15-second sample recorded 0.00% for both app processes and
96.67% for GNOME Shell under the still-variable desktop workload. This measures
the local observer/UI only, not content-transfer CPU or ten-minute acceptance.

The subsequent anaNAS branding adds two small bundled static SVGs. The panel's
pineapple remains fixed and only its status overlay changes; repeated identical
metadata updates still emit no desktop redraw signals. No animation, new timer,
dependency or external asset request was added. Existing measurements above
predate the branding and are not a fresh desktop-resource acceptance run.

### Rolling delta microbenchmark (2026-09-09)

On this Linux/amd64 PC, Go 1.27.1, Intel Core Ultra 7 270K Plus:

```sh
GOCACHE=/tmp/nas-sync-go-cache GOMODCACHE=/tmp/nas-sync-go-mod-cache \
  go test ./internal/delta -run '^$' -bench BenchmarkEncode -benchtime=200ms -count=1
```

| In-memory 4 MiB target, 64 KiB blocks | Encode time | Throughput | Allocated bytes/op |
|---|---:|---:|---:|
| Identical | 5.00 ms | 838 MB/s | 204,697 |
| One-byte prefix insertion | 5.11 ms | 822 MB/s | 204,697 |
| All new (4 MiB + 1 byte) | 40.48 ms | 104 MB/s | 204,698 |

All three reported 16 allocations/op. Signature construction is outside timing;
input is in memory, output is discarded, no pacing/quota is applied, and the NAS
is not contacted. These are short microbenchmarks, not sustained CPU/RSS, disk or
NAS results. The all-new rolling scan costs more CPU and must remain under the
future scheduler's pacing and deployed CPU quota. Nothing invokes it at idle.

`TestProductionBlockTraffic` reconstructs a 4 MiB file with a one-byte prefix
insertion using 1 literal byte and 4 MiB of receiver-local reuse. The serialized
delta is 3,011 bytes; a separately needed base signature is another 2,360 bytes.
This excludes transport framing, authentication, acknowledgements, journal and
retries. It is not an SMB/wire measurement or proof of NAS-local reconstruction.

`go test -race ./internal/delta ./internal/stage` passed. Tests cover aligned
edits, arbitrary-offset insert/delete, short tails, duplicates, bad/truncated
streams, changed base bytes, bounds, cancellation, stalled I/O, private path
checks, killed-receiver partial recovery and duplicate-operation refusal.
Five-second fuzz runs for round trips and malformed streams passed (569 and
739,065 executions respectively); short fuzz runs are limited evidence.

### Cooperative journal resource boundaries (2026-09-09)

The new journal accepts at most 128 entries and less than 128 KiB of encoded
proposal metadata per batch; returned committed pages are capped at 32 batches.
It serializes publication, holds no bbolt transaction during content work, and
durably records preparation and commit as two separate database updates. This
amortizes those updates over a batch; it does not eliminate content/namespace
flushes required by a real publisher. There is no idle timer, directory scan,
network operation or content hash in this package. The lifetime database lock
attempt has a 150 ms timeout; bounded retries occur only while opening it.

The native staging directory is pinned and the companion journal file is opened
without symlink following. Existing ready content cannot be overwritten through
the journal opener. Journal history is retained on disk; safe pruning and an
aggregate storage quota remain integration requirements. The 32-batch cap bounds
encoded page data to 4 MiB; decoded Go values have additional allocation overhead.
These are code limits, not measured daemon/NAS RSS or sustained CPU guarantees.

Validation: `go test -race ./internal/journal ./internal/stage` includes concurrent
clients, exclusive ownership across processes, process termination during a
prepared transaction, restart/recovery, cursors and metadata bounds. Content
publication is simulated; no NAS or ten-minute acceptance workload is involved.

### Native file publication boundaries (2026-09-09)

`internal/publish` now connects prepared journal records to real native file
operations in local tests. Content verification/copy uses a 64 KiB buffer,
checks cancellation between reads and waits, and accepts an explicit read rate
from 1 KiB/s to 1 GiB/s. Each pass allows one initial buffer; separate file passes
have separate pacing. There is no idle polling or per-file goroutine. Traversal
opens only the selected path's parents and checks local descriptor mount IDs.

Replacing a file currently verifies the visible base, copies/verifies the ready
candidate into a separate native file, then verifies the displaced file after
exchange. Recovery verifies the visible and retained candidates again. These
are NAS-local reads/writes only when the publisher runs on the NAS; the package
refuses network/FUSE roots. This minimizes network reuse traffic but still costs
local disk work and hashing. Reflink/offload optimization, source-generation
cache integration and sustained NAS CPU/disk measurements remain open.

Ready versions and displaced inodes are retained; replacements temporarily need
both immutable data and a separate writable publication copy. The current
package does not enforce the configured aggregate cache limit or prune history.
Those controls must be integrated before automatic writes. No memory, storage
or CPU acceptance claim follows from the small local tests.

`go test -race ./internal/publish ./internal/journal ./internal/stage` passed,
including actual namespace changes, retained concurrent-writer content, process
termination after rename, idempotent recovery and a partially published batch.
The test rate is 1 GiB/s, with small PC-local fixtures; it is not a deployed
transfer rate, NAS benchmark or ten-minute workload.

### Standalone native probe, local run (2026-09-09)

The amd64 `cmd/ananas-native-probe` executable passed its read-only mode and
`-write` mode against a newly created PC temporary `nas-sync-capability-test`
directory. The write run reported 2.444 seconds, 524,288 reused bytes, 1 literal
byte, a 491-byte delta stream, a 344-byte base signature and successful cleanup.
The executable sets `GOMAXPROCS=1` (runtime/I/O threads may still exist), nice 15, a 64 MiB
soft Go memory target and a process CPU-time limit of 15/20 seconds. Its overall
context expires after 45 seconds; that cannot interrupt a blocked kernel I/O
call. A soft Go memory target is not a hard RSS limit.

Publisher passes are paced at 2 MiB/s. Fixture generation, bounded delta work
and final readback checks are unpaced for this fixed 512 KiB test. Estimated
fixture storage is 8 MiB, with a preflight requirement for 32 MiB free space.
The SSH runner separately reports the setup executable's upload size; it is not
part of the delta payload measurement. No NAS CPU, actual wire bytes, sustained
transfer resource usage or hardware crash durability was measured in this run.

Validation commands: `go test -race ./internal/nativeprobe`, the native CLI
without/with `-write`, and `python3 -m unittest discover -s scripts -p
'test_run_native_probe.py'`. Tests verify default read-only behavior, scoped
cleanup preserving an existing sentinel, path/symlink refusal, and no upload
without the write flag. An SSH observation timeout preserves its exact upload
directory for investigation instead of treating the remote process as stopped.

### Directory publication resource scope (2026-09-09)

Directory versions carry a flag, no payload, and one 16-byte private native
identity receipt. Preparing an empty directory reads at most one entry from
that newly created private directory, then performs explicit flushes and an
exclusive rename. Deletion uses the kernel's atomic empty-directory check;
it does not enumerate or hash the visible children. There is no recursion,
idle timer, new worker or external asset request. Parent/child transaction
ordering remains a scheduler requirement; receipts/history still need the
aggregate storage quota and retention policy.

`go test -race ./internal/publish ./internal/journal ./internal/nativeprobe`
passes local directory and nested-file tests, recovery, exclusions and racing
child protection. The probe now includes five commits and parent directory
creation/removal; the 2.444-second CLI measurement above predates that extension.
These tests do not establish sustained NAS CPU, durability or complete autosync.

### Experimental transfer API resource boundaries (2026-09-09)

The HTTP handler accepts one active operation and immediately returns busy for
another; it does not create a producer queue or per-file worker. The listener
wrapper caps accepted connections, including idle connections and TLS handshakes
(deployment/test selection: four; supported bound: one to eight), and disables
TCP keepalive probes. HTTP header timeout is five seconds, idle connections expire
after 15 seconds, and application headers are capped at 16 KiB. Active operations
have a 45-second context and socket deadlines. HTTP/2 is disabled/refused.

Each operation shares a read budget across incoming wire and native content reads.
Read calls are capped at 64 KiB; waits are coalesced after 64 KiB to avoid a timer
per tiny copy frame. This permits less than 128 KiB of unpaced initial work.
The explicit rate must be 1 KiB/s–1 GiB/s. Responses/signature serialization are
bounded but have no independent outbound-byte rate limiter. Native publication
has its own paced passes. There is no heartbeat, periodic remote scan or update
check. Idle connection expiry closes a socket once; it does not poll storage.

The caller must set maximum file and reconstructed-batch bytes (each at most
8 GiB), and delta headers must agree with those approved sizes before staging.
128 entries and 128 KiB framed metadata remain upper bounds. These per-operation
bounds do not enforce aggregate retained storage: orphan staging recovery,
retention and a durable aggregate quota remain mandatory before production writes.
Large/slow operations can exceed the current 45-second protocol deadline; resumable
scheduling and operation-budget selection are not integrated yet.

Local TLS race tests pass for upload/download, multi-file publication, certificate
rejection, identities/exclusions/size gates, connection saturation, read cancellation,
idempotent retry and recovery. The fixtures use 256 KiB content and a 1 GiB/s test
rate. These are correctness checks, not NAS CPU, full wire-accounting, memory or
ten-minute workload measurements. Build and vet cover the new packages.

### Transfer client validation (2026-09-09)

`go test -race ./internal/transferapi` passes with the typed client using real
loopback TLS sockets and 256 KiB temporary PC fixtures. Source/interface binding
to `127.0.0.1`/`lo` succeeded in the host namespace. Tests reconstruct a one-byte
prefix insertion with one literal byte, reject oversized target headers before
writing output, and cancel an active socket. This is not NAS execution or a
CPU/memory benchmark. No automatic transfer worker was enabled.

The client permits one active operation and an optional separate notification
stream (two connections maximum), bounds response metadata and coalesces traffic
notifications into one pending slot. It adds no
idle polling or per-file worker. Encrypted stream counters exclude packet headers
and kernel retransmissions. Download/source pacing and durable aggregate quotas
remain caller obligations before daemon integration; local correctness tests do
not establish sustained lightweight transfer behavior.

### Change feed and local inbox bounds (2026-09-09)

The explicit feed request returns at most 32 journal batches of at most 128 KiB
each, plus a 4 KiB page-envelope allowance. Filtering and validation use bounded
metadata; they do not list roots or read content. Exclusion requests are capped
at 128 patterns and 8 KiB combined pattern text. There is no idle fetch timer.
JSON encoding/decoding and validation use additional bounded allocations beyond
wire size; no new peak-RSS or CPU benchmark has been measured.

The local bbolt inbox stores one outstanding page and returns one batch at a
time. Receiving commits the page plus its receive cursor atomically; completion
removes one batch after verifying durable base/conflict bookkeeping. Receipt
does not clear local dirty work. Repeated identical receipt avoids rewriting
data. bbolt may retain freed pages for reuse; this bound on pending metadata is
not an aggregate database/cache quota or a claim that the database file shrinks.

`go test -race ./internal/changefeed ./internal/index ./internal/journal` and
the loopback `go test -race ./internal/transferapi` pass. Tests cover exclusion
filtering, TLS receipt, restart, gaps, policy mismatch, premature completion and
backpressure. These are PC-local metadata tests, with simulated publication for
feed fixtures; they do not prove NAS content synchronization or resource budgets.

### Directory bases and historical lookup (2026-09-09)

Directory manifests contain no blocks and encode in the same 96-byte fixed
header/identity size as empty files and tombstones, with a distinct type flag.
They add no content read, watcher, timer or worker. Received-directory completion
compares the persisted base type and retains newer dirty generations.

Committed version metadata additionally stores one original relative path
(up to 4096 source bytes; JSON escaping may expand it). The version decoder caps
the entire stored object at 32 KiB. Historical selection makes a keyed metadata
lookup, not a history or root scan, before opening only the selected immutable
candidate. Retained content still requires verification and paced reading; this
does not remove the pending aggregate quota and retention work.

Local race tests cover directory-vs-empty-file classification, manifest round
trips, restart, stale generations, historical lookup after replacement/deletion,
and refusal through a different path. No NAS or sustained resource measurement
was performed for these changes. Sequential history replay is not installed;
the scheduler must still coalesce superseded work to avoid unnecessary transfers.

### Explicit replica pull resource scope (2026-09-09)

The new pull operation handles one batch at a time with no queued work, idle
polling or per-file goroutine. It accepts explicit file/batch maxima up to 8 GiB
and 128 entries, plus a local read/write rate from 1 KiB/s to 1 GiB/s. One file
shares that rate across base-signature reads, reused base reads and staged writes;
64 KiB chunks and coalesced waits permit less than 128 KiB of initial/unpaced I/O.
Native publication has its own paced verification/copy passes. The operation
context expires after 45 seconds; blocked kernel I/O is not forcibly interruptible.
Large or slow work needs resumable scheduling before these limits are useful in
production.

Verified output hashes staged bytes again while writing, in addition to the
delta decoder's verification. This adds CPU but no second content read. Retained
candidate reuse hashes/builds a bounded signature locally before skipping the
network download. Aggregate cache/storage quotas and retention remain missing;
per-operation limits do not cover accumulated ready/displaced/history files.

Local race tests pass for the replica, staging and TLS integration. The TLS
fixture uses independent temporary PC roots and a 256 KiB random base, followed
by one-byte insertion. It asserts one literal byte, full receiver-local reuse,
matching visible files, received encrypted-stream bytes below the base size and
no new traffic on a committed local retry. Tests run at 1 GiB/s, without deployed
CPU quotas. These are correctness/relative-traffic checks, not NAS wire capture,
NAS CPU/RSS or sustained acceptance measurements.

### Upload outbox and receipt recovery (2026-09-09)

The local upload outbox retains at most one batch, up to 128 entries and 160 KiB
encoded metadata. The 8 GiB reconstructed-content bound is not storage allocated
by the index; the snapshot/spool implementation described below still lacks
aggregate quotas. A compact commit receipt stores sequence and epoch alongside
the original proposal. Database operations touch selected records only and add
no timer, watcher, per-file worker or whole-tree scan.

Receipt recovery makes at most one explicit bounded metadata request; a receipt
already saved locally causes no network request. The response is capped at one
128 KiB journal record plus 1 KiB envelope allowance. Tests exercise restart,
identity/namespace/type/generation checks, one-batch backpressure, preserved newer
edits and TLS receipt recovery without a repeated upload. No new NAS, CPU/RSS,
ten-minute workload or packet-loss measurement was performed.

### Source snapshots and durable delta spools (2026-09-09)

Snapshot capture reads the selected file once using a 64 KiB buffer, copies it
to private native storage and collects at most 131,072 block digests (8 GiB at
64 KiB). Whole-file hashing runs while writing; it requires no second source
read. The explicit source rate is 1 KiB/s–1 GiB/s with one initial block burst.
Staging writes accompany those reads, so aggregate disk I/O includes both.
Fingerprint/path checks are metadata operations. There is no watcher, idle
timer or per-file goroutine in snapshot capture.

Delta encoding reads the immutable snapshot and writes an encoded spool with a
shared paced budget, allowing less than 128 KiB initial/unpaced work. Its context
expires after 45 seconds. The conservative encoded bound is target size plus
`MaxOperations*45 + 93` bytes; an all-new target can therefore temporarily need
both its full immutable snapshot and a comparably sized encoded spool. Spool
verification after reopen adds a bounded paced read before upload and then
paces reads from the returned handle. These disk/CPU costs require measurement
and aggregate quota/reservation before production use.

The outbox now includes each spool's digest and length, with a 160 KiB metadata
cap and space reserved for its later commit receipt. No content blobs are stored
in bbolt. Tests use a 256 KiB random source and a one-byte insertion at a 1 GiB/s
test rate. They verify stable snapshot bytes after an edit, compact delta reuse,
spool reopen/corruption refusal, TLS upload and preservation of newer dirty work.
They do not measure NAS CPU, sustained RSS, power loss or automatic throughput.

### Explicit upload dispatch (2026-09-09)

The dispatcher adds no idle timer or worker. One explicit call processes at most
one 128-entry outbox with a 45-second context. It holds up to 128 read-only wire
spool descriptors until the call finishes; it does not allocate file-sized
buffers. Full selected-path checks run a constant number of times per batch,
with global policy checks between spool verifications. Each spool retains its
own paced reader and initial burst allowance. This is not a measured aggregate
I/O or CPU bound across many small files.

`go test -race ./internal/transferapi ./internal/replica` exercises local TLS
dispatch, one-byte diff reuse, index restart and publication/receipt recovery
without local payload spools. A saved receipt causes no new encrypted-stream
bytes; disabled/paused/excluded/over-limit/canceled/policy-denied operations cause
none either. Namespace mismatch is tested against authenticated loopback TLS.
No NAS workload, sustained CPU/RSS or actual packet accounting was measured by
these tests, and the running observer does not call the dispatcher.

### Durable upload intake and retry (2026-09-09)

Each new upload adds one bounded durable metadata reservation before receiving
bytes, with at most 128 entries and 128 KiB. Only the exact candidate names are
checked; there is no directory sweep. The reservation is removed in the existing
transaction that prepares publication. No idle worker, age-based expiry or
per-file goroutine is added. A failed intake intentionally blocks new commits
until its owner resumes or explicit resolution becomes available.

On retry, each owned ready candidate gets one bounded paced size/digest check.
It retains its inode and is not reconstructed again. Abandoned partial cleanup
includes directory fsync. The wire spools are still sent and consumed at the
shared request pace; these retries do not yet omit previously received payload.
New delta receipt reuses the decoder's target digest for pre-seal verification,
avoiding a second hashing writer. These are code-level work bounds, not measured
NAS resource guarantees or aggregate retained-storage quotas.

`go test -race ./internal/journal ./internal/stage ./internal/transferapi` covers
a killed receiver with a real partial, lifetime-lock exclusion, exact-owner
restart, a truncated second TLS stream, retained inode/mtime reuse, corrupt
candidate refusal and wrong target digest before sealing. TLS fixtures use a
1 GiB/s test rate. No actual QNAP failure, sustained CPU/RSS, power loss or packet
budget was measured. The installed observer still has no transfer caller.

### Completing uploaded snapshots (2026-09-09)

`FinishUpload` processes one explicit outbox with a 45-second context. Snapshot
reads share a paced budget across the batch, with 64 KiB coalescing and no idle
timer. One file handle and block buffer are active at a time; whole/block hashes
are collected in the same read pass. Rebuilding manifests adds a full local
snapshot read after upload, including after a restart between the two databases.
It never rereads the live source or sends network data.

At most 8 GiB reconstructed content and 128 files yield at most 131,200 manifest
blocks across the batch. Decoded and encoded manifest collections coexist during
the atomic index update (roughly 5 MiB and 4.6 MiB at that bound, plus database,
allocator and other process overhead). These are representation bounds, not a
measured RSS limit. Snapshot and wire retention still need aggregate quotas.

Local TLS tests use a 256 KiB random file and one-byte update at a 1 GiB/s test
rate. They verify zero network traffic during completion, later delta reuse,
own-upload feed completion and preservation of intervening edits. Separate local
tests reopen the journal/index between completion steps and verify atomic rollback
and corrupt-snapshot refusal. The full non-race suite is included because the
killed-intake fixture previously exited early without the race runtime; it now
remains live until explicitly killed. No NAS CPU/RSS or sustained-load acceptance
measurement was performed, and this code is not invoked by the installed daemon.

### Completing downloaded changes (2026-09-09)

`FinishDownload` handles one inbox batch with the same 45-second, 8 GiB/128-entry
and 64 KiB paced-read limits as upload completion. Retained files are read once
to rebuild whole/block hashes; directories, tombstones and excluded entries
need no content reads. It uses one active content handle and retains bounded
batch manifests through one atomic index update. It does not read live source
content, send network data, enumerate a directory or add an idle worker.

Observer records and dirty generations are preserved, including events caused
by the publication itself. The explicit bounded comparison described below clears
unchanged dirty work with a live-file read. Automatic invocation remains an integration gap,
not a claim of zero processing after downloads. Rechecking an already pinned
replica origin uses a read-only database transaction; it does not fsync on each
completion attempt. Initial origin binding is one durable metadata write.

`go test -race ./internal/replica ./internal/index ./internal/journal` covers
restart, newer observed edits, corrupt candidates, atomic completion, directory
creation/deletion, all-excluded batches and origin binding. The loopback TLS
replica test completes actual file create/delta update/delete with a 256 KiB
fixture at a 1 GiB/s test rate. These are local correctness checks; no NAS CPU,
RSS, sustained throughput or packet-loss acceptance measurement was performed.

### Selected local comparison (2026-09-09)

`Compare` handles one dirty path with a 45-second context and a 1 KiB/s–1 GiB/s
read rate. A regular file receives one bounded fixed-block hash pass, up to the
configured 8 GiB format ceiling. Unchanged acknowledged content generates no NAS
request, snapshot or wire spool. A changed file whose remote head still matches
the verified immutable base needs one head request and reuses cached block
metadata; it does not request a remote signature rebuild. A different remote
version needs bounded signature metadata and can require a native NAS content
read behind that request. These are per-operation bounds, not aggregate quotas.

Metadata checks open only the selected leaf and parent; no directory enumeration
occurs. Missing parents are not deletion evidence. The operation has no idle
worker, per-file goroutine or retry loop. Tests exercise generation changes during
comparison, disappearance of an unacknowledged temporary file and independent
local/remote edits. Loopback TLS verifies no new encrypted-stream bytes while
clearing an unchanged download-generated event. Automatic scheduling is still
absent, and this comparison has not run against the production NAS share.

### Native QNAP fixture as the sync account (2026-09-09)

The native probe now passed all 11 checks on Linux 4.2.8/aarch64, first through
the admin setup session and then as UID 1000/GID 100, supplementary group 100.
Command: `python3 scripts/run_native_probe.py --write --as-sync-user
--session-dir /tmp/ananas-admin-session-vyxJ2VTA
--binary /tmp/ananas-native-probe-arm64`, preceded by the same read-only preflight
without `--write`. The foreground command uses `GOMAXPROCS=1`, a 64 MiB Go soft
memory limit, nice 15, 15/20-second CPU limits, 45-second context and paced native
publication. It does not install or run a persistent helper.

For the 512 KiB random fixture and one-byte insertion, the non-admin run reported
4.724918849 seconds elapsed, 0.269685 seconds user CPU, 0.140705 seconds system CPU
and 8,175,616 bytes (about 7.8 MiB) peak RSS. CPU/RSS come from Linux process
`getrusage` through probe completion, including process startup, and exclude
unrelated NAS services and the SSH transport process. The 491-byte delta and
344-byte signature are serialized local sizes, not packet counters. The 5,528,128
byte executable upload was setup traffic and is separate from those sizes.

The probe cleaned its own fixtures; an exact-path readback confirmed the uploaded
tool child was absent and production/test-parent modes/owners were unchanged.
[Raw recorded values](evidence/qnap-native-probe-2026-09-09.json) include the binary
digest and scope. This small native test does not measure sustained daemon CPU,
TLS-helper RSS, idle behavior, network isolation, disconnects or power loss. Those
acceptance gates remain open; no automatic writes were enabled.

### Selected upload preparation (2026-09-09)

`replica.PrepareUpload` processes one explicit selection of at most 128 paths,
with a shared 45-second context and configurable file/batch limits up to 8 GiB.
It adds no timer, idle worker, per-file goroutine or background scan. A single
160 KiB-bounded metadata preparation record holds candidate ownership before
content is written. Atomic promotion replaces that record with the one outbox.
Failed preparation prevents accumulation of more preparations until explicit
cleanup. This is not an aggregate quota for committed history or other stores.

Unchanged files use the comparison's one paced content pass and generate no
snapshot or NAS request. Changed regular files currently incur comparison and
snapshot source passes, followed by local retained-base signature generation,
snapshot delta encoding and wire writing. These are bounded per-file passes;
the configured rate applies to each operation and permits their existing initial
bursts. It is not a measured aggregate disk/CPU budget for a many-file batch.
Base signatures use local retained content, avoiding another NAS signature read.
Snapshot and encoded copies can coexist at their earlier documented size bounds.
Selected manifests remain bounded by the aggregate source-byte and entry limits;
no file-sized RAM buffer is introduced.

Abort reads and removes at most four exact private names per claimed ID, with no
directory enumeration or user-file content reads. It flushes removals before the
index forgets ownership. The caller must serialize all replica work and hold the
journal lifetime lock. The killed-process fixture covers PC-local recovery after
sealing a snapshot; it does not simulate NAS power loss or disconnects.

Validation: `go test ./internal/index ./internal/stage ./internal/replica` and
`go test -race ./internal/transferapi ./internal/index ./internal/stage
./internal/replica ./internal/content ./internal/journal`. The TLS fixture uses
a 256 KiB random file and 1 GiB/s test rate, verifies a sub-1-KiB serialized update
delta, matching visible content, retained newer edits and explicit deletion.
These tests provide correctness and relative transfer evidence, not new CPU/RSS,
sustained throughput or packet-counter measurements. Automatic writes remain off.

### Event-driven worker and lifecycle integration (2026-09-09)

The optional replica worker adds one goroutine when supplied to the daemon
lifecycle runner, one coalesced wake slot and one status-change slot. Successful
observer batch completion has a separate one-slot hint; per-file UI counter
updates do not wake content work. Pause/resume notification also has one slot and
is emitted only after changed intent is durable. A nil worker adds no transfer
goroutine or NAS requests and remains the production CLI configuration.

Work uses existing 128-entry/8-GiB maximum batch bounds, at most one received
32-record page, one upload preparation/outbox and sequential transfer operations.
The selected-path/source-read costs documented above still apply. Local dirty
paging does not itself refetch an empty feed; feed reads follow an external hint,
upload completion, saved-inbox recovery or a possibly full remote page. A pass
may process many bounded turns while actual work remains. An error waits for a
new hint, with no internal polling/backoff timer. Repeated external notifications
can still cause repeated attempts; production route/reconnect and conflict handling
need acceptance. Per-operation byte limits are not aggregate storage quotas.

`go test -race ./internal/transferapi -run
TestTLSWorkerObservesUploadsPullsAndPauses` passes actual inotify → worker → TLS
file and nested-directory creation, a one-byte diff update, a remote update through
the authenticated notification stream, and local deletion. Test source size is 256 KiB,
read rate 1 GiB/s, observer coalescing 20/60 ms and metadata pace 10,000 ops/s;
these are correctness-test settings, not deployment defaults. Update encrypted
sent-byte growth is asserted below the base file size. After a 1.1-second settle,
150 ms idle adds no encrypted bytes or observer metadata reads. That short window
does not establish sustained idle CPU or GNOME overhead.

`go test -race ./internal/daemon ./internal/replica ./internal/control
./internal/observe` covers shutdown joins, separate display/work hints, durable
control notifications, active-request cancellation on pause and absence of an
internal retry loop after feed failure. No new NAS workload, packet capture,
ten-minute CPU/RSS or deployed service measurement was performed.

### Native QNAP TLS helper fixture (2026-09-09)

Command and exact binary hashes are recorded in [helper service validation](helper-service.md)
and [raw evidence](evidence/qnap-helper-test-2026-09-09.json). The helper ran for
45 seconds as UID 1000/GID 100, with `GOMAXPROCS=1`, Go soft memory limit 64 MiB,
nice 15, CPU soft/hard limits 30/45 seconds, two accepted connections, 1 MiB file
maximum, 2 MiB batch maximum and 2 MiB/s service read pace. These explicit test
settings do not configure the installed daemon. There is no idle request timer,
watcher or per-file goroutine in this helper service.

For a fixed 512 KiB file and one-byte insertion, all eight protocol checks passed
in 4.20377939 seconds. Upload and download each reconstructed the update from
512 KiB of local receiver reuse plus one literal byte; the encoded update was
491 bytes. The write probe counted 534,109 encrypted stream bytes sent and 11,418
received, including initial upload and metadata. The separate state-only probe
counted 2,321 sent/2,292 received. These socket counters exclude TCP/IP headers,
retransmissions and the 16,833,694-byte setup executable uploads.

Linux `getrusage(RUSAGE_SELF)` through helper shutdown reported 0.155733 seconds
user CPU plus 0.069215 seconds system CPU (0.224948 total) and 10,100,736 bytes
peak RSS, about 9.6 MiB. This includes helper startup and the short fixture but
excludes the root launcher, SSH and other NAS services. It is not a ten-minute
idle/load test, a hard RSS bound, a many-small-files benchmark or GNOME measurement.
The helper's soft Go limit is not a process memory limit. Aggregate retained
storage quotas, deployment budgets and sustained acceptance remain open.

### Authenticated remote notifications (2026-09-09)

The coordinator now rotates one shared signal after each successful durable
commit, including recovery and replica adoption. It allocates no per-subscriber
queue or worker and does not notify on idempotent retry or uncommitted publication.
A stream reads the current sequence at subscription and after actual changes;
there is no idle database read, heartbeat or polling timer. Sequence and signal
are sampled under the publication mutex to avoid losing a concurrent commit.

Each helper stream carries a length-prefixed JSON frame capped at 256 bytes plus
the four-byte prefix, containing namespace, epoch and sequence only. Real changes
coalesce to at most one frame per second; the client also paces received hints to
one per second. Timers run only after an actual change/frame. Each client retains
one pending hint and a 4 KiB buffered reader, plus HTTP/TLS/socket overhead. No
path, version manifest or content is sent in a hint, including for excluded edits.

API notification allowance is explicit, 0–4 streams, and one per authenticated
client ID. The native helper derives `min(4, maxConnections-1)` to leave at least
one accepted connection outside the stream allowance; `maxConnections=1` disables
streams. The example's two connections permit one stream. Connection limits still
include idle transfers and TLS handshakes; this is not a fairness guarantee for
multiple clients. Slow stream writes have a five-second deadline. Client transport
now permits two connections, with one active transfer and one stream, and one idle
connection. Stream cancellation/close joins application work before state closes.

`go test -race ./internal/journal ./internal/daemon ./internal/transferapi
./internal/helper` covers durable signals, paused content, concurrent transfers,
offline commit replay on explicit reconnect, malformed frames, same-client stream
refusal, remote-failure shutdown and quiet-helper shutdown. The worker's real
inotify/TLS test now downloads remote content from a stream hint; no hint is injected
by the test. Its 150 ms idle assertion follows a 1.1-second settle. The dedicated
stream test separately checks unchanged encrypted-byte counters for 1.1 seconds
after draining coalesced changes. These short local windows are not sustained
CPU/RSS, kernel packet capture or QNAP stream measurements.

The lifecycle adds one stream goroutine only when explicitly configured. Detected
stream failure stops the lifecycle for its supervisor; reconnect is not an internal
timer. A silent broken link needs network-event cancellation, which is still
pending. Content pause allows metadata hints; automatic production network policy,
external SMB observation and helper deployment remain unfinished. The installed
CLI continues to supply neither a worker nor a remote stream.

### QNAP notifications and socket flags (2026-09-09)

The expanded explicit probe now tests a state query, initial notification and
two-second quiet window before writes. Its write mode runs ten checks, adding a
committed-change hint while the transfer connection remains usable. Both modes
passed on QNAP before and after adding `SO_DONTROUTE` to device-bound sockets.
The helper validates inherited and accepted socket flags; the PC validates them
before connect. This adds bounded socket-option syscalls per connection, with
no polling, extra per-file work or new dependency. It is not a route-failure test.

Both runs used the existing 45-second, UID 1000/GID 100 helper setup and 512 KiB
fixture, with 1 MiB file/2 MiB batch limits, two connections, 2 MiB/s read pace,
`GOMAXPROCS=1`, 64 MiB Go soft limit and nice 15. [Commands](helper-service.md)
record exact scope; separate raw files pin each build.

| Run | Write probe seconds | Helper CPU seconds | Peak helper RSS bytes | Setup binary bytes |
|---|---:|---:|---:|---:|
| [Notifications](evidence/qnap-helper-events-test-2026-09-09.json) | 5.011939962 | 0.236009 | 11,255,808 | 16,838,371 |
| [Notifications + socket flags](evidence/qnap-helper-direct-socket-test-2026-09-09.json) | 4.832729521 | 0.245706 | 11,124,736 | 16,843,610 |

CPU/RSS use helper process `getrusage` through shutdown, excluding root launcher,
SSH and unrelated NAS services. These two short runs do not establish a performance
difference between builds. The final read-only quiet window lasted 2.001152511
seconds and added zero encrypted bytes. Its state/notification probe counted
2,538 bytes sent and 2,633 received overall. The write probe counted 536,481 sent
and 14,353 received, including initial upload, metadata and notifications. There
was no separate quiet-window measurement in write mode. Socket counters exclude
IP/TCP headers, retransmissions and setup binary traffic.

Both updates reused 524,288 receiver-local bytes and one literal byte, with a
491-byte serialized upload delta. Cleanup and unchanged existing directory owners
were independently verified. No production worker, global network rule, sustained
CPU/RSS workload or route-failure packet test was installed/run. See
[remaining egress validation](egress-policy.md); M2 and production writes remain open/off.

### Isolated socket routing fixture (2026-09-09)

The new `ananas-route-probe` and Python namespace harness add no production
worker, timer or polling. They run only explicitly in fresh privileged network
namespaces. The actual socket-policy package is exercised with one or two fixed
64-byte echoes per connection, a two-second client I/O deadline, ten-second client
lifetime, 75-second server lifetime and 90-second harness lifetime. Each server
accepts at most 32 connections sequentially. Packet capture caps snapshots at
160 bytes and retained records at 2048; event-pipe messages are capped at 2 MiB.

The final twelve-case run took 15.254244189 seconds on PC kernel 7.0.0-30-generic.
It retained 128 full test TCP frames, largest 130 bytes, and kernel capture
statistics reported 416 packets received with zero drops. The larger counter
includes nonselected packets/bridge observations. Every ordinary-socket control
used the gateway (27 selected packets total); constrained counterparts used none,
while still completing direct echoes. Each case has a 1.25-second post-client
capture window. Independent `tcpdump` decoding confirmed the gateway selection.

This is packet-path evidence, not a CPU/RSS benchmark, sync throughput measure,
WAN packet budget or long retransmission test. It uses virtual Ethernet and a
bridge/router in three anonymous namespaces, not the physical LAN or QNAP kernel.
All child processes were reaped and both namespace holders independently checked
absent. Commands, build hashes, raw frame snapshots and remaining cases are in
[egress validation](egress-policy.md). Production automatic writes remain disabled.

## Native publication admission budget, 2026-09-09

The assembled helper now requires `maxCacheBytes` and `maxCacheEntries`; the example
and disposable runner specify 32 MiB and 128 entries, without defaults. Charges
cover a conservative candidate/publication/displaced-base allowance plus fixed
per-operation/entry amounts. They persist through failures, retries and commits.
Accounting uses bounded exact metadata lookups and no idle timer, whole-tree scan
or payload read. A one-time bootstrap reads at most two private native state names.

`go test ./internal/transferapi ./internal/helper ./internal/journal ./internal/stage`
and `go test -race ./...` passed with temporary Go caches and host loopback access.
Local native TLS create/diff/download/delete uses five history entries and reserves
4,128,771 logical bytes. Separate tests verify refusal before decoding/staging,
smaller subsequent admission, process-kill/restart and unchanged retry charges.
These are functional accounting checks, not measured physical disk/CPU/RSS budgets.
The earlier QNAP evidence does not validate this new behavior.

No committed-history credit exists yet. The budget eventually stops new work rather than
letting retained payload grow indefinitely. Safe ACK/conflict-aware reclamation,
physical disk/free-space limits and QNAP validation
remain pending. External writers can grow an already displaced inode beyond its
recorded size; filesystem/bbolt allocation is outside the logical byte allowance.
See [full scope and commands](cache-budget.md). Autosync remains disabled.

PC mode now shares a persisted budget between upload snapshots/wire spools and
downloaded publications. Worker construction requires it; dispatch/adoption also
verify the exact outbox reservation. Bootstrap permits the observer index and
reads at most three private state names. An upload charges after durable index
preparation and before content creation, while a pull reserves durable intake
before its first download request. Exact untransmitted preparation abort removes
and flushes owned candidates before crediting the reservation; committed content
remains charged. No periodic accounting scans or idle work were added.

The encoder now limits a spool to target bytes plus at most 45 framing bytes per
target byte (capped by the existing operation count), fixed header and terminator.
This avoids charging a tiny file for the entire global frame allowance. This is
a conservative size bound, not the expected amount of transmitted data. Local
fixtures continue to verify one-byte diffs; wire-bound tests cover zero to 512 KiB
with empty/literal/mixed content and limit arithmetic without large allocation.

The complete `go test -race ./...` suite passes, including the TLS/inotify worker
with a 256 MiB/128-entry test cache, refused dispatch of unaccounted old outboxes,
budget refusal before snapshot or download, process kill after snapshot creation,
reopen/abort credit, cleanup failure without credit and shared upload/pull capacity.
These test limits are not PC deployment defaults. Existing short idle checks still
pass; sustained CPU/RSS, physical allocation and QNAP budget acceptance are pending.

Completed upload wire reclamation now removes only exact owned encoded-spool
names after verified adoption, while the receipt/outbox remains durable. It uses
one directory fsync per bounded batch and one atomic ledger credit; snapshots and
history counts remain charged. This adds no content read, directory scan, age timer
or network request. The reclaimed amount is the reserved wire allowance, not a
physical disk measurement. Errors before credit retain the full charge.

The full local Go race suite passes with the real TLS/inotify worker verifying
absent completed spools and retained diff snapshots. A killed-process test stops
after unlink/credit but before index finalization, then finishes through the worker
without networking, preserves a newer edit and verifies no double credit. A partial
cleanup/hard-link refusal test preserves the full charge across reopen. These tests
do not establish power-loss durability, sustained resource use or QNAP behavior.

## Batched client acknowledgements, 2026-09-09

The worker now sends authenticated cumulative acknowledgements only when its
durable completed prefix exceeds the confirmed cursor. One request covers at most
32 committed batches, with a 16 KiB framed request limit and 1 KiB response limit.
The NAS verifies at most 32 exact journal records and atomically updates one client
cursor/policy. First policy registration inspects at most 64 small client records;
established identities use exact lookups. The policy table is capped at 64 clients.
No file data, directory traversal, idle poll or periodic retry is involved.

An identical NAS retry performs no bbolt write; the journal test checks its write
counter around that retry. PC confirmation is separate from receipt/completion and
is saved only after an exact response. A lost reply remains pending across restart.
The worker confirms completed pages before continuing, while preserving local work
between full pages. ACK metadata commits do not generate content-change hints.

`go test -race ./...` passes, including a full 32-batch range, gaps/future sequences,
policy and identity refusal, bounded clients, local index reopen and lost-reply
simulation. The real local TLS/inotify worker verifies server/local cursor agreement
after bidirectional work; its existing 150 ms no-added-traffic/read check after
settling still passes. This does not measure long-term CPU/RSS, packet-level ACK
overhead or QNAP behavior. [Protocol and limits](acknowledgements.md) describe the
authenticated bookkeeping claim and the still-unimplemented history reclamation.

## Kernel network notification lifecycle, 2026-09-09

The local TLS/inotify worker test now uses a kernel monitor with one blocking
goroutine, a netlink socket and cancellation eventfd. Startup uses one exact
interface-index ioctl through a transient unconnected control socket. No IP packet,
DNS lookup, interface dump, root scan, ping or heartbeat is performed by the monitor.
Its userspace receive buffer is 32 KiB; it requests a 64 KiB socket buffer and
requires reported capacity no greater than 128 KiB. A subscription processes at
most 64 ignored datagrams before forcing revalidation, including events accumulated
over a long lifetime. Relevant events or uncertainty terminate it immediately.

The lifecycle subscribes before starting transfers, propagates cancellation to the
worker and quiet remote stream, and joins each session before reuse. Poll has
no timeout and cancellation wakes it via eventfd; a started cancellation callback
is joined before descriptors can be reused. `go test -race ./...` passes on the PC.
Real read-only subscription/cancellation tests run a warmup plus 32 cycles with no
change in `/proc/self/fd` count. Synthetic parser/lifecycle tests exercise event
selection, uncertainty, startup failure and joins. The real local TLS/inotify worker
with this monitor retains its existing short idle check.

Session recovery now keeps the observer, index and controls alive. One supervisor
goroutine owns nonoverlapping worker/stream/monitor attempts, bounded one-slot
channels and a retry timer only after failure. Delays are 1, 2, 4, 8, 16, 32, then
60 seconds; an attempt lasting at least 60 seconds resets the delay. There is no
jitter yet. Each attempt subscribes before its mandatory local-only LAN check.
Denied policy and healthy sessions have no retry timer. An absent interface waits
for link events, while failed subscription setup retries locally without NAS I/O.

Lifecycle tests verify one observer lifetime, responsive offline controls, no
repeated denied-policy checks over 1.1 seconds, stream-failure recovery without a
route event, and no new session while an old worker's shutdown is delayed. The
real local TLS/inotify test injects lifecycle interruptions around the kernel
monitor, records offline edits on both roots and verifies bidirectional replay.
One observer invocation spans recovery; interruption alone does not change its
scan count. Ordinary file reconciliation can increment that same counter. The
disconnected client's encrypted counters remain unchanged through the offline
edit/window; this is not a packet capture or sustained resource measurement.

No routes/interfaces or NAS data were changed for these checks. They do not prove
actual route-event cancellation latency, silent-link failure detection, QNAP
support or sustained CPU/RSS. Content errors with a still-healthy stream need
separate retry classification; final UI/deployment integration remains open.
[Scope and commands](network-lifecycle.md).

The subsequent isolated kernel event fixture passes six real fault cases with the
actual monitor and session supervisor, using lifecycle doubles for observation,
work and notifications. Its race-instrumented run took 24.71 seconds (rounded by
Go test) on PC kernel 7.0.0-30-generic. Command-start to service-stop observations
were 0.959–14.958 ms; restoration-to-restart was 2003.541–2005.820 ms including the
two-second retry delay. Each case observes 1.1 seconds offline without repeated
gate checks or worker restart and verifies responsive controls. These are small
fixture timings, not content cancellation, isolated kernel latency, sustained
CPU/RSS or a worst-case guarantee. The test uses one disposable veth pair in a
fresh namespace, a 70-second context, two-second command limits and 16 KiB output
bounds. It removes the pair and verifies only loopback remains. No production
timer, network rule or service changed. [Evidence and scope](network-lifecycle.md).
