# PLAN — anaNAS

A lightweight Linux folder synchronizer between a PC and a NAS, written in Go,
with a local web UI. **[PROJECT.md](PROJECT.md) is the requirements source of truth.**
This document translates its eight requirements into implementation steps; design
proposals below do not add new hard requirements.

### Current live checkpoint — 2026-09-10

The first real PC reboot exposed a GNOME menu initialization race. The panel
registered seven rows, but its first daemon snapshot emitted `LayoutUpdated`
while GNOME AppIndicators was between its layout and deferred property requests.
That client cancels the first load and can retain the default empty labels, which
matched the visible seven hover targets with no text. The fixed menu now starts
with the connecting layout as its baseline, so the first state uses property
deltas only. Failed user actions send a desktop notification and disconnected
status is contextual. Seven panel tests pass; the installed menu registered at
revision zero with all labels populated.

The same reboot successfully started the saved-credential CIFS mount, daemon and
graphical indicator. A 104-byte PC file reached the NAS in 3.113 s and its deletion
in 2.510 s; a separate 111-byte NAS file reached the PC in 2.001 s and its deletion
in 2.001 s. Both fixtures were removed. Final status was unpaused, ready and
`LAN sync active`. See `docs/live-trial.md` and `docs/startup.md`.

The GNOME menu caching bug is fixed and the panel update installed. The daemon
and exported DBus menu reported active sync while GNOME retained an old
"transfers disabled" label: the panel emitted only `LayoutUpdated`, and the
installed AppIndicators client fetches only type/child structure on that signal.
Existing labels now emit bounded `ItemsPropertiesUpdated` deltas. Unit checks
preserve no signals for unchanged metadata; real DBus menu clicks verified pause
and resume label updates, with sync restored active. See the panel evidence in
`docs/live-trial.md`.

Production startup is now configured at the user's explicit request.
`ananas-mount.service` and its scoped SMB firewall are enabled at boot; a
NetworkManager dispatcher triggers later LAN mounts. User lingering is enabled
for the PC daemon. A normal unmount/remount used the existing root-only saved
credentials, and a subsequent exact file sync/cleanup passed. No NAS password
was requested by runtime mounting or synchronization. The earlier manual-mount
and before-login limitations below are historical. The 2026-09-10 PC reboot now
supersedes that earlier pre-reboot evidence; a NAS reboot remains untested.
The user also explicitly removed all safeguards/approval rules from AGENTS.md
and authorized continued production operation/testing. Application requirements
remain described in PROJECT.md. See `docs/startup.md` and its recorded evidence.

The user explicitly approved the LAN-only trial before M2 completion. The PC
worker and non-admin NAS helper are installed and running; the loopback status
reports `automaticWrites: true`. Dedicated PC/NAS endpoint firewall rules are in
place, and the initial 256 KiB verification file matches on both sides. Native
NAS regular-file observation is enabled with a separate private index, 50 paced
metadata operations/s, 8192 watches and 1.5–5 second coalescing. Selected changed
files are captured at 2 MiB/s; equal-size edits may require one comparison pass
plus one capture pass. No idle content polling was added. The full Go race suite
passes with the helper's real local TLS/inotify import test.

The folder update is now installed on both sides. Native NAS nested-file and
empty-directory creation reached the PC in 3.612 s; a PC edit returned to the
NAS in 0.992 s with matching hashes. Directory capture seals an identity receipt
without listing children; child-driven directory timestamp/size changes do not
create false local conflicts or duplicate directory versions. The full Go race
suite and vet passed. See the commands and scope in `docs/live-trial.md`.

The subsequent regular-file deletion update is installed on both sides. A
32 KiB observed file deleted on the NAS disappeared on the PC in 2.001 s;
PC re-creation and deletion returned to the NAS in 1.957 s and 2.068 s.
Native absence capture verifies an accessible parent and unchanged root/mount,
flushes the parent, and seals a receipt before committing the tombstone. It
never deletes a visible path itself and retains the prior immutable version.
Local tests cover receipt recovery after a later re-creation, safe refusal of
uncertain paths, reservation cleanup, and preservation of a divergent PC edit.
Reconfirmed absence now dirties an existing missing index record after each
completed requested scan; this fixes coalesced create/delete and restart windows
for previously observed paths, with no extra filesystem reads or idle polling.
The full Go race suite and vet pass. See `docs/live-trial.md` for commands.

