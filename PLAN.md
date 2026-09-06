# PLAN — `nas-sync`

A lightweight Linux folder synchronizer between a PC and a NAS, written in Go,
with a local web UI. **[PROJECT.md](PROJECT.md) is the requirements source of truth.**
This document translates its eight requirements into implementation steps; design
proposals below do not add new hard requirements.

Reviewed against the source and existing code on 2026-09-06. The priority is
correct synchronization with **minimal CPU, disk work, and network traffic**.
Performance numbers below are proposed acceptance budgets, not measured results.

## 1. Requirements and design boundaries

| PROJECT.md | Planned behavior |
|---|---|
| 1. Complete sync only on the same LAN, without internet transit | Automatic push, pull, discovery and maintenance require a verified direct LAN route to the configured NAS. A VPN/private IP alone does not qualify. Loss of that route suspends automatic NAS access. |
| 2. Automatic sync by diff, minimizing transferred data for rewritten files | Coalesce writes, compare content against the acknowledged base, reuse verified blocks and transfer only missing content plus bounded metadata. Measure actual wire traffic in both directions, including materialization on the NAS. |
| 3. Continuously updated file list with minimal traffic | Update the local view from local events and incremental NAS changes. Adaptive LAN discovery trades a documented maximum delay for fewer requests. Outside the LAN show a cached view with its freshness and refresh only on request by default. |
| 4. Individual on-demand sync while online, using diffs | Explicit browse/open/download/upload of selected files is available off-LAN. Reuse cached manifests and blocks, resume interrupted downloads, and keep user actions scoped to the selection. Conflicts require a choice. |
| 5. Exclude local and remote folders from tracking and sync | Apply exclusions before watching, listing, hashing or transferring. Exclusion is not deletion; changing a rule cannot silently remove data or cause the opposite direction to overwrite an excluded path. |
| 6. Very low resource use, including many small writes | No idle hash work, bounded queues and caches, one background hash worker initially, paced I/O, event coalescing, and measurable CPU/memory/network budgets. |
| 7. Group edits; track PCs and their synchronization progress; tolerate clock differences | Bounded batches of changes and a NAS-side journal/checkpoint with persistent per-PC acknowledgements. Logical versions and base comparisons determine causality; wall time is for display. |
| 8. Warn on conflict; choose local/server or retain different versions and stop local sync | Preserve both candidates, present explicit resolution, and persist a per-PC pause for that path when the user keeps differing local and NAS versions. |

Interpretation retained from the previous plan: automatic synchronization is LAN-only;
WAN operations are on demand. Requirement 3 does not promise an always-fresh WAN list
without requests: that would require recurring internet traffic. Display that limitation.
Initial copies and completely changed content necessarily transfer all missing bytes.

Go, Linux/inotify, a QNAP target, SMB-first transport and a local web UI are current
project choices. **Zero NAS installation is a preference from the previous plan, not a
requirement in PROJECT.md.** Test whether existing NAS services satisfy the requirements;
if they do not, evaluate a small on-demand NAS helper before accepting excess transfer.
Do not silently replace diff synchronization with full-file transfers.

The previous plan's IPs, kernel version, QNAP firmware and service availability are
examples/unverified deployment assumptions. Discover actual capabilities during setup.

## 2. Current repository and known gaps

Implemented and exercised locally:

- CLI/configuration with strict JSON, bounded sizes/work settings, disjoint root/state
  checks and a mandatory LAN guard. `-scan-once` builds a metadata snapshot and prints
  status; normal mode runs the recursive local observer.
- Cancellable streaming BLAKE3 hashing with a bounded reusable buffer, invalid-size /
  stalled-reader checks, and direct digest collection without an intermediate block list.
- Deadline-driven coalescing with bounded path counts/bytes, continued ingestion under
  slow consumers and explicit root-reconciliation requests on overflow.
- A persistent bbolt metadata index with logical scan IDs and dirty generations;
  recursive fsnotify observation, exclusions before traversal, paced paginated scans,
  restart recovery, directory-move handling and conservative incomplete-scan behavior.
