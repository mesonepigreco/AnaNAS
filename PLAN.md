# PLAN — `nas-sync`

A Dropbox-like, **diff-based** folder synchronizer between a Linux PC and a QNAP NAS,
optimized for the **LAN** and written in **Go**, with a local web UI.

**Terminology (clarified):** "internet" in `PROJECT.md` means *outside the LAN* (WAN/remote).
The design maximizes automatic sync on the LAN, and separately allows **on-demand access**
(and explicit, user-requested sync) to files from outside the LAN.

---

## 1. Locked decisions & context

| Question | Decision |
|---|---|
| Sync client OS | **Linux only** (watch APIs: inotify via `fsnotify`) |
| Language | **Go** (single static binaries, small idle CPU/RAM) |
| NAS software | **None.** Pure **SMB3 (preferred) / NFS** share. Zero install on the QNAP |
| NAS data layout | **Real browsable files** (latest committed version) + hidden **`.nas-sync/`** metadata |
| LAN coordination | The NAS share *is* the coordination point: per-PC state + a change **journal** as files under `.nas-sync/`, written cooperatively by clients |
| Remote (off-LAN) access | An **SSH/SFTP adapter** to the NAS (QNAP SSH, reached via VPN, port-forward, or myQNAPcloud DDNS). Used for **on-demand read** and **explicit upload only** — never for automatic sync |

Because there is no always-on server process on the NAS, every "server" responsibility is
fulfilled by **cooperative file-based coordination** on the share: per-PC "latest synced
version" pointers, a monotonic change journal, and optimistic concurrency with conflict
detection at commit time.

Environment facts:

- Client: Ubuntu (kernel 7.0), LAN `192.168.1.0/24`, host `192.168.1.17`.
- QNAP: QTS/QuTS hero; SMB3 + NFS; SSH (SFTP) supported; share paths under `/share/<folder>`.
  No Container Station / QPKG required.

---

## 2. Requirement → design mapping

| # | Requirement | Design |
|---|---|---|
| 1 | **Full sync only on the LAN**; never automatically off-LAN | The engine has two modes (§5.2). **LAN mode** (share mount present + NAS on a private address) runs automatic, diff-based full sync in both directions. **Remote mode** (off-LAN) disables every background transfer: no auto upload, no background download. The moment the LAN mount reappears, the engine resumes and **automatically reconciles everything** (catching up remote-side changes made meanwhile and flushing local changes made while away). |
| 2 | Automatic sync at diff level → rewritten files send minimal data | **Block-level diff** (§5.4). A changed file is re-materialized on the NAS by reusing unchanged blocks; content-addressed chunk store dedups identical blocks across files/versions. Append/tail fast path avoids whole-file rewrite for constantly-grown files. The same diff machinery is reused for on-demand remote access so WAN transfers stay minimal too. |
| 3 | File list updated constantly, minimal traffic | On the LAN, a cooperative **change journal** (`.nas-sync/journal/`) appends one tiny entry per committed change; clients read only new entries, never scanning the tree. Remotely, the file list is fetched **on demand** only. |
| 4 | Access / on-demand sync of individual files when not on the LAN | **Remote mode** (§5.5): browse the NAS tree and *open/download* individual files (or a specific version from `.nas-sync`) by **block-diff** — only missing blocks cross the WAN. **Uploading a file off-LAN happens only when the user explicitly requests it** (UI "upload this file" / "push selection"); it is never automatic. Conflicts still force full-file handling. |
| 5 | Selective sync (exclude folders local & remote) | Two exclusion sets: **local** (never watch/upload) and **remote** (never track/download). Applied in the watcher/plan builder and in the pull path. Remote exclusions stay on the NAS untouched (opt-in "prune remote" deletes them). |
| 6 | Lightweight, no CPU burn under many small writes | Event **debounce + coalescing** groups bursts into one commit (§5.3). Hash cache keyed by `(dev, inode, size, mtime_ns)`. In LAN mode, idle state polls only one stat/s (newest journal file); in remote mode the engine is dormant except for explicit requests. Retry with backoff + jitter. |
| 7 | Merge edits into chunks; track syncing PCs & their latest sync; clock skew | One coalesced burst → one **commit** → one journal entry. Per-PC state files `.nas-sync/pcs/<pcID>.json` record last-applied journal seq + last-observed HLC. Ordering uses a **monotonic sequence + hybrid logical clock (HLC)**; wall clocks are for display/lease expiry only (§5.6). |
| 8 | Conflicts: warn; dialog local vs server; "keep two versions" stops syncing that file locally | Version graph per path. Push with a stale base ⇒ **CAS failure ⇒ conflict**, nothing clobbered. Divergent versions recorded under `.nas-sync/conflicts/`; web UI dialog: **keep local / keep remote / keep both** (latter stores a `name (conflicted copy)` and **pauses that path locally** until re-enabled). |