Directory-tree deletion, journal heads that have never had an observer record
(including a first create/delete entirely inside one coalescing window),
conflict choices, history reclamation and
remaining M2 acceptance are unfinished. The user removed the obsolete
M2-before-sync rule from AGENTS.md and requested permanent NAS SSH access;
these requests supersede earlier sequencing and SSH-disable instructions.
The checkpoints below describe earlier
builds unless explicitly updated; their disabled-write statements do not undo
the later, explicit trial approval. Current evidence belongs in
[live-trial.md](docs/live-trial.md).

Reviewed against the source and existing code on 2026-09-06. The priority is
correct synchronization with **minimal CPU, disk work, and network traffic**.
Performance numbers below are proposed acceptance budgets, not measured results.

### Active deployment target (2026-09-08)

The user requested completing and deploying real synchronization, a background
daemon and a desktop status icon with traffic, properties and pause/resume.
The local directory `/home/darth-vader/NASdir` was created with mode `0700`.
The selected NAS destination is a new dedicated share named `Nasdir`, separate
from the disposable `nas-sync-test` share. QNAP share creation completed after
the user approved read/write access for `nas-sync-test` and admin, with guest
access denied. The manual CIFS mount is active at `/mnt/nasdir`, with SMB 3.1.1,
`seal`, `cache=strict`, `serverino`, `actimeo=1`, `nosuid,nodev,noexec` and `soft`.
The systemd user service is installed, enabled and running for this local folder,
with the loopback dashboard at `http://127.0.0.1:8721/`. It currently tracks local
metadata only and explicitly reports `automaticWrites: false`. The user unit
enforces 10% of one CPU, `MemoryHigh=128 MiB` and `MemoryMax=256 MiB`.
Do not treat the new local folder as a synchronized or backed-up location yet.
The graphical-session panel service is installed and registered with GNOME's
existing AppIndicators host. It shows an anaNAS label, sync state, payload totals,
pause/resume, local-folder access and the dashboard. It receives bounded local
event streams, has no idle polling and enforces a separate 5% CPU / 64–96 MiB
memory budget. Pause from the menu, persistence across daemon restart, reconnect
and dashboard-to-panel resume were verified. This does not enable real transfers.

The user renamed the application **anaNAS**. The installed dashboard, launcher,
service descriptions and panel label now use that brand. A static symbolic
pineapple is the persistent panel icon, with separate state overlays. A colored
pineapple is bundled in the dashboard and launcher. Existing service/config/state
identifiers remain compatible, with an `anaNAS` command alias. No transfer gate
was changed by this branding update.

The host is Ubuntu GNOME Shell 50.1 on Wayland. QNAP's control panel reports
TS-128A, QTS 5.2.6.3195, Realtek RTD1295 ARM Cortex-A53 and 982 MB RAM.
The existing test mount is `rw` in the host namespace; `eno1` still has the
direct `192.168.1.0/24` route. Noninteractive sudo is unavailable; host
authentication through a desktop prompt enabled the manual mount and a bounded
packet capture. Egress rules have not been installed or validated.
QNAP's Telnet/SSH panel limits SSH logins to administrators.
The existing sync account remains non-admin; browser sign-in does not provide
an SSH session or authorize reading any password.
After automatic review initially rejected expanded administrator access, the user
explicitly approved temporary SSH setup on port 22 and disabling it after setup
and validation. The QNAP panel now reports the SSH setting applied. A visible
Ptyxis desktop terminal was opened for user-entered authentication using
`scripts/open_nas_admin_session.py`; authenticated access succeeded. Read-only
inspection reports Linux 4.2.8/aarch64, admin UID 0, and rsync 3.0.7/protocol 30.
`flock`, `python3`, `su` and `setpriv` were not found on that session's PATH.
The PC content reader's `openat2` requirement does not work on this NAS kernel;
NAS-side implementation needs a separately validated compatible path API.
Close the temporary control session
and disable NAS SSH when setup/validation finishes; do not leave this admin access
as the automatic synchronizer's runtime identity.
QNAP's per-share requested disk-flush option is enabled for `Nasdir`. The user
approved QNAP's warning about temporary suspension of all NAS services. After
applying, reopening the properties confirmed the checked option; the host CIFS
mount remained `rw`, the existing disposable child passed its read-only probe,
and the local observer service remained active. This is not crash-durability proof.