- A pure bounded diff planner that compares local/remote manifests against an
  acknowledged base, classifies push/pull/conflict without choosing a conflict winner,
  validates manifest bounds and deduplicates missing block payloads in manifest order.
- A read-only `-check-lan` diagnostic for SMB mount/source identity and physical-interface /
  direct-route evidence. It performs no NAS probe and never enables automatic writes.
- A `-discover-nas` command that reports kernel network mounts and user-session GVFS SMB
  paths without contacting them. The current environment exposes the dedicated
  `//192.168.1.30/nas-sync-test` share through kernel CIFS, while `satanasso.local`
  `home` and `public` remain GVFS/FUSE inspection paths.
- Reproducible local CPU/memory and network-syscall checks, hashing benchmarks, and an
  optional systemd user unit with CPU/I/O priority and an enforced CPU quota.

`go test ./...`, `go test -race ./...`, `go build ./...` and `go vet ./...` pass on
Go 1.27.1. See [README.md](README.md), [local measurements](docs/performance.md) and
[real-NAS validation status](docs/e2e-qnap.md). These results validate local observation,
not the proposed synchronization protocol or the full ten-minute acceptance workloads.

Remaining: persistent content manifests/acknowledged sync bases, transfer scheduling
and pacing, transport capability probes, write-capable LAN enforcement, diff
materialization, journal/checkpoints, conflicts and paused-path resolution, retention,
WAN actions and the full web UI. A loopback-only read-only status dashboard is now
implemented; it does not initiate NAS work. No NAS content is accessed or modified by
the current observer. Configuration settings for hashing throughput, cache size,
remote scans and web/SSH endpoints are reserved and validated where applicable; they
do not enable those future features.

Live mount discovery now finds the dedicated kernel CIFS mount
`//192.168.1.30/nas-sync-test` at `/mnt/nas-sync-test`, using SMB 3.1.1,
`cache=strict`, `serverino` and `actimeo=1`. The host is `192.168.1.17/24` on
`eno1`, with a direct `192.168.1.0/24` route. The two GVFS SMB mounts at
`/run/user/1000/gvfs/smb-share:server=satanasso.local,share=home` and
`...share=public` remain inspection/test-only because GVFS is a user-space FUSE
adapter and does not prove automatic-sync locking, fsync, watch, server-side copy or
route behavior. The real-NAS probe and setup are documented in `docs/e2e-qnap.md` and
`scripts/test_real_nas.py`; its write mode is explicit and directory-scoped.
The read-only probe passed for both the prior GVFS path and the dedicated CIFS path.
The disposable CIFS write probe passed for readback, same-filesystem rename,
exclusive create, `fsync` and `copy_file_range`, and removed its temporary objects.

Host preparation is complete for client-side testing: `mount.cifs` and `smbclient`
are installed, the dedicated QNAP share and account are mounted, and
`config.real-nas.example.json` records the verified IP, interface, prefix and mount
point. M2 remains open for wire-byte capture, server-side copy/offload, durability,
cross-client fencing, crash recovery and OS egress enforcement; the successful client
probe does not establish those properties.

The first implementation deliberately retains safe create/missing observations for
renames; verified content reuse and download-feedback suppression belong to the
write-capable engine. An observation failure currently stops the daemon with recovery
state intact; resilient path-specific retries and degraded availability reporting remain.

## 3. Architecture and capability gate

```text
local directory → recursive watcher → bounded dirty index → scheduler
                                                          ↓
local UI ← persistent local index ← reconciliation / conflict engine
                                                          ↓
                  LAN policy gate → transport capabilities → NAS
explicit scoped UI request → WAN policy gate → lazy SSH/SFTP → NAS

NAS: browsable files + hidden immutable versions/content + journal + PC cursors
```

Keep one daemon, a small embedded UI and one embedded metadata database (bbolt is a
candidate, not yet a dependency). Store cached content as bounded files outside the
sync root; do not put large blobs in a metadata database. No per-file goroutines.
Separate transport capability checks from policy and from pure diff planning.

### Gate before automatic writes

