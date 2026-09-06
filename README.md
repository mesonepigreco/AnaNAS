# nas-sync

A Go synchronizer in development, guided by [PROJECT.md](PROJECT.md) and
[PLAN.md](PLAN.md). The implemented daemon currently **observes local metadata**.
It does not yet sync NAS content or connect through SSH/SFTP. An optional read-only
status UI binds to loopback when `webPort` is nonzero; it has no external assets.

The local observer uses recursive inotify watches, a persistent bbolt index,
bounded event coalescing and paced scans. It never reads file contents. Idle
observation does not poll the filesystem or contact the network. Excluded folders
are skipped before traversal; failed scans cannot turn unavailable data into deletes.

## Build and run

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
read-only dashboard at `http://127.0.0.1:<port>/`; `0` disables it. The dashboard
fetches only local status while its tab is visible and does not initiate NAS or WAN
work.

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
checks; automatic synchronization remains disabled until the protocol and egress
gates in [docs/e2e-qnap.md](docs/e2e-qnap.md) pass.

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

```sh
go test ./...
go test -race ./...
go vet ./...
go test ./internal/hash -run '^$' -bench . -benchmem
python3 scripts/measure_local.py --binary ./nas-sync --idle-seconds 30
python3 scripts/check_no_network.py --binary ./nas-sync
```

Tests include real filesystem events, restart recovery, bounded queues, cancellation,
exclusions, directory moves, failed scans and pure LAN guard decisions. Real NAS
acceptance is tracked separately in [docs/e2e-qnap.md](docs/e2e-qnap.md).
