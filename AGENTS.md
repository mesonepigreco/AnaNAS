# Agent instructions for `nas-sync`

## Read first

Read these files before changing behavior:

1. `PROJECT.md` — requirements and hard constraints. It is the source of truth.
2. `PLAN.md` — implementation plan, resource budgets, acceptance gates and known gaps.
3. `README.md` — supported commands and local usage.
4. `docs/e2e-qnap.md` — real-NAS setup and capability-test status.
5. `docs/performance.md` — measured local resource behavior and its limits.

Keep those documents consistent with the code. When a fact is measured on a host or
NAS, record the command, scope and limitation rather than presenting it as a general
guarantee.

## Project state

The implemented program is a lightweight local metadata observer. It uses recursive
inotify watches, a persistent bbolt index, bounded event coalescing and paced scans.
It does not yet implement bidirectional content synchronization, NAS-side diff
materialization, the web UI, or SSH/SFTP actions. `automaticWrites` must remain false
until the M2 protocol and egress gates are complete.

The prepared test NAS is currently:

```text
NAS:          //192.168.1.30/nas-sync-test
mount point:  /mnt/nas-sync-test
client:       192.168.1.17/24 on eno1
route:        192.168.1.0/24 dev eno1 (direct)
filesystem:   kernel CIFS, SMB 3.1.1, cache=strict, serverino, actimeo=1
test child:   /mnt/nas-sync-test/nas-sync-capability-test
```

The disposable client-side probe has passed. It does not prove server-side
`copy_file_range` offload, durability, crash recovery, multi-client fencing, or WAN
egress safety. The live mount reports `soft`; include its timeout behavior in the
pending failure-recovery tests. GVFS paths under `/run/user/1000/gvfs` are
inspection-only and must never be treated as automatic-sync mounts.

## Non-negotiable design constraints

- Automatic synchronization is LAN-only. Use a configured private IP literal,
  connected physical interface, direct route and matching kernel CIFS mount. Do not
  resolve DNS or use a gateway/VPN for background synchronization.
- Minimize CPU, disk and network traffic. Do not add idle polling, repeated whole-tree
  scans, per-file goroutines, unbounded queues, or content reads to metadata-only code.
  Coalesce events, pace scans and transfers, and keep caches bounded.
- Preserve safety on uncertainty. Missing/inaccessible data must not become deletes;
  ambiguous mounts, routes, protocols and read-only mounts must fail closed.
- Exclusions are applied before watching, listing, hashing or transferring. Never
  traverse symlinks or special files as regular content. Keep state and caches outside
  both sync roots; `.nas-sync/` is always excluded.
- Do not implement full-file transfer as an invisible substitute for diff sync. Any
  fallback that materially increases traffic needs an explicit, bounded operation and
  an estimate.
- Do not claim server-side offload from a successful local copy syscall. Validate with
  wire counters and NAS-side behavior.

## NAS and credential safety

- Never read, print, commit or copy `/etc/nas-sync-test.cred` or any NAS password.
- Never recursively search or hash a GVFS/CIFS mount. Every directory read can create
  NAS traffic. Inspect only the exact disposable path needed by a test.
- Never write to `home`, `public`, `@Recycle`, the mount root, or an existing user
  directory. Write tests require an explicitly pre-created disposable child and the
  `--write` flag:

  ```sh
  python3 scripts/test_real_nas.py --write \
    --mount-point /mnt/nas-sync-test \
    --test-dir /mnt/nas-sync-test/nas-sync-capability-test
  ```

- The probe cleans up its uniquely named temporary files. Use `--keep` only when a
  failure investigation specifically requires artifacts.
- Keep the mount manual until M2 validation is complete. Do not install the fstab
  example or enable automounting casually: touching an automounted path can initiate
  network I/O.
- A privileged host operation may be needed for `ip`, `findmnt`, CIFS mounting or
  packet capture. Request the narrowest escalation, explain why it is read-only or
  directory-scoped, and do not use it to inspect credentials. The restricted Codex
  namespace may show the host CIFS mount as read-only and cannot read netlink state;
  distinguish that from the host namespace before changing the mount.

## Useful commands

Development assumes Linux and Go 1.27 or newer.

Build and local validation:

```sh
GOCACHE=/tmp/nas-sync-go-cache GOMODCACHE=/tmp/nas-sync-go-mod-cache go test ./...
GOCACHE=/tmp/nas-sync-go-cache GOMODCACHE=/tmp/nas-sync-go-mod-cache go test -race ./...
GOCACHE=/tmp/nas-sync-go-cache GOMODCACHE=/tmp/nas-sync-go-mod-cache go vet ./...
GOCACHE=/tmp/nas-sync-go-cache GOMODCACHE=/tmp/nas-sync-go-mod-cache go build ./...
python3 -m py_compile scripts/*.py
git diff --check
```

Use temporary Go caches because the shared module/build caches may be read-only. If
modules are missing, download only the pinned dependencies needed for these checks;
do not add dependencies without updating `go.mod`, `go.sum` and the plan.

Local observer and LAN diagnostics:

```sh
./nas-sync -config config.json -scan-once
./nas-sync -config config.json
./nas-sync -discover-nas
./nas-sync -config config.real-nas.example.json -check-lan
```

`-scan-once` and normal observer mode are local metadata operations. They must not
contact the NAS. `-discover-nas` reads mount metadata only. `-check-lan` performs
local route/interface/mount checks and must not ping, resolve DNS or stat an
automount path.

Real-NAS checks:

```sh
findmnt -T /mnt/nas-sync-test -o TARGET,SOURCE,FSTYPE,OPTIONS
python3 scripts/test_real_nas.py \
  --mount-point /mnt/nas-sync-test \
  --test-dir /mnt/nas-sync-test/nas-sync-capability-test
```

Run the read-only probe first. Run the write probe only against the disposable child
after verifying the host-side mount is `rw` and the test directory is correct.

## Code and configuration conventions

- Keep packages small and responsibilities separate: configuration validation,
  exclusions, hashing, coalescing, indexing, observation and LAN evidence are distinct
  areas. Prefer pure functions for policy decisions and table-driven tests for them.
- Use cancellation-aware I/O and bounded buffers. A slow consumer must not cause an
  unbounded producer queue. Overflow should request a root reconciliation rather than
  infer deletes from incomplete data.
- The observer records metadata only. Do not read file contents or hash on every event;
  content hashing belongs to a future bounded transfer scheduler.
- Update `config.example.json` whenever a configuration field becomes implemented.
  Keep real credentials outside the repository and keep `config.real-nas.example.json`
  free of secrets.
- When changing resource behavior, update `README.md`, `PLAN.md` and measurements or
  acceptance notes together. State defaults and effective limits explicitly.
- Use `apply_patch` for edits. Avoid destructive cleanup and never reset or discard
  unrelated working-tree changes. Remove generated `__pycache__` files before finishing.
- Do not run a long-lived daemon, install a systemd unit, alter fstab, or modify NAS
  data as a side effect of a test unless the user explicitly asks for that operation.

## M2 boundary

M2 is still open even though the client-side CIFS setup passed. Before marking it
complete, record evidence for:

- packet-level traffic and whether same-volume copy is actually server-side;
- metadata and data durability, atomic publication and crash recovery;
- disconnect, timeout, route loss, VPN/IPv6 and multichannel behavior;
- coordination and fencing across independent clients/protocols;
- OS/network egress rules that prevent automatic WAN traffic;
- the supported native mode or the bounded NAS-side-helper decision.

Until those checks pass, keep synchronization read-only/disabled and describe the
limitation in the plan and documentation.