A mounted share does **not** imply server-side reconstruction. Reading old NAS blocks
into the PC and writing them to a NAS temp file sends unchanged bytes over the network.
Neither content addressing nor an atomic rename removes that traffic.

Prototype on the actual NAS and client stack:

1. Check same-volume atomic replacement, durable writes, exclusive coordination,
   cache visibility and recovery after client disconnect.
2. Check SMB server-side range copy/clone or an equivalent operation for creating a
   staged file from existing NAS content. `copy_file_range` support is filesystem/server
   dependent; success is insufficient proof of offload. Verify with wire counters.
3. For SFTP, discover supported extensions. Server-side `copy-data` and durability/
   rename extensions are optional; ordinary SFTP reads/writes do not imply them.
4. Check that coordination and visibility work **across SMB and SFTP clients**, not
   merely twice through one protocol. Test NFS separately before advertising it.
5. If native operations cannot meet these needs, compare an explicitly configured,
   bounded on-demand SSH helper with a small NAS service. The helper can reconstruct
   from NAS-local blocks and serialize publication without a PC round trip. It needs
   the same resource limits; no busy loop or perpetual hashing on the NAS.

Record the supported mode and measured bytes in `docs/e2e-qnap.md`. Enable automatic
writes only for a mode with verified diff materialization and commit guarantees.
Unsupported setups remain paused/read-only with an explanation. A costly full-file
WAN fallback needs explicit approval for that operation after showing its estimate.
Native-only multiwriter limitations must not be hidden behind an unproven lock scheme.

## 4. Data model

- **Content block:** digest, length and bytes. Start with fixed 64 KiB BLAKE3 blocks for
  the prototype; persist algorithm, block size and format version in manifests. Tune
  only after benchmarks. Existing versions remain readable after a setting changes.
- **Immutable version:** unique ID, path ID, parent/base version ID, author PC, file size,
  display timestamps, ordered block references and tombstone flag. Use compact binary
  digests internally and paged manifests for large files. At 64 KiB, a 1 GiB file has
  16,384 digests (512 KiB of raw hashes before metadata); do not refetch them unchanged.
- **Current head:** path → immutable version ID, published through the transaction
  protocol. Keep version objects separately so history is not an overwritten `.v` file.
- **Journal batch:** immutable transaction ID, assigned sequence and bounded list of
  path changes, including put/delete/rename. One coalesced batch can contain many files;
  publication/recovery is explicit per file, not a promise of atomic whole-tree sync.
- **Journal head/checkpoint:** small discoverable pointer to committed batches plus a
  compact snapshot. No repeated listing of an ever-growing directory to find “newest”.
- **PC state:** stable random PC ID, last durably processed sequence, and outstanding
  conflicts/deferred work. A processed event is not necessarily an applied file; expose
  that distinction. Batch acknowledgements after durable local progress, not per event.
- **Local index:** fingerprints, acknowledged bases, dirty generations, cached NAS view,
  pending operations, pauses and bounded content-cache metadata.

Use opaque IDs/sharded paths for metadata, not unescaped user paths. Validate all paths
against traversal and symlink escapes. Metadata/temp/cache directories are always
excluded. Detect case collisions; do not silently lowercase names. Initially support
regular files/directories; report symlinks, special files and unsupported metadata.

Content addressing reduces repeat payload, but one file/request per small block can
cost more than it saves. Batch availability checks, range reads and manifest updates;
consider immutable pack files with bounded indexes if measurements justify them.
Defer cross-file dedup beyond basic reuse if its lookup/GC costs exceed the savings.

## 5. Core algorithms

### 5.1 Lifecycle and reconciliation

- OFFLINE and REMOTE: keep lightweight local observation/dirty tracking; do not hash
  an accumulating backlog until a permitted operation needs it. No periodic NAS probe,
  WAN listing, DNS refresh, SSH keepalive or background transfer. Network/mount events
  from the OS can trigger local reevaluation of LAN eligibility.
- LAN: verify the gate before NAS I/O, replay journal entries since the durable cursor,
  compare local dirty state and remote versions against their common acknowledged base,
  then schedule nonconflicting work. **Never blindly flush local edits before examining
  remote changes on reconnect.**