## 1. Requirements and design boundaries

Live-trial priority (2026-09-09): the user explicitly requested focusing on a
working installation and real use instead of completing broader edge-case
validation first. An opt-in native LAN trial is being prepared for the existing
`NASdir`/`Nasdir` pair. `sync.enabled` and helper `liveWrites` activate the existing
transfer engine with private TLS identities, direct physical-LAN/mount gates,
device-bound `SO_DONTROUTE` sockets and dedicated endpoint firewall rules. Defaults
remain disabled. This user-authorized trial does not close M2 or represent full
release acceptance. Cache/history capacity, conflicts and unsupported external
NAS edits must be reported honestly; the complete objective remains active.

After automatic review rejected activation under the earlier sequencing rule,
the user explicitly approved enabling the LAN-only trial before M2 is complete,
including daemon startup, sync writes and scoped firewall rules. The PC endpoint
rule is now installed; the live trial is activated. Native inotify capture of
regular-file edits now passes the local helper TLS/race fixture. External
directory creation subsequently passed both local and real-NAS tests. External
deletions and the full conflict dialog remain unfinished.

Network lifecycle progress (2026-09-09): the tested daemon now preserves local
observation and controls across transfer-session recovery. A bounded netlink
subscription precedes each local-only LAN check; denied policy waits for an event
without polling. Session/subscription failures retry with 1–60 second exponential
backoff, with no healthy-idle timer; all previous services and pooled connections
are released before reuse. A missing interface waits for link events. Read-only
PC subscription, descriptor cleanup, lifecycle and TLS/inotify offline-edit replay
tests pass, with one observer invocation across reconnects. Six actual kernel
fault cases now pass in a fresh isolated PC namespace: route/address/link loss,
interface recreation, a blocking rule and a gateway route, using lifecycle doubles
around the real monitor/supervisor. Physical transfer faults, silent failures,
QNAP, jitter, status UI wiring and final OS egress acceptance
remain open. The installed observer does not instantiate this monitor or worker.
[Scope and bounds](docs/network-lifecycle.md).

Client progress (2026-09-09): authenticated `POST /v1/ack` now advances at most 32
committed batches per request, binding logical client identity and exclusion policy.
The PC persists confirmation separately from its durable completed prefix; the
worker retries lost replies from that state and emits no idle acknowledgements.
Local TLS/inotify, range/restart/refusal and full Go race tests pass. The NAS trusts
the authenticated client's bookkeeping claim; no history reclamation uses it yet.
Retention must account for all configured clients and current/in-flight/conflict
pins. QNAP endpoint acceptance and deployment remain open.
[Scope and tests](docs/acknowledgements.md).

Storage progress (2026-09-09): the assembled native helper now requires durable
publication reservations before intake/publishing. The example and disposable
runner use 32 MiB and 128 history entries; exact retries preserve one charge and
commits retain their historical charge. Local TLS refusal, process-kill recovery
and full Go race checks pass. This bounds logical receiver admission, not physical
disk allocation. PC mode now accounts for upload snapshots/wire spools and download
publication together. Worker construction and outbox dispatch require a matching
reservation; explicit untransmitted preparation abort credits only after durable
cleanup. Local killed-process, quota-refusal and TLS/inotify worker tests pass.
Completed uploads now reclaim only encoded spools after verified local adoption
and before clearing their receipt/outbox. One batch directory flush precedes atomic
wire credit; process-kill recovery completes without network or double credit.
Snapshots and metadata remain charged. Retention/ACK/conflict pins, abandoned
intake resolution and QNAP budget validation remain open; M2 and production writes
remain disabled. [Details and commands](docs/cache-budget.md).

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

Latest addition (2026-09-09): the native helper executable, strict configuration,
inherited device-bound socket and non-admin launcher now pass the real disposable
QNAP TLS fixture. Upload/download each reuse 512 KiB for a one-byte update; the
serialized update is 491 bytes. The 45-second helper reported 0.224948 seconds CPU
and 9.6 MiB peak RSS, excluding launcher/SSH. Cleanup and unchanged parent owners
were independently checked. See `docs/helper-service.md` for exact commands,
binary-pinned evidence and limits. This advances authenticated native transport
validation; M2 remains open. No production helper or transfer worker is installed.
Egress enforcement, aggregate quota/retention, native SMB observation and remote
notifications, recovery/conflicts and sustained resource validation remain open.

