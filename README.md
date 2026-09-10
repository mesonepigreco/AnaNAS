# anaNAS 🍍

A Go synchronizer in development, guided by [PROJECT.md](PROJECT.md) and
[PLAN.md](PLAN.md). An explicitly approved **LAN-only sync trial is now installed**
for `~/NASdir` and the dedicated QNAP `Nasdir` share. It transfers content by delta
through a native NAS helper using mutual TLS. SSH is used only for setup. An optional
status/control UI binds to loopback when `webPort` is nonzero; it has no external assets.

The application is now named **anaNAS**. The dashboard, desktop launcher and
top-panel label use this name and bundled pineapple SVG artwork. The command
`~/.local/bin/anaNAS` is an alias for the installed binary. Existing `nas-sync`
service names, configuration/state paths and protocol identifiers remain stable
to preserve the current deployment and index.

The local observer uses recursive inotify watches, a persistent bbolt index,
bounded event coalescing and paced scans. It never reads file contents. Idle
observation does not poll the filesystem or contact the network. Excluded folders
are skipped before traversal; failed scans cannot turn unavailable data into deletes.

## Build and run

The active deployment target is `/home/darth-vader/NASdir` and a dedicated QNAP
share named `Nasdir`, automatically mounted on the configured LAN at `/mnt/nasdir`. The local folder and
share exist, and the systemd user daemon is installed and enabled. The active
profile now has `sync.enabled: true`; default/example profiles remain disabled.
The trial supports regular-file uploads, NAS-native regular-file edits back
to the PC, and new directories created on either side, with 64 MiB/file and
1 GiB/4096-entry history limits. A native NAS nested file and empty directory,
then a PC edit back, have passed on the real target. Deletion and re-creation of
an observed regular file with an accessible parent also pass in both directions.
Directory-tree deletion, conflict resolution and safe history reclamation remain unfinished. Use test
files while evaluating it; the configured size cap is not a large-file acceptance test.
See [live deployment and verification](docs/live-trial.md).

The deployed [Nasdir profile](config.nasdir.example.json) excludes the NAS recycle
folder and keeps all state outside the two roots. The dashboard is available at
`http://127.0.0.1:8721/`. User-service management:

```sh
systemctl --user status nas-sync.service
systemctl --user stop nas-sync.service
systemctl --user start nas-sync.service
```

Boot startup is enabled through user lingering, so the sync daemon can run
before graphical login. `ananas-mount.service` mounts the share using the existing
protected credential file when the configured physical LAN is available.
NetworkManager events trigger mounting after a later LAN connection; failed
mount attempts retry every 60 seconds while that LAN remains eligible. The
successful mount process exits, with no healthy-idle polling. Routine sync uses
saved TLS identities and requires no NAS password entry. The GNOME panel starts
with the graphical session. A normal remount and subsequent sync were verified;
a PC reboot was subsequently observed on 2026-09-10, followed by exact file
create/delete checks in both sync directions. A NAS reboot has not been tested.
See [startup configuration](docs/startup.md).

The desktop panel client is installed as `nas-sync-indicator.service` and starts
with the graphical session. Look for the **anaNAS** label and pineapple in the top panel. Its menu
shows sync state, uploaded/downloaded encrypted TCP totals since daemon start, durable
pause/resume, Open NASdir, Properties and status, and an explicit status refresh.
Properties opens the local dashboard; editing settings and instantaneous transfer
rates are not implemented. Traffic totals include TLS and protocol metadata.
The pineapple stays visible; a small status overlay indicates disabled transfers,
pause or an unavailable daemon. The static SVG has no animation, remote assets
or polling. The desktop launcher opens the current deployment's loopback dashboard
on port 8721.

```sh
systemctl --user status nas-sync-indicator.service
systemctl --user restart nas-sync-indicator.service
```