- Bootstrap, watcher gaps and expired journal cursors require reconciliation. Normal
  reconnect uses the index/journal and dirty set, not a complete rehash. A daemon restart
  with an observation gap requires a paced metadata walk of affected roots.
- Missing/unmounted/inaccessible roots are unavailable, not empty. Never infer mass
  deletion from disconnects, permissions errors, incomplete scans or excluded folders.

### 5.2 LAN and WAN policy

Require the expected mount identity/source/share, configured NAS identity/address and
an allowed directly connected LAN interface/prefix, with route selection consistent
with that interface. Read local mount/route state before touching an automount path.
Reject public routes, VPN/tunnel paths and ambiguous identity by default; private or
link-local addressing is insufficient. Account for IPv6 and any SMB multichannel
paths, or disable unsupported routing configurations.

Recheck on route/address/mount changes and before opening transfer work. Bind supported
connections to the permitted interface. A strict no-WAN guarantee during route changes
also needs OS routing/firewall enforcement for mounted-share connections; an application
check alone has a race. Document/test that deployment prerequisite before claiming the
guarantee. Stop scheduling, close/cancel supported connections and keep recovery state
on loss of eligibility; do not leave unbounded goroutines blocked in mount I/O.

The WAN adapter is opened lazily with pinned host verification. Each explicit action
carries a selection, direction, byte budget and cancellation scope. It cannot inherit
or drain the background queue. Retry only within the action's lifetime and budget;
close idle sessions. Resuming the daemon does not resume a WAN upload automatically.

### 5.3 Coalescing and bounded scheduling

- Watch local directories recursively, registering new subdirectories and applying
  exclusions before traversal. Do not rely on mounted-share fsnotify for remote edits.
- Track a dirty generation per path. A single resettable timer waits for the next
  deadline; no active ticker when there is nothing to do. Start with 1.5 s quiet time
  and 5 s eligibility cap. **Eligibility is not a requirement to rehash a hot file every
  five seconds**, nor a guarantee of completion under continuous writes.
- Bound in-memory pending paths (initial target 10,000) and batch entries/bytes. Persist
  overflow or collapse it into subtree-rescan markers; keep draining watcher events.
  Kernel overflow sets a durable rescan-needed marker rather than losing correctness.
- Separate watcher ingestion, scheduling and transfer. One active job per path, deduped
  latest work, fair scheduling across paths. Preserve edits arriving during hashing.
- Back off repeatedly unstable or large hot files (initial retry range 5–60 s), with
  visible status. Do not grow queues or let one hot file starve quiet files. New limits
  bound queued work regardless of the coalescing interval.
- Persist dirty state in bounded batches. After an unclean shutdown, compensate for
  any unflushed event window with a metadata walk; no fsync for every write notification.

### 5.4 Hashing, diff and materialization

1. **Inspect cheaply.** Cache `(device, inode, size, mtime_ns, ctime_ns)` and a dirty
   generation. A fingerprint is a skip hint, not proof of content identity. A write
   event invalidates it even if size/mtime match. Metadata-only changes reuse content
   once verified; same-content rewrites produce no content transfer or new content version.
2. **Hash a stable candidate.** inotify does not report changed byte ranges. An arbitrary
   modified file generally needs one sequential read after coalescing, even if few
   blocks eventually transfer. Reuse a buffer and stream digests into bounded storage;
   check cancellation and pace between reads. Compare metadata/generation before and
   after hashing and recheck before publication; retry if changed. Active writers can
   still require snapshots/application quiescence for a consistent application-level
   image. Never advertise transaction-consistent database backups from this mechanism.
3. **Compare with the acknowledged base and current remote version.** If both sides
   diverged, preserve candidates for conflict resolution. If digests agree, acknowledge
   without retransmitting. Transfer only verified missing blocks; account for manifest,
   existence checks and protocol overhead as well as payload.
4. **Choose an evidenced algorithm.** Fixed blocks cheaply handle aligned overwrites but
   insertion near the start can shift every later block. Benchmark a bounded rolling
   matcher or content-defined chunking for large shift-heavy files before promising
   efficient arbitrary rewrites. Avoid rolling scans on small files by default. Full
   replacement and encrypted/compressed rewrites may have no reusable content.