---

## 3. High-level architecture

```
┌─────────────────────────────────── Linux PC ─────────────────────────────────┐
│                                                                                │
│  ┌──────────┐  fsnotify  ┌───────────┐  dirty paths  ┌──────────────┐         │
│  │ Local dir │──────────▶│ Watcher    │──────────────▶│ Coalescer    │──┐      │
│  │  (root)   │           │ (inotify)  │               │ (chunk/batch)│  │plan  │
│  └─────┬────┘           └───────────┘               └──────────────┘  ▼      │
│        │ materialize / read                                ┌──────────────┐  │
│  ┌─────┴───────┐   ┌────────────┐    ┌────────────────────▶│ Sync engine  │  │
│  │ Block cache │   │Local index │    │ LAN auto-sync (diff)│ (modes §5.2) │  │
│  │  (bbolt)    │   │ (bbolt)    │    └─────────────────────│  + conflict  │  │
│  └─────────────┘   └────────────┘                         └──────┬───────┘  │
│        ▲                                                        │  ▲        │
│        │ on-demand fetch/diff (req 4)             ┌─────────────┘  │        │
│        │                                          │                │explicit│
│  ┌──────────────────┐                 ┌───────────┴────┐            │upload  │
│  │ Remote client    │  SSH/SFTP       │ LAN transport  │   (auto only) │only   │
│  │ (on-demand r/w)  │◀─ WAN (off-LAN) │ SMB3 mount     │◀──────────────┘       │
│  └────────┬─────────┘                 └───────┬────────┘                     │
│           │   network boundary               │ only when NAS reachable on LAN │
└───────────┼───────────────────────────────────┼────────────────────────────────┘
            ▼                                   ▼
┌────────────────────────── QNAP NAS  (browsable SMB/NFS share + SSH/SFTP) ─────┐
│  root/                    browsable, latest committed content (real files)     │
│  .nas-sync/                                                                     │
│    chunks/                content-addressed blocks (dedup)                     │
│    versions/<relpath>.v   per-file ordered block list + metadata + version     │
│    journal/<seq>.json     tiny append-only change records (path/ver/pc/hlc)    │
│    pcs/<pcID>.json        per-PC: last applied seq, last observed HLC          │
│    conflicts/<file>.json  divergent versions awaiting user resolution          │
│    locks/                 O_EXCL advisory locks for multi-PC transactions      │
│    tree.json              root directory digest (hint for cheap scans)         │
└──────────────────────────────────────────────────────────────────────────────────┘
```

Key abstraction: **`nasfs`** — a filesystem interface with two adapters:
- `mount` adapter (LAN): operates on the SMB/NFS mount path. Backs **automatic sync**.
- `sftp` adapter (WAN): SSH/SFTP to the NAS, opened lazily for **on-demand** operations.

The engine treats both as the same logical store; mode rules decide which may be used and
for what. In tests both adapters can point at plain temp directories, keeping tests hermetic.

---

## 4. Data model

- **Block**: fixed 64 KiB slice of a file (last block shorter), digest = BLAKE3.
- **Version**: immutable snapshot of one path = `{seq, hlc, pcID, base, size, mtime_ns,
  blocks:[digest…], tombstone?}`.
- **Manifest** (`.nas-sync/versions/<relpath>.v`): current version of a path + link to the
  previous (`base`), keeping history cheap.
- **Journal entry**: `{seq, kind: put/delete, path, digest, pcID, hlc, tz}` — the unit a
  client needs to refresh its view.
- **Chunk store**: content-addressed immutable files; identical content ⇒ identical path ⇒
  never uploaded twice (across files, versions, and PCs — including via SFTP).
- **Per-PC state** `pcs/<pcID>.json`: `lastAppliedSeq`, `lastObservedHLC`. This is the
  spec's "server keeps the latest sync for that PC", done cooperatively.

### Rename handling
`(device, inode, size, mtime_ns)` fingerprints pair delete+create into
`kind: rename` journal entries so moved files are not re-uploaded.

---