Subsequent local integration now delivers remote commit hints over a persistent
authenticated stream to `daemon.RunWithRemote`. The TLS/inotify worker test
downloads a remote edit without an injected hint. Journal signals occur only
after durable commit; reconnect retrieves the current sequence, and replay still
uses the durable inbox cursor. One-slot hints and at most one frame per second
bound notification work, with no idle heartbeat/poll. Tests cover missed commits,
duplicate streams, concurrent transfers, protocol errors and shutdown joins.
Real-QNAP stream validation now passes the expanded disposable fixture, including
a two-second quiet stream with zero added encrypted bytes. Outgoing, inherited
and accepted TCP sockets now verify device binding plus `SO_DONTROUTE`; the same
fixture passes on QNAP with these flags. Final helper CPU was 0.245706 seconds and
RSS 10.6 MiB for the small test, excluding launcher/SSH. This does not close M2:
route-failure packet tests, external SMB observation, route/interface event
cancellation and supervised reconnect deployment are still pending. See
`docs/egress-policy.md`; no global firewall/routing rules or production writes changed.

The subsequent isolated PC routing fixture now passes twelve cases with the actual
socket-policy package: gateway/default/priority routes, established-connection
route changes and server replies. Ordinary controls sent 27 captured TCP packets
to the simulated gateway; constrained sockets sent zero and completed direct
echoes. The capture retained 128 frames with zero kernel-reported drops. This
advances the PC-kernel portion of M2; QNAP vendor-kernel route faults, live VPN/NAT,
interface/address loss, network-event cancellation and full transfer recovery
remain open. Exact commands, bounds, build hashes and scope are in
`docs/egress-policy.md` and `docs/evidence/socket-routes-2026-09-09.json`.

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
- A versioned binary BLAKE3 manifest codec with validation before allocation,
  capped at 131,072 fixed blocks (8 GiB per file at 64 KiB). The local index now
  persists a stable client ID, acknowledged manifests, global/per-path pause
  intent and unresolved conflict identities. A transactionally maintained dirty
  path index supports bounded work pages and preserves newer generations when
  acknowledging an older completed transfer. These are local primitives;
  conflict bytes still need immutable retention in the transfer engine.
- A separate local content reader for future scheduled work, with read pacing,
  fingerprint/root revalidation and digest verification before exposing upload
  ranges. Linux `openat2` rejects symlinks, special files and child mounts, and
  network/FUSE roots are rejected. It uses the existing pinned `x/sys` module
  (now a direct dependency), one candidate handle and reusable block buffers.
  The observer never calls this reader; automatic writes remain disabled.
- A standalone bounded rolling delta encoder/receiver and binary base signatures.
  Full base blocks can be reused after insertions/deletions; the receiver reads
  reused bytes locally. Corrupt/truncated streams and excessive weak-checksum
  verification work fail without fallback. A private native-filesystem staging
  store uses exclusive partials, verified/flushed ready candidates and explicit
  operation-scoped cleanup. Local tests include a killed receiver and retry;
  these are not wired into the daemon or NAS transport. Limits and
  remaining integration obligations are in `docs/delta-format.md`.
- A native-filesystem cooperative journal with an exclusive lifetime database
  lock, durable owner epochs, bounded prepared batches, expected-base conflict
  checks, idempotent operation IDs, immutable version descriptions and contiguous
  per-client cursors. A failed publisher leaves recovery work and blocks new
  commits. Local tests exercise competing processes, killed-publisher recovery,
  epoch rejection and journal gaps. A filesystem publisher is integrated in
  local tests; NAS transport is not integrated and this does not fence
  noncooperating SMB writers.
  See `docs/journal-protocol.md` for the contract and remaining gates.