5. **Append optimization is conditional.** Growth alone does not prove an unchanged
   prefix. Without a trustworthy append-only contract, verify it before reuse. Preserve
   old versions and stage even appends; no default in-place patch to the visible file.
6. **Stage and verify.** Reuse NAS-local content through the verified capability (§3),
   write missing blocks once, verify lengths/digests as they enter storage, then publish.
   Never reread unchanged NAS payload through the PC just to build a temporary file.
   If verified uploaded bytes could change locally, retain a bounded immutable spool
   or rehash on transfer. The published manifest must describe the bytes actually sent.
7. **Downloads** build a local temp file using verified local/cache ranges and only
   missing remote blocks, then atomically replace after checking for intervening local
   edits. Keep partial verified blocks for an explicit resume. Metadata browsing fetches
   no content, previews or hashes of remote files.

Do not hash an unindexed NAS file by downloading all of it over WAN merely to discover
its diff. Show the available full-download estimate or use an approved NAS-side hashing
capability; local WAN caching cannot eliminate a first read of previously unseen content.
Default compression off; enable only if measured byte savings justify CPU cost. Avoid
recompressing already compressed content and encrypt through the chosen transport.

### 5.5 Remote on-demand access

Serve cached, paginated directory listings with age/staleness and an explicit refresh.
Fetch only the requested directory/page and selected version manifests. Cache immutable
manifests and verified blocks; batch missing-range requests with bounded concurrency.
A click to open one file must not enumerate or sync its siblings or all version history.

Estimate payload plus metadata before a costly operation; display actual transferred
bytes including retries. Cancellation stops further requests. Conflicts do not silently
trigger a full-file WAN download; show its cost if a resolution requires one. Downloads
outside the sync root do not silently mark that root's version as acknowledged.

### 5.6 Change discovery

For cooperating clients, read the small journal head adaptively: initially 2 s while
active, back off to 30 s while quiet with jitter, reset on activity. Read only new bounded
batches; batch cursor writes. Allow a lower-traffic profile (up to 60 s idle discovery).
SMB metadata caching can hide head updates, so test effective delay and bytes with the
actual mount options; one application stat is not necessarily one network request.

Out-of-band NAS edits cannot be discovered from the cooperative journal. Directory
mtime detects entry changes, **not edits to existing file contents**. Use a resumable,
budgeted file-metadata walk on the LAN, with directory hints only as an optimization.
Start with a 15-minute target sweep, at most 50 metadata operations/s and bounded slices;
large trees take longer and the UI must show actual coverage age. Hash only candidates
under the same budgets. Elect/assign one scanner when safe coordination is available
so multiple PCs do not all read/hash the NAS tree. External tools do not honor our locks;
coordinate exclusive managed writes when strong overwrite guarantees are required.

Size/mtime checks alone can miss metadata-preserving edits. Offer an explicit integrity
scan and optional infrequent, paced LAN verification. Document this detection tradeoff;
constant perfect discovery without a NAS observer requires recurring scan work.

### 5.7 Ordering and cooperative commit protocol

Atomic rename replaces a name; it is **not compare-and-swap on a version** and does not
atomically update content, manifest and journal together. HLC timestamps do not allocate
a global sequence or serialize writers. Prefer a coordinator-issued sequence and
immutable version IDs; defer HLC unless a concrete need remains. Never select a winner
by wall-clock time or reclaim a lock solely because another PC's clock says it expired.

Before enabling multiwriter mode, validate a common coordinator/locking mechanism for
all transports, with safe recovery and fencing of disconnected writers. An `O_EXCL`
lock file with a timeout is not sufficient. If native cross-protocol fencing cannot be
proved, use the helper/service route or report the mode unsupported. No automatic stale
lock stealing without ensuring the old writer cannot publish again.

Proposed recoverable transaction, under that validated serialization:

1. Stage immutable blocks/version and persist an operation ID and intended base locally.
2. Acquire commit coordination, verify ownership/fence and reread current heads. Compare
   with the expected bases and check for out-of-band changes; disagreement is a conflict.