The client uses the existing Python GObject/GIO bindings and desktop
StatusNotifierItem host (verified with Ubuntu's AppIndicators on GNOME 50.1).
It reads the configured loopback port and local root, never touches the NAS,
ignores HTTP proxy environment variables and follows no redirects. A token-guarded
`GET /api/events` stream sends an initial snapshot and coalesced changes at most
once per second. There are at most four streams and one pending hint per stream;
quiet streams have no heartbeat or polling timer. Unchanged menu, icon and title
values emit no desktop update signals, including during local metadata churn.
Changed existing labels use DBusMenu `ItemsPropertiesUpdated`; structural
`LayoutUpdated` signals alone do not refresh GNOME's cached labels. The installed
panel now sends those property updates, fixing stale status, traffic and control
text after daemon changes. Real panel pause/resume notifications were verified.
A post-reboot correction also treats the fixed connecting menu as the initial
layout. This prevents the first daemon snapshot from canceling GNOME's asynchronous
property load and leaving seven interactive rows with blank labels. A disconnected
panel shows a contextual daemon error and retries with 1–30 second backoff. Failed
refresh, pause/resume and open actions also send a desktop error notification.
One bounded command worker handles user controls. The panel unit limits CPU to 5%
of one core and memory to 64/96 MiB
(high/max); these limits are separate from the daemon's budget.

Requires Linux and Go 1.27 or newer.

```sh
go build -o nas-sync ./cmd/nas-sync
cp config.example.json /tmp/nas-sync.json
# Edit local.root and nas.mountPoint before running.
./nas-sync -config /tmp/nas-sync.json -scan-once
./nas-sync -config /tmp/nas-sync.json
```

`-scan-once` prints JSON status after a metadata scan. Normal mode scans once and
then watches until SIGINT/SIGTERM. It reconciles on every restart to cover changes
made while stopped. A root replacement, watch-limit failure or inaccessible path
stops observation with a clear error; fix the cause and restart. No data is deleted.
The process logs readiness once and does not log every file event.

Set `webPort` to a nonzero port (the examples use `8721`) to enable the local
dashboard at `http://127.0.0.1:<port>/`; `0` disables it. The dashboard
receives local status through the same bounded event stream while its tab is
visible and does not initiate NAS or WAN work. Hiding the tab aborts the stream;
there is no idle polling or heartbeat. DOM text changes only when its displayed
value changes. Reconnection after errors uses 1–30 second backoff.

Pause/resume saves user intent in the local index and survives restarts. Pausing
keeps metadata observation running, so changes remain queued. Resuming does not
authorize a disabled profile: transfers require an explicitly enabled sync profile
and the live LAN gate. The dashboard reports roots, effective limits, current
sync reason and encrypted transfer counters. The installed trial reports
`automaticWrites: true`; pause still suspends its content work.
`POST /api/pause` accepts only a bounded JSON boolean with the per-process
control token and, for browser requests, an exact matching origin. Control
requests cannot change roots, exclusions or the safety gates.
Control actions obtain a fresh token, so a dashboard left open across a daemon
restart can still pause/resume. Requests and refreshes cannot overlap themselves.

State defaults to `$XDG_STATE_HOME/nas-sync/<root-id>` or
`~/.local/state/nas-sync/<root-id>`; `stateDir` overrides it. State must be outside
both sync roots. Only one daemon can open a given index at a time. Missing config
files yield printable defaults, but starting observation requires valid roots.

`-check-lan` reports local mount/interface/route evidence without pinging the NAS,
resolving DNS or accessing the mount path. Configure `nas.host` as an IP literal,
`nas.share`, `nas.interface` and `nas.prefix`. This diagnostic requires the Linux
`ip` command; missing/ambiguous evidence fails closed. It never enables writes.
Actual SMB copy/commit capabilities and OS egress enforcement still need validation.

Use `-discover-nas` first when configuring a machine:

```sh
./nas-sync -discover-nas
```

The command reads local mount metadata and lists kernel CIFS/NFS mounts plus SMB
shares exposed by the current user's GVFS session. It does not ping, resolve, list,
or write to a share. The prepared test mount is:

```text
/mnt/nas-sync-test -> //192.168.1.30/nas-sync-test (cifs, SMB 3.1.1)
```

The same session also exposes these GVFS SMB paths:

```text
/run/user/1000/gvfs/smb-share:server=satanasso.local,share=home
/run/user/1000/gvfs/smb-share:server=satanasso.local,share=public
```

Those paths are user-space FUSE mounts. They are useful for a compatibility probe,
but they are not accepted by the LAN guard for automatic synchronization because
their locking, fsync, notification and route behavior is not equivalent to a
kernel CIFS mount. The [real-NAS example](config.real-nas.example.json) records the
verified dedicated `nas-sync-test` CIFS share, NAS IP, mount point, interface and
LAN prefix. Confirm the values with `findmnt` and `ip -4 route show` if the host's
network changes before using `-check-lan`.

Run the read-only real-NAS probe first. This is the safe command for the observed
GVFS share:

```sh
python3 scripts/test_real_nas.py \
  --mount-point '/run/user/1000/gvfs/smb-share:server=satanasso.local,share=home' \
  --test-dir '/run/user/1000/gvfs/smb-share:server=satanasso.local,share=home/@Recycle'
```

The observed GVFS session has no write probe because it is inspection-only. The
read-only command uses `@Recycle` only to avoid creating anything. Do not use
`@Recycle` or another existing NAS directory as a write target. The dedicated CIFS
share has a pre-created disposable child directory; its explicit write probe is:

```sh
python3 scripts/test_real_nas.py --write \
  --mount-point '/mnt/nas-sync-test' \
  --test-dir '/mnt/nas-sync-test/nas-sync-capability-test'
```

The probe only creates uniquely named temporary files and removes them after the
write test. It measures readback, same-filesystem rename, exclusive create, fsync,
and `copy_file_range` where available. It does not qualify GVFS for automatic sync,
and it cannot measure wire offload or crash recovery. Create the disposable child
directory through the NAS client before running the write command; the application
does not create it automatically. The prepared QNAP probe passed these client-side
checks. Full protocol and egress acceptance remains open; the installed trial
uses the user's explicit approval to proceed before M2 completion.

## Resource controls

- `coalesce.idle` / `maxWait`: default 1.5 s quiet / 5 s eligibility deadline.
- `coalesce.maxPending` / `maxBytes`: 10,000 paths / 4 MiB pending path bytes.
  There can be one ready batch plus one pending batch. Overflow collapses to a
  root-rescan request. `tick` is deprecated and ignored.
- `limits.scanOpsPerSecond`: 50 paced filesystem operations/s by default, including
  directory reads. Large initial scans take longer to keep resource use low.
- `limits.maxWatches`: 100,000 maximum directories; hitting the limit reports an
  incomplete scan without inferring deletions.
- `limits.readBytesPerSecond`, `cacheBytes` and `scanRemote` are validated settings
  reserved for the future transfer/discovery engine; they do not schedule work yet.

The transfer foundations include bounded binary manifests (131,072 blocks,
8 GiB per file with the default block size), durable acknowledged bases and
pause/conflict identities, and a paced local content reader. The reader is not
connected to the observer or a NAS transport yet. It rejects symlinks, special
files, child mounts and network/FUSE roots; files changed during hashing must
be compared again. Dirty paths are indexed separately in bbolt so a future
scheduler can read pages of at most 1,000 entries without walking clean records.
Existing indexes receive a one-time local metadata migration on opening.

A standalone rolling delta codec now reconstructs from receiver-local blocks,
including content shifted by insertions or deletions. Its private staging store
exposes a completed candidate only after stream verification and file flushing;
interrupted partials require explicit recovery. These packages are tested locally
and are **not wired into the daemon**. They do not yet implement NAS transport
or automatic synchronization. See
[delta format and limits](docs/delta-format.md) and
[local resource measurements](docs/performance.md).

The separate [cooperative journal](docs/journal-protocol.md) now persists
prepared/committed batches, checks expected base versions, and tracks each
client's contiguous acknowledgement. Local restart and competing-process tests
pass. Its [native file publisher](docs/publication.md) is now integrated with
delta/staging/journal in local tests, including process termination after a
rename. Directory creation and empty-directory deletion also pass local tests;
parent/child scheduling, existing-directory adoption, conflict resolution, NAS
acceptance are unfinished. The deployed trial now uses the native transfer worker.

An experimental [authenticated transfer API](docs/transfer-api.md) now passes
local TLS push/pull and multi-file publication tests. A separate typed client now
passes local pinned-certificate, bounded-download and cancellation tests. It requires verified,
allowlisted client certificates and a caller-supplied policy gate; writes default
to disabled. The dedicated live trial now installs both helper and PC client.

The API also provides bounded, exclusion-filtered journal pages. A local durable
inbox distinguishes received changes from completed work and survives restart;
receiving metadata never marks files synchronized. This path passes local TLS
and index tests, and is scheduled by the installed trial daemon. Directory bases
now preserve their explicit type, and exact retained historical versions can be
retrieved through their committed path binding. Replay scheduling/coalescing and
server acknowledgement integration remain open.

The explicit `internal/replica` pull operation now connects the TLS downloader
to verified local staging, a local journal and native publication. Local tests
exercise two independent temporary roots, a one-byte diff update, deletion and
retry without repeat traffic. It starts no watcher or polling loop and is not
called by the installed daemon. Upload integration, inbox/base completion,
aggregate quotas and deployment acceptance are still required for autosync.

A bounded upload outbox now retains one exact proposal, observed generations and
delta lengths across restart. Its recovery helper can retrieve and save an
already committed remote result without resending payload. It cannot clear local
work until matching bases are durable. A single-pass local snapshot builder and
bounded durable delta spools now pass TLS upload tests, including preservation
of edits made after capture. An explicit dispatcher now queries remote outcome
before opening payload spools, recovers prepared publication without retransmission
and durably saves matching receipts. It checks target namespace, pause/exclusion
state and configured limits. This is library code exercised by local TLS tests;
owned partial uploads can now resume through a durable intake record. Exact
retries verify and reuse earlier candidates, while unowned artifacts are refused.
Aggregate quotas, abandoned-intake resolution and production scheduling remain
unfinished. The installed daemon still does not invoke these transfer operations.

Upload completion now verifies retained snapshots, records the confirmed versions
as local replica bases, and atomically updates observer manifests and releases the
outbox. It preserves edits made during upload and can resume between the two
databases after restart. Local TLS tests cover an upload followed by a one-byte
delta download and completion of the upload's own change-feed entry. This is still
explicit library work: directory identity adoption, observer-event reconciliation,
conflict resolution, quotas and automatic scheduling remain incomplete.

Download completion now verifies retained candidates and atomically saves their
bases together with the received-change cursor. It requires matching committed
local publication and preserves all observer records and dirty generations.
Thus an edit made after publication is not mistaken for part of the download.
An explicit bounded comparison now clears unchanged local events against the
acknowledged base without a NAS request or snapshot. Changed paths use authenticated
head/signature metadata to select push, pull or conflict; matching immutable heads
reuse the cached base manifest. The scheduler still needs to invoke these steps.
The puller now pins a configured NAS namespace in its local journal before work;
changing that target or silently assigning old unbound history is refused.

An explicit upload builder now joins comparison, immutable snapshots, local-base
delta encoding and outbox preparation for up to 128 selected paths. A durable
preparation record claims unused candidate IDs before writing snapshots; promotion
to the sendable outbox is atomic. Failed/interrupted preparation can be explicitly
aborted using those exact IDs, preserving source files, dirty generations and all
committed versions. Local TLS tests now run the builder through initial upload,
one-byte diff update and deletion. A killed-process test covers recovery after
snapshot creation. This is not yet invoked by the installed daemon: aggregate
storage quotas, full directory/history reconciliation and production acceptance
remain open. The tested worker below now orders parent/child creation.

An event-driven replica worker now runs the upload/download workflows with a real
observer in local TLS integration tests. The daemon lifecycle runner forwards only
completed metadata batches to it, cancels active work on durable pause changes,
and joins services before closing storage. It orders parent/child creation, drains
bounded dirty/inbox pages and has no idle polling or error retry loop. The CLI
now instantiates the worker when `sync.enabled` is true. Broader remote notification,
aggregate quotas, directory/history/conflict reconciliation and NAS acceptance
remain open. See [worker behavior and evidence](docs/daemon-worker.md).

A native anaNAS helper executable and privileged socket launcher now pass a
45-second disposable QNAP TLS test as the non-admin sync account. The test creates
a 512 KiB file, verifies a one-byte diff in both directions and checks deletion.
That disposable fixture is separate from the subsequently installed `~/NASdir`
live trial. See [helper configuration, commands and measured
scope](docs/helper-service.md) and the secret-free `config.helper.example.json`.

The native helper now requires a persistent publication cache budget (example:
32 MiB, 128 history entries). It refuses work before staging when the reservation
would exceed the limit, and preserves charges across failure, restart and commit.
The PC worker now also requires a cache budget, charges snapshot/wire spools before
creation and downloads before content requests, and releases failed preparation
charges only after verified abort. Completed uploads now reclaim encoded spools
before clearing their receipts, retaining snapshots for future diffs. Local
TLS/inotify and process-kill recovery tests pass. Safe historical-version
reclamation remains unfinished; this does not enable autosync. See
[accounting, scope and tests](docs/cache-budget.md).

The worker now confirms its durable completed prefix through authenticated,
policy-bound NAS acknowledgements in batches of up to 32. Received-only work cannot
advance confirmation; lost replies remain retryable across restart, with no idle
polling. Local TLS/inotify and race tests pass. This supplies client progress for
future safe history cleanup; it does not retire historical versions or enable
production transfers. [Protocol and evidence](docs/acknowledgements.md).

Authenticated remote notifications now trigger the worker in local TLS/inotify
tests, replacing the earlier manually supplied remote hint. Quiet streams have no
heartbeat or polling timer; actual changes coalesce to at most one hint per second.
Reconnect starts with a current sequence hint and replay uses the durable cursor.
The stream now also passes a disposable QNAP test with a two-second quiet window
showing no added encrypted bytes. Client and helper sockets additionally apply
and verify device binding plus `SO_DONTROUTE`; the QNAP fixture passes with both.
This is not deployed: observation of direct SMB edits, network lifecycle/egress
fault tests and the other production gates remain open. See
[socket policy and remaining validation](docs/egress-policy.md).

A kernel monitor now cancels and joins transfer sessions on relevant network
changes or uncertain notifications while preserving local observation and controls.
Each session subscribes before its LAN check. Denied policy waits for an event;
failed sessions retry with 1–60 second backoff and no healthy-idle polling.
Local TLS/inotify tests verify that edits on both sides during disconnection
converge after recovery with one observer invocation. Six isolated kernel fault
tests also pass for route/address/link loss, interface recreation, a blocking rule
and a gateway route. Those use lifecycle doubles, without content transfers.
Physical/QNAP fault acceptance, silent failure detection, UI wiring and deployment
remain pending; the installed
observer has no monitor or transfers. [Behavior and bounds](docs/network-lifecycle.md).

An isolated PC routing test now passes twelve cases, including changes during
established connections and server replies. Captured constrained traffic used
no simulated gateway, while ordinary-socket controls did. This uses temporary
network namespaces and fixed echo data; it is not production autosync or QNAP
route-failure acceptance. See the linked evidence for commands and limitations.

Local and remote exclusions both prevent automatic local tracking of the path.
Patterns support `*`, `?`, character classes and `**` as a whole path component.
Slash-free patterns match at any depth; leading `/` anchors at the root; trailing
`/` matches directories and their descendants. Negation (`!`) is rejected because
excluded directories are never traversed. `.nas-sync/` is always excluded.
Symlinks and special files are recorded as unsupported/excluded and never followed.

The example [systemd user unit](deploy/nas-sync.service) includes low CPU/I/O
priority and an optional 10% CPU quota. It is not installed automatically. The quota
covers the entire daemon; see [measurement notes](docs/performance.md) before tuning.
The [CIFS fstab example](deploy/fstab.cifs.example) is intentionally a post-validation
step because automounting can initiate network I/O when a path is touched.

## Verification

The [native probe](docs/native-probe.md) is a separate, explicit setup tool for
testing delta reconstruction and publication on the NAS itself. It defaults to
read-only inspection; write mode is restricted to a new child of a pre-existing
disposable test directory. It does not start synchronization or change the daemon.

The native probe passed on the QNAP on 2026-09-09, including all 11 checks as
`nas-sync-test` (UID 1000/GID 100). The 512 KiB fixture plus one-byte insertion used
0.41039 seconds of process CPU and 7.8 MiB peak RSS over 4.72 seconds. These are
small native-fixture measurements, not sustained autosync or network-isolation
acceptance. [Recorded evidence](docs/evidence/qnap-native-probe-2026-09-09.json).

```sh
go test ./...
go test -race ./...
go vet ./...
go test ./internal/hash -run '^$' -bench . -benchmem
python3 scripts/measure_local.py --binary ./nas-sync --idle-seconds 30
python3 scripts/check_no_network.py --binary ./nas-sync
python3 -m unittest discover -s scripts -p 'test_analyze_copy_capture.py'
```

Tests include real filesystem events, restart recovery, bounded queues, cancellation,
exclusions, directory moves, failed scans and pure LAN guard decisions. Real NAS
acceptance is tracked separately in [docs/e2e-qnap.md](docs/e2e-qnap.md).