- A native same-mount publisher now applies prepared file creates, replacements
  and explicit deletes, retains immutable candidates and displaced inodes, and
  replays interrupted publications. Local integration tests connect rolling
  delta → staging → journal → visible files, including a killed process after
  namespace exchange and partially published batches. It rejects unsafe paths
  and preserves external-write conflicts across retries. Parent directories must
  be committed before children. Directory creates and empty-directory deletes
  now have durable identity receipts and local recovery tests; no recursive
  directory deletion is performed. Adopting pre-existing directories into sync
  state, scheduler ordering, conflict resolution, storage quotas and
  full NAS failure/recovery acceptance remain open. The scoped native probe below
  now exercises the basic primitives on QNAP. See `docs/publication.md`.
- A separate `ananas-native-probe` executable and scoped SSH setup runner are
  ready for native QNAP validation. The command passes local read-only/write
  tests and cleanup, and cross-builds for ARM64. It exercises the real delta,
  staging, journal and publisher with a 512 KiB fixture and a controlled
  interruption before journal commit. All 11 checks now pass on the QNAP,
  including a foreground run as UID 1000/GID 100 with 0.41039 seconds process
  CPU and 7.8 MiB peak RSS for the small fixture. This does not close M2 or enable
  automatic writes; network isolation, real disconnect/power-loss, multi-client
  and sustained-resource acceptance remain open.
  Commands and scope are in `docs/native-probe.md`.
- An experimental authenticated HTTP transfer handler now integrates delta
  upload/download, bounded multi-file batches, directory/tombstone publication,
  idempotent retry and scoped recovery. Local TLS 1.3 tests exercise real temporary
  files with verified, allowlisted client certificates. It caps active work,
  accepted connections and reconstructed batch bytes, and paces reads. It is not
  a deployed helper or daemon client; production egress, aggregate storage quota,
  change replay and scheduling remain open. See `docs/transfer-api.md`.
- The separate transfer client now binds a private literal endpoint, source IP
  and interface, verifies TLS trust plus a server certificate pin, bounds response
  metadata and checks download headers before staging output. Local TLS race tests
  cover real diff round trips, cancellation, rejected responses and recovery.
  Encrypted stream traffic counters notify through one coalesced slot. This is
  not connected to the observer or desktop counters; route-change egress policy,
  source/output pacing and scheduler integration remain required.
- Authenticated journal-page reads now omit excluded path/version metadata while
  retaining batch sequence and omitted-entry counts. Pages bind namespace and
  exclusion policy; changed rules cannot silently reuse an old cursor. The local
  index durably stores at most one page (32 batches), separates receipt from
  completion and requires matching durable bases or conflict identities before
  completing visible entries. Local TLS/restart/gap/backpressure tests pass.
  The deployed observer does not fetch these pages. Replay coalescing,
  durable conflict-content retention, remote acknowledgements and policy-change
  reconciliation remain integration work.
- Local binary manifests and the planner now distinguish directories from empty
  files and tombstones. Directory bases persist through restart and can complete
  received directory entries without erasing intervening dirty generations.
  Committed version metadata now retains its path, permitting exact historical
  content lookup without a journal scan. TLS tests retrieve an old version after
  replacement/deletion and reject a lookup through another path. Older unbound
  version metadata is usable only through its current head; migration/reconciliation
  is explicit. These primitives do not schedule or publish replay automatically.
- An explicit bounded replica pull now joins the transport downloader to verified
  staging and native local journal/publication. It serializes one batch, paces
  local base reads/staged writes, verifies reused staged candidates and scopes
  pending recovery to the exact proposal. Tests pass for directory/file creation,
  one-byte delta updates, explicit deletes, late failure, conflicting visible
  edits and recovery without re-downloading. A TLS integration test publishes
  matching content into two independent temporary PC roots. The operation has
  no automatic caller and does not advance observer bases or remote cursors.
  Upload/observer integration, scheduling/coalescing, aggregate quotas, durable
  conflict resolution and real-NAS acceptance remain unfinished.
- A local upload outbox now retains one bounded proposal, observed generations,
  wire lengths and a commit receipt across restart. Preparation checks current
  bases/types/generations and observed exclusion/pause/conflict flags; both
  directions bind to the same remote namespace. Scoped operation-status reads
  let the recovery helper save an exact prior commit without resending payload.
  Receipt alone cannot clear work, and newer dirty generations survive completion.
  Tests cover local restart and authenticated metadata recovery. Production wiring,
  abandoned-intake resolution and unowned staging cleanup,
  quotas and automatic scheduling are still required.