## 5. Core algorithms

### 5.1 Lifecycle & connection states

```
                 ┌──────────────── LAN mount present + NAS on private addr ─┐
   OFFLINE ◀─────┤                                                          ▼
     │           │                                              ┌──────────────────┐
     ▼           │                                              │  LAN MODE        │
  REMOTE MODE    └── re-connect ───────────────────────────────▶│  auto full sync  │
  (on-demand only)                                             │  (both directions)│
     ▲                                                          └────────▲─────────┘
     └────────────────────── mount lost / WAN only ──────────────────────┘
```

- **OFFLINE / REMOTE MODE**: no LAN mount. The daemon is nearly dormant. It may serve the
  web UI and perform **explicit user actions** through the SFTP adapter (browse, download,
  open a file/version). **No background transfer of any kind.**
- **LAN MODE**: the mount is up. The engine wakes, and on **first reconnect** runs a full
  reconciliation (both directions): flush local changes accumulated while away, pull remote
  changes made meanwhile, resolve nothing silently (conflicts surface in the UI). Then
  steady-state inotify pushes + journal pulls continue automatically.

### 5.2 Mode rules & the "no upload off-LAN" guard

1. Automatic sync engine **only runs in LAN mode** — determined by: mount present in
   `/proc/mounts` AND mount source resolves to a private/link-local address.
2. In remote mode the **SFTP adapter is read-only by default**. Write capability is armed
   per-operation only when the user explicitly requests an upload from the UI.
3. Explicit remote uploads are still routed through the same diff/commit machinery, but as a
   single on-demand action (never queued by the watcher).
This makes "accidental" WAN uploads impossible while keeping remote read access natural.

### 5.3 Coalescing edits into chunks (req 7)

- fsnotify events land in a per-path dirty set; a **debounce timer** (default ~1.5 s quiet,
  hard cap ~5 s of churn) drains it into one **commit unit** — a constantly-writing process
  yields one block-diff, not thousands. Knobs `coalesce.idle`, `coalesce.maxWait`.

### 5.4 Diff engine (req 2) — shared by LAN auto-sync and WAN on-demand sync