3. Persist a prepared transaction with recovery information and assigned sequence. Retain
   the previous content/version until publication is recoverable.
4. Atomically replace each staged visible file, then its head, and publish the committed
   journal record/head last. Use same-filesystem staging and supported durable flushes.
5. Recover incomplete transactions under coordination before permitting overlapping
   work; idempotent operation IDs prevent duplicate publication. Readers encountering
   prepared work wait/retry or use the prior committed immutable version.
6. Advance the local acknowledgement only after durable application or durable recording
   of a conflict/exclusion/deferred job. Never skip a journal gap as if it were applied.

Browsable files and metadata can temporarily differ during a crash window. Test recovery
at every boundary; do not claim multi-file atomic visibility to File Station or other
external readers. Concurrent noncooperating NAS writers remain a documented limitation:
pre/post checks reduce races but cannot create CAS against arbitrary external writes.

### 5.8 Conflicts, deletes and renames

Track a common base for every path. Concurrent edit/edit, edit/delete, rename/edit and
same-name create must preserve data and stop writes to the conflicted path. Tombstones
represent intentional deletes, never disappearance caused by a failed traversal.

- **Keep local / keep NAS:** show the candidates and apply the choice against their
  current version IDs. A new intervening edit requires another comparison/choice.
- **Keep different versions:** retain the chosen local and NAS contents, persist a local
  pause for the original path, and perform no automatic push/pull/delete for it until
  re-enabled. A safety copy is optional, not a substitute for keeping the local version.
- Retain unresolved conflict versions independently of normal history expiration.

Use rename event information when available, otherwise verify identity/content against
indexed state. Inodes can be reused; a fingerprint alone is insufficient proof. Preserve
reuse on a verified rename and fall back to safe create/delete with content dedup when
ambiguous. Prevent watcher feedback from applied downloads using expected generations/
content, not by ignoring all events for a time window.

### 5.9 Exclusions and retention

Define local/remote exclusion precedence explicitly and test it in both directions.
An excluded endpoint blocks automatic operations that would touch that endpoint, including
deletions. Keep only minimal base/pause state needed to avoid treating exclusions as
missing files. Re-inclusion schedules comparison, not a blind overwrite. Pruning is a
separate explicit operation and is outside the initial release.

Bound local cache (initial 1 GiB), temporary staging, logs, history and journal storage.
Persist last-use metadata in batches; do not touch it on every block read. Never evict
unique pending upload bytes or pinned conflict/resume data; pause when space is exhausted.
Propose normal history retention of 30 days and a configurable NAS storage quota, with
conflicts and in-flight versions pinned. Show when pins prevent quota reclamation.

Use checkpointed, resumable mark/sweep or another verified reachability scheme under
commit-safe coordination. Protect current versions, retained history, conflicts and active
transactions. Publish checkpoints before pruning journal segments. PCs older than the
retention horizon reconcile from a checkpoint against their retained bases (or conflict
if the base is unavailable), rather than replaying missing history or resurrecting deletes.
GC and compaction run only on eligible LAN connections, in bounded low-priority slices.

### 5.10 Reliability and resource enforcement

Use bounded retries with exponential backoff/jitter, per-path failure state, cancellation
and a global failure circuit breaker. Disk-full, invalid manifests and checksum failures
must preserve the last committed version. Bound input sizes, block counts and directory
pages before allocation. Protocol faults must not trigger tight retry loops.

Expose separate foreground/background limits. Start background hashing and transfer
with one worker each, reusable buffers, read pacing (initial 20 MiB/s) and a CPU duty
budget. One goroutine or a fast hash alone does not cap CPU. Provide a systemd user
service example with low CPU/I/O priority and an optional enforced CPU quota; measure
its effect. Track NAS helper resources too. Large files may take longer rather than
monopolize CPU. Manual actions can use a separately configured higher budget.

## 6. Performance acceptance budgets

Measure on a documented reference PC/NAS/network with warm and cold caches. Use process
CPU time, RSS/allocations, bytes read/hashed, requests, disk/temp bytes and **actual wire
bytes in both directions**. Separate payload/metadata/protocol/retries and LAN/WAN.
App counters alone cannot prove SMB copy offload or absence of WAN packets.