- Local snapshot capture now reads a selected file once at an explicit rate,
  creates an immutable candidate and its block/whole digests, and rechecks source
  identity before sealing. Bounded delta encoding creates a separate durable
  wire spool; its length and digest persist in the one-batch upload outbox.
  Reopening a spool verifies size/digest/fingerprint before exposing paced bytes.
  Local TLS tests connect snapshot → spool → outbox → upload → acknowledgement,
  including one-byte diff reuse and preservation of a later dirty generation.
  These remain explicit calls in tests; scheduler wiring, quotas,
  incomplete-operation cleanup and NAS acceptance are still unfinished.
- An explicit upload dispatcher now consumes one durable outbox, queries the
  exact remote operation before opening spools, recovers prepared publication
  without retransmission and saves a matching commit receipt. Requests bind a
  configured journal namespace in addition to TLS identity. Local TLS tests
  exercise upload, restart recovery without local spools, pause/exclusion/size
  refusal and zero traffic when using a saved receipt. At most 128 read-only spool
  handles are held for one 45-second batch; selected-path checks are linear in
  batch size and there is no idle loop. This does not acknowledge local bases,
  resolve orphan remote staging, provide aggregate quotas or enable the daemon.
- The transfer handler now reserves one durable upload intake before receiving
  content. The journal binds the complete proposal and checks vacant candidate
  names, expected heads and version identities. Its lifetime lock and mutex
  protect receive/resume through commit; intake becomes prepared publication in
  one transaction. Exact retries verify retained candidates and can clean their
  abandoned partials. Local tests cover killed-receiver ownership, truncated
  multi-file TLS retry, corruption refusal and target-digest checks before sealing.
  The transport's store must match the pinned journal directory. Retry still
  sends the bounded wire spools; it does not negotiate omitted payloads. Unknown
  artifacts, abandoned/conflicting intake resolution, quotas and real NAS
  recovery acceptance remain open. No automatic service calls this protocol yet.
- Upload completion now rebuilds fixed-block manifests from retained snapshots,
  verifies their whole digests and records the authenticated remote result as
  local logical replica heads without publishing visible files. One index
  transaction saves the complete manifest batch and releases its outbox while
  preserving later dirty generations. Recovery after the journal commit but
  before the index transaction is idempotent. Local TLS tests cover subsequent
  delta pull, visible-edit preservation and completion of the upload's own feed
  entry. Directory inode adoption, observer-event reconciliation, conflict
  resolution, quotas, scheduling and production configuration remain open.
- Download completion now requires an exact committed local operation and
  matching heads, verifies retained file manifests and atomically saves bases
  with the oldest inbox batch's completion cursor. It leaves every observer
  record and dirty generation intact; the explicit comparison below reconciles
  selected publication-generated events. Directory/tombstone and fully excluded
  batches need no content reads. Local tests cover TLS create/update/delete,
  directory publication/deletion, restart, corruption and atomic rollback.
  Puller construction now pins the configured remote namespace, refusing a
  changed target or unbound prior history. The deployed daemon still does not
  call it; remote cursor acknowledgement and full NAS acceptance remain open.
- A bounded comparison now validates one observed path and its acknowledged
  journal/index base, hashes regular content once at a configured rate and clears
  an unchanged generation without contacting the NAS or creating a snapshot.
  Otherwise authenticated heads/signatures drive a three-way decision; an unchanged
  immutable remote version reuses the cached block manifest. Exact missing-leaf
  checks require an accessible stable parent/root, and unacknowledged absence
  cannot become delete intent. Tests cover zero-network reconciliation after TLS
  download, cached-base reuse, independent conflicts and intervening events.
  Comparison itself does not publish content or claim conflict-byte retention.
  The upload builder below now consumes push decisions; automatic scheduling and
  conflict materialization remain integration work.
- Explicit upload preparation now joins selected-path comparison, stable snapshots,
  local retained-base signature generation, delta encoding and a durable outbox.
  At most 128 nonoverlapping paths share one preparation record, capped at 160 KiB.
  Unused candidate IDs are durably claimed before content creation; the preparation
  becomes the outbox in one index transaction. A failed preparation blocks new
  work until explicit exact-ID abort, which leaves local source/dirty/base state
  intact and refuses committed versions. Local TLS tests cover actual preparation
  through initial upload, one-byte update, preserved newer edits and deletion.
  A real killed-process fixture verifies recovery after snapshot creation; index
  tests cover concurrent-builder/abort refusal and atomic promotion. This does not
  add an automatic worker or production NAS writes. Storage quota/reservation,
  committed-history retention, parent/child ordering and scheduler integration
  remain required before automatic use.