1. **Fingerprint check** — skip files whose `(ino,size,mtime_ns)` are unchanged.
2. **Block the file** (64 KiB) → ordered digest list; only re-hash changed blocks (cache).
3. **Compare against the remote base** = ordered digest list of the *current* remote version
   (from its `.v` manifest, or the remote's block list for on-demand). No whole-file pull.
4. **Classify**:
   - *Append/tail-only* (same prefix, grew): **tail patch** — write only new trailing bytes.
   - *Suffix/few-block edit*: pwrite only changed ranges when block layout is preserved
     (rolling checksum locates matches).
   - *Structural edit*: temp file + atomic rename; unchanged blocks copied block-wise from
     the existing copy, **only missing blocks uploaded** from the chunk store ⇒ wire bytes ≈
     changed blocks, not the whole file.
5. Update `.v`, append journal entry, bump HLC.

Content-addressing keeps even layout-shifting rewrites cheap.

### 5.5 Remote (off-LAN) on-demand access (req 4)

- UI file list and "open/download" operate on the **remote store view**: built by lazy
  stat/readdir over SFTP plus `.v` manifests when a file has version history.
- Downloading a file = materialize from the remote base + only missing blocks over the wire.
- Downloading an older version = read that version's manifest block list from `.nas-sync`
  and fetch only blocks absent locally.
- **Explicit upload** = same commit pipeline, flagged `explicit`, executed once; never
  triggered by the watcher while off-LAN.
- Conflicts during a remote operation resolve through the same dialog; **keep both** is
  available (the remote and local copies diverge, per-path sync pauses locally).

### 5.6 Change discovery & journal pull (req 3)

- **LAN**: pull loop reads `pcs/<ownID>.json.lastAppliedSeq+1 … newest` and fetches missing
  blocks. Out-of-band NAS edits (File Station, other tools) leave no journal → caught by a
  **dir-mtime scan**: only directories whose fingerprint `(dir, dirMtime)` changed are
  re-listed (default 60 s).
- **Remote**: no background discovery. The remote view refreshes on user navigation
  (bounded readdir depth).

### 5.7 Clock skew (req 7)

- Never compare wall clocks for freshness. Ordering is by monotonic **sequence + HLC**.
- Wall time is display/lease-only with a skew tolerance (default 5 min).
- `pcs/*.json` last-observed HLC lets a rejoining PC be told exactly what it missed.

### 5.8 Conflicts & resolution (req 8)

- A push carries `base` = the manifest seq it believes it updates. Commit succeeds only if
  `manifest.seq == base` (CAS via atomic rename). Otherwise → conflict, nothing lost.
- Engine stores both versions under `.nas-sync/conflicts/<relpath>`, marks the path pending,
  stops touching it, and surfaces a UI dialog.
- **Keep local** → push ours. **Keep remote** → drop local changes (keep a `conflicted copy`
  for safety). **Keep both** → versions diverge (`name (conflicted copy)` locally); that
  path is **paused on this PC** until re-enabled. Choices persist; nothing is auto-committed.

### 5.9 Selective sync (req 5)

- `excludeLocal`: applied in watcher + plan builder → never watched/uploaded, dropped from
  the local index. `excludeRemote`: applied on pull/remote view → never downloaded/tracked.
- gitignore-style globs + exact overrides; edits re-index only affected subtrees.

### 5.10 Reliability

- NAS writes are **temp file + atomic rename** on the same volume.
- Local **oplog** (`~/.config/nas-sync/<root>/oplog`) is fsynced before touching the NAS, so
  crashes resume rather than corrupt; startup reconciliation is idempotent (CAS).
- Retries with exponential backoff + jitter; a failing path never blocks the queue.

---

## 6. Performance / CPU posture (req 6)

- LAN idle: watcher goroutine + ~1 stat/s on the journal. ~0% CPU.
- Remote idle: fully dormant (only serving the UI / awaiting explicit actions).
- Burst: coalescer absorbs the storm → one commit per burst; hashing limited to dirty files
  and cached by fingerprint; BLAKE3 is multi-GB/s.
- Backpressure: if commit throughput < write rate, `maxWait` grows instead of the queue.
- All file IO streaming, bounded buffers; the NAS tree is never fully enumerated in steady
  state (LAN), and remote listing is depth/recursion-bounded.

---

## 7. Web UI (local webapp)

Served by the daemon at `http://localhost:<port>` (Go templates + htmx + Tailwind via CDN,
embedded with `go:embed`, single binary). The UI also acts as the "outside-LAN access
portal": the same UI is reachable when the client is off-LAN and shows remote/on-demand
actions.

- **Dashboard**: mode (LAN/remote/offline), queue depth, recent commits, per-PC activity.
- **File list**: live tree from the local index in LAN mode; lazily-loaded remote view in
  remote mode. Searchable, respects selective sync.
- **File activity/versions**: click a file → history from its `.v` chain; **Open** fetches a
  specific version on demand via block-diff (req 4).
- **Remote upload**: explicit "push this file/selection" action (enabled only off-LAN by
  explicit request; automatic in LAN mode). Clear UI labelling: *auto* on LAN vs *manual*.
- **Conflicts**: badge + dialog (keep local / keep remote / keep both), preview.
- **Settings**: NAS host/mount, SSH/SFTP remote endpoint, exclusions, coalesce knobs, scan
  interval, pause/re-enable per path.

---

## 8. QNAP specifics

- **Folder**: share, e.g. `/share/nas-sync`; data lands as normal browsable files (File
  Station, QTS snapshots, HBS).
- **LAN mount (SMB3)** in `/etc/fstab`:
  ```
  //192.168.1.x/nas-sync  /mnt/nas-sync  cifs  uid=1000,gid=1000,vers=3.1.1,_netdev,nofail,credentials=/etc/nas-sync.cred,x-systemd.automount  0 0
  ```
  `credentials` file `chmod 600`. NFSv4 alternative.
- **Off-LAN access**: enable QNAP SSH, reach the NAS via WireGuard (recommended) or a
  port-forward / myQNAPcloud DDNS. The app consumes an SFTP endpoint
  `host:port/user + SSH key`; it never tunnels or ports itself.
- QNAP SMB caveats to validate early: SMB3 multichannel, case-sensitivity policy (QNAP
  shares default case-insensitive → normalize or reject conflicting-case names), locking
  semantics (our coordination uses atomic renames + O_EXCL, SMB-safe).
- No Container Station / QPKG / myQNAPcloud client logic involved for LAN sync.

---

## 9. Repository layout (Go)

```
nas-sync/
  cmd/nas-sync/            main: CLI + daemon + embedded web UI
  internal/
    config/                config schema + defaults + validation
    nasfs/                 filesystem interface; mount adapter; sftp adapter; atomic helpers
    mode/                  LAN vs remote mode detection + guard rules
    index/                 bbolt store: local tree, fingerprints, block cache, remote view
    watch/                 fsnotify recursive watcher + rename pairing
    coalesce/              debounce/chunking of dirty paths
    hash/                  BLAKE3 block hashing + cache
    diff/                  block diff, append-patch, in-place & copy planner
    store/                 NAS chunk store + .v manifests + history chain
    journal/               cooperative change journal (read/write, seq/HLC)
    clock/                 hybrid logical clock + monotonic seq
    engine/                orchestrator: LAN sync / remote on-demand / conflict FSM
    conflict/              detection + resolution records
    remote/                SFTP on-demand client (browse/download/open/explicit upload)
    api/                   localhost HTTP/JSON API for the UI
    web/                   templates + embedded assets
  deploy/
    nas-sync.service       systemd user unit
    fstab.example          SMB3 mount example
    ssh.example            SSH/SFTP remote endpoint example
  go.mod
```

Module path placeholder: `nas-sync`.

---

## 10. Milestones & acceptance criteria

**M0 — Foundation** *(this session)*
- Git repo, `.gitignore`, `PLAN.md`, Go module + package skeleton that compiles
  (`go build ./...`), config loader, BLAKE3 block hasher, coalescer, with unit tests.

**M1 — Local observation (no NAS yet)**
- Recursive watcher + fingerprint cache + coalesced commit events; rename pairing.
- **Accept**: 500 writes in 3 s ⇒ ≤3 commit units; near-zero idle CPU.

**M2 — Push to the NAS share (LAN mode core)**
- `nasfs` mount adapter (temp-dir for tests), chunk store, `.v` manifests, journal, first
  full-sync, block-diff + append-patch, atomic rename, per-PC state.
- **Accept**: initial 5 GB sync; re-sync of a rewritten 1 GB file transfers only changed
  blocks (assert via counters). Hermetic tests on temp dirs.

**M3 — Pull, journal, mode guard**
- Journal-driven pull, dir-mtime out-of-band scan, LAN/remote **mode detection**, guard
  rules (no auto upload off-LAN), reconnect full reconciliation.
- **Accept**: second client root converges via journal only; toggling mount off stops all
  background transfer; reconnecting reconciles both directions.

**M4 — Selective sync, conflicts, remote on-demand**
- Exclusion sets; conflict CAS + resolution records incl. **keep both** (per-path pause);
  SFTP remote adapter: browse/download/open-version on demand + **explicit upload** only.
- **Accept**: divergent simultaneous edits ⇒ conflict, no data loss, choices persist; with
  the mount absent, downloads work over the SFTP adapter and uploads require an explicit UI
  action.

**M5 — Web UI**
- Dashboard, file list, version history + open, conflict dialogs, settings, remote view +
  explicit push controls.
- **Accept**: full workflow from a browser on `localhost`, LAN and remote mode.

**M6 — Hardening & real-NAS E2E**
- Clock-skew fault injection, crash/resume (kill -9 mid-commit), backoff storms,
  self-heal, `go vet` + `staticcheck` + race detector, benchmarks; E2E against the real
  QNAP SMB mount **and** real SSH/SFTP remote path; results in `docs/e2e-qnap.md`.

---

## 11. Risks & open questions

- **SMB locking semantics** — mitigated by atomic rename CAS; spike on the real share in M2.
- **In-place patching over CIFS** — fallback to block-wise copy always available; spike early.
- **WAN diff transfers** — SFTP random-access reads are feasible but slower; on-demand diffs
  may fall back to whole-file download for pathological layouts. Acceptable; measure in M4.
- **Case sensitivity** on QNAP shares — decide normalize/reject policy before M2.
- **Remote endpoint security** — SSH keys, known_hosts pinning; VPN recommended. Enforced
  read-only unless explicit write.
- Open for user: module path/remote URL; UI framework (Go templates + htmx proposed);
  sync root + NAS folder; block size & coalesce defaults (tunable).
- Multi-PC is secondary (Linux-only client), but journal/`pcs` design keeps it open without a
  NAS process.

---

## 12. First concrete build steps

1. `go mod init` + compiling package skeleton.
2. Implement `internal/config`, `internal/hash`, `internal/coalesce` with unit tests.
3. `go build ./...` + `go test ./...` green.
4. Continue M1…M6 in subsequent sessions.