| Scenario | Initial acceptance target |
|---|---|
| 10 min idle, no UI interaction | Average daemon CPU ≤0.1% of one core; no content reads/hashes. Remote/offline: zero app-initiated WAN requests/bytes, including UI dependencies. |
| Quiet LAN, scan not due | At most one journal-head poll per 30 s after backoff; no unchanged cursor writes, whole-tree listing or content reads. Record protocol keepalive/metadata bytes separately. |
| 100,000 tracked paths | Idle RSS target ≤128 MiB; no unbounded per-path goroutines or in-memory full manifests. Record watch/index overhead; lower limits cause visible backpressure. |
| Sustained churn for 10 min | Background process CPU target ≤10% of one core on average; pending memory stays capped, quiet files progress, cancellation responds within 1 s between cancellable I/O operations. |
| 500 writes to one small file in 3 s | ≤3 eligible batches with the default window; after stabilization, converge to final content. Measure actual hashing passes as well as batch count. |
| Rewrite with identical content | Zero content upload and no redundant content version; one bounded verification pass may still be necessary. |
| 1 GiB file, one aligned 64 KiB edit | Missing content payload ≤64 KiB with a warm base; no unchanged payload round trip during NAS staging. Report metadata and wire overhead separately. |
| Append and one-byte insertion near start | Verify final bytes and old version; record full hashing cost, reused bytes, metadata and wire totals. Shift-aware mode must demonstrate suffix reuse before passing the arbitrary-rewrite gate. |
| WAN reopen cached unchanged version | Zero content bytes; only necessary bounded freshness metadata. No sibling prefetch or periodic background refresh. |
| Disconnect/VPN/default-route change | No automatic WAN packets in controlled capture; no deletions caused by missing mount; bounded retry/worker count. |
| Journal catch-up / external edits | Cooperating changes discovered within configured poll bound plus processing time; external edits within measured sweep coverage, with stale status when budget prevents it. |

The initial 5 GiB transfer is a streaming/bounded-memory test, not a delta-saving claim.
Include empty/tiny files, 100,000 small files, sparse/large files, truncation, binary and
compressed rewrites, rename storms, slow storage and high-latency WAN. Reject performance
“wins” that lose events, skip conflicts, omit protocol traffic or corrupt content.

## 7. Local web UI and configuration

The current implementation provides an opt-in, loopback-only read-only dashboard and
`/api/status` endpoint. It binds only when `webPort` is nonzero, embeds its HTML/CSS/
JavaScript, validates loopback Host/Origin headers, allows only GET/HEAD, uses
`Cache-Control: no-store` and refreshes only while the browser tab is visible. It
reports the observer's local status; it does not expose file contents or initiate NAS
traffic. The full indexed file/conflict/action UI remains future work.

Embed templates and all necessary CSS/JS with `go:embed`; use simple Go templates and
minimal JavaScript (vendored htmx if useful). **No runtime CDN, web fonts, analytics or
update checks.** Tailwind, if used, is compiled at build time. Render paginated/indexed
views and coalesced event updates only while a UI client is connected; stop hidden-tab
refresh work. Opening the dashboard does not initiate WAN access.

Bind explicitly to loopback, validate origin/Host and protect mutating requests against
CSRF. Show LAN eligibility/reason, paused/dirty/conflict states, data freshness, bytes
transferred, resource limits and retention pressure. File previews/open/version actions
are explicit and bounded; do not execute downloaded files automatically.

Settings include roots, LAN identity/interface policy, transport capabilities, verified
SSH endpoint, selective sync, coalescing, background budgets, listing freshness, cache/
history quotas and per-path pauses. Update `config.example.json` together with each
implemented field; distinguish defaults from effective limits.

## 8. Implementation order and acceptance gates