- A single replica worker now connects real observer completion hints and durable
  controls to preparation/dispatch/completion and remote inbox pull/completion in
  local TLS integration tests. It serializes operations, orders parent creation
  before child uploads, reads bounded dirty pages and waits without idle polling
  or error retry timers. Pause interrupts active operation contexts. The shared
  daemon lifecycle runner is now used by the CLI with a nil transfer worker,
  preserving metadata-only deployment while joining all services before storage
  closes. Actual remote-hint delivery, out-of-band NAS observation, quotas, egress,
  conflict/history and full directory reconciliation remain open. No automatic
  production writes were enabled; details are in `docs/daemon-worker.md`.
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

Remaining: wiring the persistent manifests/acknowledged bases into transfer scheduling
and pacing, transport capability probes, write-capable LAN enforcement, diff
materialization, journal/checkpoints, conflicts and paused-path resolution, retention,
WAN actions and the full web UI. A loopback-only status dashboard with durable
pause/resume intent is implemented; it does not initiate NAS work. No NAS content is accessed or modified by
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
Kernel CIFS debug data reports SMB 3.1.1, AES-128-GCM encryption, one active
  session channel and no advertised multichannel capability. A 106,593-byte
same-share copy increased CIFS IOCTL counters by two without increasing payload
read/write or read/write-operation counters; this is strong server-copy evidence,
but packet capture is still required before treating offload as proven.

The 2026-09-08 repeat adds packet evidence for that small same-share copy:
8 captured packets / 2,053 captured frame bytes / 1,525 TCP payload bytes during
the copy interval, with two successful CIFS IOCTLs and no payload read/write
delta. This validates avoiding a client payload round trip for that test only.
Arbitrary-range copies, large/shifted diff workloads, durability, fencing,
recovery and WAN egress gates remain open. See `docs/e2e-qnap.md` for scope.

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

Keep one daemon, a small embedded UI and the existing embedded bbolt metadata
database. Store cached content as bounded files outside the
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

The current implementation provides an opt-in, loopback-only dashboard and
`/api/status` endpoint (GET/HEAD), plus `POST /api/pause` for durable pause/resume
intent. It binds only when `webPort` is nonzero, embeds its HTML/CSS/JavaScript,
validates loopback Host/Origin headers, uses `Cache-Control: no-store` and receives
pushed status only while the browser tab is visible. Hiding the tab cancels its
stream; idle UI connections have no heartbeat or polling. DOM text is updated
only when different. Mutations require a random per-process
CSRF header, an exact matching browser origin and at most 1 KiB of JSON input.
The page uses a script nonce and rejects framing. Pause intent survives restart;
local observation continues, and resume cannot override capability/LAN suspension.
It reports the local observer, configured NAS identity, limits and transfer payload
counters (currently zero). `automaticWrites` is explicitly false. It does not expose
file contents or initiate NAS traffic. The Python/GIO desktop icon is implemented
using the session-bus StatusNotifierItem/DBusMenu protocols. The token-guarded
`/api/events` endpoint allows four streams, one pending hint per stream, and at
most one snapshot per second during changes; idle connections have no timers.
Writers have a five-second write deadline and shutdown closes streams explicitly.
The indicator has one stream worker, one bounded command worker and at most one
pending main-loop update; reconnect backoff is 1–30 seconds to loopback only.
Menu/icon/title signals are emitted only when those displayed values change,
so metadata-only churn cannot continuously repaint the desktop panel.
Desktop cost is part of the resource gate: measure GNOME Shell with the indicator
and dashboard present and absent, not just the application process's CPU. The
2026-09-08 short samples found sustained shell load even with these components
absent; they do not prove zero incremental UI cost or identify the shell's cause.
The full indexed file/conflict/action UI remains future work.

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
  A later bounded packet capture and CIFS counters validate one small same-share
  server-copy operation; larger and arbitrary-range workloads remain pending.
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