- [x] **M0a — Existing foundation:** module, CLI, config, hashing/coalescing unit tests.
- [x] **M0b — Harden foundations and measure:** hashing/coalescing/config fixes,
  cancellation/slow-consumer/bounded-queue tests, hashing/allocation benchmarks and local
  resource/network-syscall harnesses implemented. Actual transport wire measurements
  remain in M2/M6; a local syscall trace cannot prove NAS offload.
- [ ] **M1 — Local observation (substantially implemented):** persistent index, recursive
  watcher, exclusions, bounded ingestion, batched metadata writes and restart/overflow
  reconciliation are working. Local idle/burst measurements and filesystem integration
  tests pass, including a 100,000-file idle memory check (~58.7 MiB RSS). Complete
  long-duration budgets, retry/fairness under scan storms and
  generation-aware transfer scheduling/feedback suppression before closing this gate.
- [ ] **M2 — LAN safety and NAS feasibility:** local SMB mount/route diagnostics and
  kernel-CIFS discovery are implemented; GVFS compatibility paths remain test-only.
  The prepared QNAP mount passed the bounded client-side read/write probe, but
  automatic writes remain disabled. Implement the enforced policy gate **before any
  automatic write**. Measure offload, durability, cross-protocol coordination and
  failure recovery on the real QNAP. Record the native/helper decision and supported
  deployment matrix (§3). Temp-dir tests cover logic, not SMB/NFS/SFTP semantics. Do
  not claim hard guarantees without E2E.
- [ ] **M3 — Correct bidirectional core:** immutable versions, persistent acknowledged
  bases, diff materialization,
  recoverable transactions, journal/checkpoints, acknowledgements, conflict pause and
  tombstones from the first write-capable engine. Verify two-client divergence, clock
  skew, each crash boundary, disk full and route loss. Then pass byte-saving budgets.
- [ ] **M4 — Low-cost discovery and lifecycle:** adaptive polling, paced external scans,
  reconnect comparison, retention/GC and old-cursor recovery. Verify existing-file edits
  with unchanged parent directory timestamps and no repeated whole-tree rehash.
- [ ] **M5 — WAN actions and full UI:** cached views, bounded explicit SFTP/helper actions,
  resumable diff downloads/uploads and conflict choices. Embed all assets. Pass packet
  captures for idle WAN and selection-only operations, including unsupported-copy cases.
- [ ] **M6 — Release hardening:** benchmark documented workloads on the actual NAS; run
  build/tests/vet/race checks, integration fault injection and long churn tests. Document
  setup, limits, LAN enforcement, recovery and measured CPU/network costs.

Planned packages: `index`, `watch`, `scheduler`, `languard`, `nasfs`, `diff`, `store`,
`journal`, `engine`, `conflict`, `remote`, `api`, `web`, alongside existing `config`,
`hash`, `coalesce`. Add dependencies/packages when their milestone needs them; avoid an
unused skeleton, redundant clock framework or mandatory frontend toolchain.

## 9. Remaining decisions

Resolve through M1/M2 measurements: actual QNAP/transport features and access paths;
whether a helper is needed; safe common coordination; fixed versus shift-aware chunking;
small-block storage layout; realistic scan freshness and CPU budgets on the target PC.
Do not mark these as “locked” based on the previous plan's assumptions. Preserve the
requirements when capabilities are absent, and explain the blocked mode to the user.

## 10. Technical references for corrected assumptions

- [fsnotify documentation](https://github.com/fsnotify/fsnotify): recursive watch setup,
  directory watching for editor replacement, and limitations on mounted network filesystems.
- [Linux inotify](https://www.man7.org/linux/man-pages/man7/inotify.7.html): event format,
  overflow and races; events do not provide modified byte ranges.
- [Linux rename](https://www.man7.org/linux/man-pages/man2/rename.2.html): atomic name
  replacement is not a conditional comparison of manifest versions or a multi-file transaction.
- [Linux copy_file_range](https://www.man7.org/linux/man-pages/man2/copy_file_range.2.html):
  filesystem-dependent copy/offload behavior; wire measurements remain necessary.
- [OpenSSH protocol extensions](https://github.com/openssh/openssh-portable/blob/master/PROTOCOL):
  discover SFTP copy/rename/fsync capabilities; do not assume the NAS supports them.
