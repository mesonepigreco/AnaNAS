# QNAP capability validation — CIFS mount ready; protocol gate pending

**Startup update:** automatic LAN mounting and boot daemon startup are installed.
A normal remount used saved credentials, followed by successful file sync and
cleanup. [Startup evidence](startup.md) supersedes earlier manual-mount and
before-login limitations. A 2026-09-10 PC reboot subsequently started the mount,
daemon and panel; exact file sync and cleanup passed in both directions. A NAS
reboot was not performed.

**Current deployment, 2026-09-09:** the user explicitly approved activating the
LAN-only trial before M2 completion. The dedicated `Nasdir` helper and PC worker
are installed with endpoint firewall rules and native NAS regular-file capture.
A 256 KiB file, a one-byte edit in each direction, five-second pause and service
restarts passed on the real target. The subsequent folder update also passed
native NAS nested-file and empty-directory creation followed by a PC edit back.
The next installed update passed deletion of one observed native NAS file,
PC re-creation, and PC deletion back to the NAS, retaining the empty test child.
[Live evidence and limitations](live-trial.md)
supersede the disabled-deployment statements in the earlier checkpoints below.
M2 remains open; NAS reboot, power-loss, conflict and full directory reconciliation
acceptance are not claimed. The user removed the obsolete M2-before-sync rule
and explicitly requested permanent NAS SSH access; older instructions below
requiring disabled writes or SSH shutdown are historical.

The native helper now requires publication cache limits, with 32 MiB/128 entries
in the disposable runner. Local budget refusal, restart and killed-process tests
pass; the QNAP evidence below predates this change. PC cache admission and explicit
failed-preparation cleanup now also pass local TLS/inotify and killed-process
tests. QNAP budget validation and safe retained-version reclamation remain pending. See
[current storage scope](cache-budget.md). Production autosync remains disabled.

PC completion now also reclaims encoded spools while retaining snapshots. Local
TLS/inotify and process-kill tests verify receipt-based completion after cleanup,
without resending data or crediting twice. This is local PC evidence, not a new
QNAP cleanup/recovery test; historical version reclamation remains unfinished.

Authenticated policy-bound client acknowledgements now also pass local TLS/inotify,
restart/retry and range-validation tests. The NAS cursor matches the local durable
completed prefix in the worker fixture, without idle ACK traffic. The recorded
QNAP runs below predate this endpoint, and historical cleanup is still disabled.
[Acknowledgement scope](acknowledgements.md) describes the remaining acceptance.

The PC worker's kernel notification monitor now passes local subscription,
cancellation and TLS/inotify lifecycle tests. Session recovery preserves the
observer and local controls; the local fixture verifies replay of edits on both
sides during a synthetic offline interval. Failed sessions use 1–60 second
backoff; denied LAN policy waits for kernel events without polling. These changes
are not deployed. The QNAP helper does not use this
new monitor and the earlier routing capture predates it. A subsequent six-case
isolated PC kernel fault test passes with the real monitor/supervisor and lifecycle
doubles; it does not transfer content. Physical event injection and QNAP
cancellation acceptance remain pending; no host network or NAS data was modified
for these tests. [Scope](network-lifecycle.md).

After the PC cache changes, `curl --fail --silent --max-time 3
http://127.0.0.1:8721/api/status` on the host still reported local observation,
`ready: true`, `scanning: false`, `automaticWrites: false`, and zero upload/download
bytes for `/home/darth-vader/NASdir`. This confirms the deployed observer's state;
it does not test NAS connectivity or demonstrate synchronization.

## Native authenticated helper test passed on 2026-09-09

The subsequent 45-second disposable helper test passed native preflight, pinned
mutual TLS state, directory/file publication, one-byte diff upload and verified
download, explicit deletion and contiguous journal checks. It ran as UID 1000/GID
100 behind a device-bound listener on `192.168.1.30:8742`/`eth0`, with the PC source
bound to `192.168.1.17`/`eno1`. Each update reused 512 KiB and transferred one
literal byte in a 491-byte encoded delta. The fixed write probe took 4.20377939
seconds. This is the first real native helper transport fixture; it does not enable
the installed daemon's automatic writes.

The runner reported successful helper exit and cleanup. Independent exact-path
readback confirmed its new child absent and the test-parent/production modes and
owners unchanged. No existing user files, production service or egress rules changed.
[Commands, architecture and limits](helper-service.md),
[raw results](evidence/qnap-helper-test-2026-09-09.json) and
[resource scope](performance.md) record the evidence. M2 still requires OS egress,
actual failure/multi-client tests, quota/retention, external-change observation and
sustained acceptance. The earlier sections below retain their original test scope.

Subsequent notification-stream changes now pass both local TLS/inotify worker
tests and an expanded disposable QNAP fixture. The final QNAP run additionally
checks device binding and `SO_DONTROUTE` on outgoing, inherited and accepted TCP
sockets. It passed three read-only checks and ten write checks, including an
authenticated committed-change hint while transfers remain available. The
2.001152511-second quiet window added zero encrypted bytes. Update reuse remains
512 KiB plus one literal byte in each direction; encoded delta is 491 bytes.

The 45-second helper exited as UID 1000/GID 100, reporting 0.160243 seconds user
CPU, 0.085463 seconds system CPU and 11,124,736 bytes peak RSS. Cleanup and unchanged
test-parent/production owners were independently verified. [Commands and scope](helper-service.md)
and [binary-pinned final results](evidence/qnap-helper-direct-socket-test-2026-09-09.json)
record this test. Earlier notification-only [results](evidence/qnap-helper-events-test-2026-09-09.json)
are retained separately. No production installation, autosync or route-failure
packet test is claimed. [Egress policy validation](egress-policy.md) remains open.

The later isolated **PC-kernel** routing test now passes twelve gateway/default/
priority/established-connection cases with packet capture and zero reported drops.
It does not run on the QNAP kernel or touch its routes. The QNAP route-failure
gate remains open; [scope and raw evidence](egress-policy.md) distinguish these
results from the real-NAS direct-connection fixture above.

## Native QNAP probe passed on 2026-09-09

After the earlier login processes ended, a new visible setup terminal established
the authenticated master at `/tmp/ananas-admin-session-vyxJ2VTA/control`.
`python3 scripts/run_native_probe.py --session-dir /tmp/ananas-admin-session-vyxJ2VTA
--binary /tmp/ananas-native-probe-arm64` passed read-only preflight for
`/share/CACHEDEV1_DATA/nas-sync-test/nas-sync-capability-test`. The same command
with `--write` passed all 11 native checks on Linux 4.2.8/aarch64.

The dedicated account reports UID 1000/GID 100. Repeating read-only preflight
and the write probe with `--as-sync-user` passed all checks in the foreground as
that account. Process CPU was 0.269685 seconds user plus 0.140705 seconds system;
peak RSS was 8,175,616 bytes, elapsed probe time 4.724918849 seconds. The one-byte
update reused 512 KiB and serialized to 491 delta bytes plus a 344-byte signature.
These are native fixture sizes, not transport packet counters. Cleanup reported
success, and the exact uploaded tool child was independently confirmed removed.

Automatic review initially rejected the non-admin invocation because it inferred
an ownership change to the existing test parent. An explicit immediate-child path
validator and exact-argument tests proved the chown targets only a new tool child
and its executable; re-review accepted the operation. Readback confirmed unchanged
mode/UID/GID for the production directory (`777/0/0`) and test parent (`777/1000/100`).
No existing user data, production helper, daemon transfer mode or egress rule changed.
[Commands and evidence](evidence/qnap-native-probe-2026-09-09.json) record the scope.
At that stage M2 remained open for authenticated network transport, egress enforcement, actual
disconnect/process-kill/power-loss, independent clients and sustained resources.

## Deployment preparation on 2026-09-08

The requested production pair is `/home/darth-vader/NASdir` and the dedicated
share `//192.168.1.30/Nasdir`. The local directory was created with
`install -d -m 0700 /home/darth-vader/NASdir`. The user approved share access for
the existing non-admin `nas-sync-test` account and admin (read/write), with guest
access denied. The QNAP creation wizard completed and its share list now shows
`Nasdir` on DataVol1. The manual mount at `/mnt/nasdir` is active.
The systemd user daemon is installed and running, but only local metadata
observation is enabled. Its dashboard reports `automaticWrites: false`.
Rechecked on 2026-09-09 with `curl --fail --silent --max-time 5
http://127.0.0.1:8721/api/status`: the live daemon still reports local observation,
zero upload/download bytes and transfers disabled. Both user services report
active; the indicator's D-Bus properties report `anaNAS`, `ananas-symbolic` and
`anaNAS — transfers disabled`. These checks verify installed status/branding,
not end-to-end synchronization. Files placed in `~/NASdir` remain local.
The subsequent change-feed/inbox tests also use temporary PC databases and
loopback TLS only. They add no NAS acceptance evidence or production writes.
Directory-base and historical-version tests likewise run on PC fixtures only;
neither change has been deployed into the observer or executed on the NAS.
The explicit replica-pull integration now also passes between two temporary PC
roots over loopback TLS, including diff update and publication. It is not wired
into the installed daemon and adds no real-NAS synchronization evidence.
Upload-outbox receipt recovery is also verified with a temporary PC index and
loopback TLS after deliberately omitting a local acknowledgement. This does not
establish behavior under a real NAS disconnect or enable upload dispatch.
The subsequent snapshot-to-upload test uses local native source/state directories
and a loopback TLS server. It verifies stable snapshots, durable delta spools and
preservation of later edits, but adds no QNAP execution or wire-capture evidence.
The explicit upload dispatcher is also exercised over loopback TLS, including
recovery from committed or prepared remote operations after local index restart
with no local payload spools. These are controlled PC failures, not QNAP timeout,
power-loss or disconnect tests. A subsequent live loopback status check still
reported `automaticWrites: false` and zero transfer totals; both services remained
active and the indicator reported `ananas-symbolic`. The existing visible NAS
login processes were running, but their SSH control socket was not authenticated.
Durable upload intake now also passes local killed-receiver and truncated TLS
batch-retry tests. They establish PC-side ownership/reuse behavior only; no
intake recovery has run on the QNAP. The same existing login processes were
rechecked live, with the control socket still unavailable; no new login was started.
Upload completion and later pull now also pass on local TLS fixtures: confirmed
snapshot versions become local logical bases, a subsequent one-byte update is
reconstructed by delta, and newer local edits are preserved. Restart between the
local journal/index commits and own-upload feed completion are tested locally.
The killed-intake fixture was corrected to remain alive in non-race runs as well
as race runs. These checks add no QNAP execution or production autosync evidence.
Download completion now also passes local TLS create/update/delete tests and
local restart, corruption, directory and excluded-batch cases. Its local origin
binding is tested across reopen. These changes still run only on PC fixtures;
they neither deploy the transfer engine nor satisfy the real-NAS acceptance gate.
Qsync integration was left disabled in the share
wizard. Its oplocks, previous versions and recycle-bin defaults were retained.
The optional immediate disk-sync setting was unchecked; durable SMB publication
remains a validation gate and must not be inferred from default share creation.
The requested-disk-flush option was subsequently enabled for `Nasdir` following
[QNAP's documented behavior](https://docs.qnap.com/operating-system/qts/5.2.x/en-us/creating-a-shared-folder-961E8DD8.html).
The user approved QNAP's warning that applying it temporarily suspends all NAS
services. The save completed, and reopening the share properties confirmed the
checkbox remained checked after the form finished loading. `findmnt --mountpoint
/mnt/nasdir -o TARGET,SOURCE,FSTYPE,VFS-OPTIONS` still reported the expected `rw`
CIFS mount. The read-only probe against the existing disposable child under
`/mnt/nas-sync-test` passed, and `systemctl --user is-active nas-sync.service`
reported `active`. These checks establish setting persistence and client
readability after this service interruption, not power-loss durability.

The Telnet/SSH panel initially showed SSH disabled (port 22), with logins limited to
administrators. The sync account was not promoted to administrator. A bounded
helper deployment therefore still needs an explicitly scoped installation/access
method; web login alone supplies no SSH credentials.
The prepared `scripts/open_nas_admin_session.py` opens a temporary user-entered
SSH control session, checking the direct route and binding to `eno1` with no
proxy configuration or forwarding. `scripts/inspect_nas_runtime.sh` contains
only runtime/tool and exact `/share/Nasdir` metadata inspection. Automatic approval review initially rejected enabling SSH because
the prior share-setup authorization did not cover expanded administrator access.
The user subsequently explicitly approved temporary port-22 SSH setup and later
disablement. Enabling SSH through the QNAP panel returned “Cambiamenti applicati.”
A normal Ptyxis desktop window titled “NAS setup — enter password here” was
opened with the login script and a new private `/tmp/nas-sync-admin-session-*`
directory. Password entry goes directly to SSH in that terminal, without tool
capture. `ssh -F /dev/null -S SESSION/control -O check admin@192.168.1.30`
confirmed the authenticated master. Running the prepared inspection with
`ssh -F /dev/null -S SESSION/control -o BatchMode=yes -o ProxyCommand=false
admin@192.168.1.30 sh -s < scripts/inspect_nas_runtime.sh` reported:

- Linux 4.2.8, aarch64; `uid=0(admin) gid=0(administrators)`.
- `/share/Nasdir` resolves to `CACHEDEV1_DATA/Nasdir` on `/dev/mapper/cachedev1`.
- `rsync --version` reports 3.0.7, protocol 30. Shell, rsync, sha256sum and sync
  are on PATH; flock, python3, su and setpriv were not found on PATH.
- The exact existing `/share/nas-sync-test/nas-sync-capability-test` child exists.

These commands inspected metadata/tool versions only; no content or credentials
were read, no NAS files were written and no helper was installed. The PC's
`openat2` content API requires a newer kernel, so it cannot simply be copied to
this NAS. The admin session is for setup, never the automatic runtime identity.
After setup
and validation, close the temporary SSH control session and disable the NAS SSH
service again. The sync account remains non-admin.

The manual production mount was created using a desktop administrator prompt:

```sh
pkexec /usr/bin/mount --mkdir=0755 -t cifs //192.168.1.30/Nasdir /mnt/nasdir \
  -o credentials=/etc/nas-sync-test.cred,vers=3.1.1,seal,cache=strict,serverino,actimeo=1,uid=1000,gid=1000,file_mode=0600,dir_mode=0700,nosuid,nodev,noexec
findmnt --mountpoint /mnt/nasdir -o TARGET,SOURCE,FSTYPE,VFS-OPTIONS,FS-OPTIONS
ip -j -4 route get 192.168.1.30
```

The result is writable kernel CIFS, SMB 3.1.1, encryption (`seal`), and the
configured `eno1` route without a gateway. The mount also reports `soft`.
No fstab or automount configuration was changed. Mount setup alone does not
enable automatic synchronization. Credentials were used only by the mount
utility; their contents were not exposed.

The signed-in QNAP control panel reports TS-128A, QTS 5.2.6.3195, Realtek RTD1295
quad-core ARM Cortex-A53 at 1.4 GHz and 982 MB RAM. These are UI-reported facts,
not NAS-side performance measurements. No passwords were read or copied.

Host-namespace checks used `findmnt --mountpoint /mnt/nas-sync-test -o
TARGET,SOURCE,FSTYPE,VFS-OPTIONS,FS-OPTIONS`, `ip -j -4 route show` and
`ip -j -4 address show dev eno1`. The test mount is `rw`; the client address
and direct route still match the values below. The restricted namespace shows
`ro` and denies netlink. `systemctl --user is-active nas-sync.service` returned
`inactive`; `sudo -n true` requires interactive authentication. A later desktop
authentication enabled the narrow packet capture below. Egress policy remains pending.

## Small same-share copy packet measurement (2026-09-08)

The existing disposable CIFS child passed a fresh read-only probe, followed by
the explicit write probe. `scripts/test_real_nas.py` now records nanosecond
start/end times for source/staging opens, `copy_file_range`, destination fsync
and closes; the later copied-content readback is outside that interval.

```sh
pkexec /usr/bin/timeout --signal=INT 20s /usr/bin/tcpdump \
  -i eno1 -nn -U -Z darth-vader --time-stamp-precision=nano -s 160 \
  -w /tmp/nas-sync-wire-R71OOL/copy.pcap 'host 192.168.1.30 and tcp port 445'
# Once tcpdump reports that it is listening, in a second terminal:
python3 scripts/test_real_nas.py --write --mount-point /mnt/nas-sync-test \
  --test-dir /mnt/nas-sync-test/nas-sync-capability-test --json \
  > /tmp/nas-sync-wire-R71OOL/probe.json
python3 scripts/analyze_copy_capture.py --pcap /tmp/nas-sync-wire-R71OOL/copy.pcap \
  --probe-json /tmp/nas-sync-wire-R71OOL/probe.json \
  --client-ip 192.168.1.17 --nas-ip 192.168.1.30
```

The `/tmp` directory above was created uniquely for this measurement; create a
new private temporary directory for a repeat. The capture stopped automatically
after 20 seconds: 344 packets captured, 344 received by the filter, zero kernel
drops. Only 8 packets fell within the copy interval:

| Direction | Packets | Original captured frame bytes | TCP payload bytes |
|---|---:|---:|---:|
| PC → NAS | 4 | 1,121 | 857 |
| NAS → PC | 4 | 932 | 668 |
| Total | 8 | 2,053 | 1,525 |

The source/copy payload was 106,593 bytes. Both digests matched. CIFS counters
increased by 2 successful IOCTLs and 4 SMB operations, with zero payload read,
payload write, read-operation and write-operation deltas. The interval was
`1788898694918855052`–`1788898694921793653` Unix nanoseconds (~2.94 ms).
The probe removed its temporary NAS files.

Together the syscall, counters and capture support server-side copying without
a client payload round trip for this small same-share copy. TCP lengths include
SMB/encryption overhead and retransmissions. Host capture frame lengths are not
physical Ethernet line bytes: NIC offloads, framing and capture boundaries limit
that interpretation. The capture does not establish arbitrary-range offload,
1 GiB edit/append/insertion budgets, server durability, fencing or WAN safety.
The raw capture is removed after recording these totals; no payload is committed.

## Local daemon deployment (2026-09-08)

After checking that the destination files did not already exist, the built binary
was installed at `~/.local/bin/nas-sync`, the Nasdir profile at
`~/.config/nas-sync/config.json` (0600), and `deploy/nas-sync.service` at
`~/.config/systemd/user/nas-sync.service`. Deployment commands included
`systemctl --user daemon-reload` and `systemctl --user enable --now nas-sync.service`.

`systemctl --user show nas-sync.service -p ActiveState -p SubState -p
UnitFileState -p CPUQuotaPerSecUSec -p MemoryHigh -p MemoryMax` reported
`active`, `running`, `enabled`, `100ms`, `134217728` and `268435456`, respectively.
The loopback status at `http://127.0.0.1:8721/api/status` reported the expected
local/NAS roots, readiness for local observation and `automaticWrites: false`.
Chrome also displayed the installed dashboard. No transfer was initiated.

The service observes `~/NASdir`, with state at `~/.local/state/nas-sync/nasdir`.
It runs under the user manager; before-login boot operation is not configured.
The test and production NAS mounts remain manual. This is deployment of the
observer and controls, not acceptance of real content synchronization.

The later panel deployment installed `scripts/nas_sync_indicator.py` as
`~/.local/bin/nas-sync-indicator` and `deploy/nas-sync-indicator.service` under
`~/.config/systemd/user/`, then used `systemctl --user daemon-reload` and
`systemctl --user enable --now nas-sync-indicator.service`. The updated Go binary
was installed through a sibling temporary file and rename, followed by a daemon
restart. Both services report active/running. No new system package was needed.

`org.kde.StatusNotifierWatcher.RegisteredStatusNotifierItems` included the new
`/StatusNotifierItem`. Calling `com.canonical.dbusmenu.GetLayout 0 1 '[]'` on
`org.kde.StatusNotifierItem.nas_sync` showed the expected menu. Its pause action
changed the dashboard to paused, persisted across a daemon restart and propagated
back through the reconnected stream. Resuming in Chrome updated the panel to
transfers disabled with `paused: false`, always retaining `automaticWrites: false`.
That test exposed a stale browser control token after restart; explicit controls
now refresh the token, and the same open-tab/restart test passed. These are live
protocol/menu checks, not a desktop screenshot or completed sync acceptance.

## Transfer core status (2026-09-09)

The rolling delta codec and private native-filesystem staging store now pass
local reconstruction and interrupted-receiver tests. Neither is connected to
the installed daemon or deployed as a NAS helper. No end-to-end automatic push
or pull has been tested. The small CIFS copy measurement below remains separate
evidence, not validation of this new codec on the NAS. NAS publication, durable
journals, client fencing, failure recovery and OS egress gates remain open;
`automaticWrites` remains false. See [delta format](delta-format.md).

The subsequent cooperative-journal implementation passes local tests for
exclusive process ownership, durable prepared work after process termination,
recovery with a new epoch, expected-base conflicts and contiguous cursors.
These tests use a simulated publisher and the PC's native filesystem. They
do not validate QNAP locks/flushes, actual content publication, SMB/SFTP fencing,
or disconnected clients. No NAS helper or automatic transport is deployed.
See [journal protocol and outstanding integration](journal-protocol.md).

Native filesystem publication has now been tested locally through the real
delta/staging/journal path, including creates, replacements, explicit deletes,
retained versions, partial batches and process termination after a rename.
A 512 KiB one-byte insertion test reuses all old bytes through a delta stream
smaller than 1 KiB, then verifies the published file and client cursor. This is
an in-memory/local-filesystem integration test, not network traffic evidence.
An ARM64 test binary compiles; it has not run on the NAS. `renameat2` flags,
`fdinfo` mount identities, native flushes and resource use still need QNAP
capability tests in the disposable directory. No NAS data changed in these tests.

The native probe executable and scoped setup runner are now prepared; see
[native probe instructions](native-probe.md). PC command-line write execution
passed in 2.44 seconds, including cleanup, using a 512 KiB fixture, a one-byte
insertion, 491 delta-stream bytes and 344 signature bytes. It verifies native
exchange/delete retention and a controlled interruption after durable file
publication but before journal commit. This run was on amd64 PC storage, not
the NAS, and does not simulate power loss or a process kill.

The earlier authenticated SSH master was confirmed missing in the host namespace.
A new visible Ptyxis login was opened on 2026-09-09. The specific login process
remained live, but its authenticated control socket was not available at the
last check at that stage. No executable upload or native NAS probe had occurred
then; the later native results are recorded above. Do not
restart an apparently waiting login solely because the control socket is not
ready; verify its process/session state first. Passwords remain user-entered
directly into the terminal, never through a tool response.

The native probe now additionally creates the fixture's parent through the
journal and removes it after the file tombstone, checking five contiguous
commits. Local race tests pass for directory publication/recovery and atomic
refusal of nonempty directories, including a racing or excluded child. The
ARM64 executable is rebuilt with these checks. This extends the prepared test;
it does not constitute a NAS run or change the installed observer's write gate.

The subsequent `internal/transferapi` work adds real local TLS 1.3 push/pull
tests between an authenticated client and the native publisher, including
one-byte diff reuse, multi-file batches and recovery. Certificates and both
roots are temporary PC fixtures. The transfer API has not been deployed to the
NAS; no certificate trust or production listener configuration has changed.
Its supplied gate callback and connection wrapper are not evidence of enforced
LAN-only egress. That deployment remains pending; the later scoped native NAS
probe passed as recorded above.

## Test mount and earlier evidence

The anaNAS branding was installed across the 2026-09-08/09 session: the panel
exports `Id=anaNAS`, `IconName=ananas-symbolic`, an owned local icon-theme path
and a state overlay; both user-service descriptions use anaNAS. The bundled
SVGs were rendered locally and visually inspected. A desktop entry and command
alias were added. Existing service and state names were retained. The dashboard's
local HTTP response includes the anaNAS heading and embedded favicon route.
The Chrome connection was unavailable after the graphical session changed, so
this final branding check used HTTP and the live StatusNotifierItem properties,
not a new browser screenshot. Automatic writes still report false.

The prepared host now has a kernel CIFS mount for the dedicated test share:

```text
NAS:          //192.168.1.30/nas-sync-test
mount point:  /mnt/nas-sync-test
client:       192.168.1.17/24 on eno1
route:        192.168.1.0/24 dev eno1 (direct)
filesystem:   cifs, SMB 3.1.1, cache=strict, serverino, actimeo=1
client kernel: Linux 7.0.0-30-generic x86_64
smbclient:    4.23.6-Ubuntu
```

The live mount also reports `soft`; its timeout and route-loss behavior must be
included in the pending failure-recovery tests before production synchronization
is enabled.

`mount.cifs` and `smbclient` are installed. The explicit disposable write probe
passed: readback, same-filesystem rename, exclusive create, `fsync` and
`copy_file_range` all succeeded. The probe cleaned up its temporary objects. This
validates the client-visible CIFS behavior, not server-side offload, crash
durability, multi-client fencing or WAN egress policy.

Kernel CIFS debug data reports client CIFS version 2.59, dialect `0x311` (SMB
3.1.1), negotiated AES-128-GCM encryption and server capabilities `0x300047`.
The SMB multichannel capability bit is absent from that mask, and the active
session has one channel. CIFS statistics sampled immediately around a 106,593-byte
`copy_file_range` showed two successful IOCTLs with zero changes to payload read,
payload write, read-operation or write-operation counters. This is strong client-side
evidence of an SMB server-side copy path; it is not packet-level proof. One failed
IOCTL exists in the cumulative counters from earlier operations, but none failed in
the measured copy delta. The same cumulative counters report 21 session and 42
share reconnects since `2026-09-06 12:53:24 UTC`; their cause is not isolated yet,
so route/timeout stability remains an M2 blocker.

The current session exposes two SMB shares through the user's GVFS FUSE mount:

```text
server: satanasso.local
shares: home, public
paths:
  /run/user/1000/gvfs/smb-share:server=satanasso.local,share=home
  /run/user/1000/gvfs/smb-share:server=satanasso.local,share=public
```

The same user session also exposes `home` and `public` through GVFS/FUSE. Those
paths remain inspection/test-only. The CIFS mount is the only path suitable for
the real-NAS capability tests. No NAS content outside the disposable probe
directory was modified.

Run `nas-sync -discover-nas` to repeat mount discovery. Use
`scripts/test_real_nas.py` with a pre-created disposable child directory for the
read-only or explicitly enabled compatibility checks. The write probe is scoped
to that directory but still requires user authorization through `--write`.

The earlier read-only probe against `home/@Recycle` confirmed readability and
reported `fuse.gvfsd-fuse`; no files were written or removed. The dedicated CIFS
read-only probe also passed before the write phase.

The real-NAS example now records the verified values `192.168.1.30`,
`192.168.1.0/24`, `eno1` and `/mnt/nas-sync-test`. `-check-lan` must still be run
from the host namespace because the restricted development process cannot read
the host's netlink state; the host-side route and address checks passed. The guard
also rejects a CIFS mount whose effective mount flags include `ro`.

Before M2 can pass, record the actual PC/kernel, QNAP firmware, SMB/NFS/SFTP
versions, mount options, LAN interface/prefix and configured OS egress policy.
Use a disposable directory on the share for all destructive/fault-injection tests.

| Required experiment | Status |
|---|---|
| Kernel CIFS mount, direct LAN route and disposable read/write probe | Passed on `//192.168.1.30/nas-sync-test`; host-side result recorded above |
| SMB dialect/encryption/multichannel capability check | Passed: SMB 3.1.1, AES-128-GCM, one channel; multichannel capability absent |
| Small same-share server-side copy/offload | Passed scoped packet/counter check above: 106,593 copied bytes, 1,525 TCP payload bytes, 2 successful IOCTLs |
| Client flush/readback and same-volume publication checks | Passed for the disposable probe; power-loss/server-crash durability remains pending |
| Route loss, VPN change, IPv6 and multichannel cannot send automatic WAN traffic | Pending dedicated packet capture and deployment egress policy |
| Native same-volume server-side range copy, measured in both network directions | Pending |
| Durable staged content and atomic name replacement | Pending |
| Cross-protocol serialization/fencing, including a disconnected writer | Pending |
| Crash at each content/head/journal publication boundary | Pending |
| SFTP copy-data/rename/fsync extension discovery | Pending SSH endpoint |
| Native capabilities versus an on-demand NAS helper decision | Pending |

Capture actual wire bytes including metadata, protocol overhead and retries. Run
warm/cold-cache tests for a 1 GiB file with a 64 KiB edit, append, insertion, and
same-content rewrite. An application byte counter or successful copy syscall does
not establish offload. Temp-directory tests cannot establish NAS durability or locks.

The automation account cannot directly open a packet socket. The small-copy check
above used host authentication through `pkexec`. For remaining range/workload
checks a host operator can also run a narrow capture, then stop it immediately:

```sh
sudo tcpdump -i eno1 -nn -s0 -w /tmp/nas-sync-copy.pcap \
  'host 192.168.1.30 and tcp port 445'
# In a second terminal:
python3 scripts/test_real_nas.py --write \
  --mount-point /mnt/nas-sync-test \
  --test-dir /mnt/nas-sync-test/nas-sync-capability-test
```

Inspect the capture for SMB2 IOCTL copy requests and the absence of a payload-sized
read/write round trip. Keep the capture on the host and remove it after measurement;
it may contain protocol metadata.

The CIFS probe satisfies the client-side setup gate, but automatic synchronization
is still disabled until wire measurements, server-side copy/offload, durability,
cross-client coordination, crash recovery and OS egress enforcement are validated.
If the NAS cannot provide the required native behavior, evaluate the bounded
NAS-side helper described in PLAN.md before enabling automatic writes.

## Required one-time setup

The host now has `gio`, `mount`, `findmnt`, `tcpdump`, `ip`, `mount.cifs` and
`smbclient`. On a new host, install `cifs-utils` and optionally `smbclient` with:

```sh
sudo apt update
sudo apt install cifs-utils smbclient
```

On the QNAP, use Control Panel → Network & File Services → Win/Mac/NFS/WebDAV →
Microsoft Networking. Enable Microsoft Networking, select SMB 3 as the highest and
lowest protocol during the first test, and leave SMB Multichannel disabled until
the route and copy tests pass. Create a dedicated shared folder named
`nas-sync-test`, grant one non-admin NAS user read/write access, and do not use the
personal `home` share or `@Recycle` as the test target. QNAP documents CIFS/SMB,
NFS and NFSv4 as supported Linux choices and requires enabling Microsoft Networking
and shared-folder permissions for CIFS. [QNAP's Linux mounting guide](https://www.qnap.com/en-as/how-to/faq/article/how-to-mount-a-qnap-nas-shared-folder-on-a-linux-client)
and [QTS Samba settings](https://docs.qnap.com/operating-system/qts/5.1.x/it-it/configurazione-delle-impostazioni-samba-servizi-di-rete-microsoft-7447174D.html)
describe these controls.

The configured NAS address is `192.168.1.30`; verify it in QTS under Network &
Virtual Switch → Interfaces, or resolve the hostname on the host with:

```sh
resolvectl query satanasso.local
getent ahostsv4 satanasso.local
```

Use a literal private IPv4 address in the app configuration. The verified host
interface is `eno1` with address `192.168.1.17/24` and a direct route to the NAS.

Create a credentials file outside the repository. It must contain only the NAS
account and password and be readable by root:

```sh
sudo install -m 0600 /dev/null /etc/nas-sync-test.cred
sudoedit /etc/nas-sync-test.cred
# username=YOUR_NAS_USER
# password=YOUR_NAS_PASSWORD
```

Mount the dedicated share manually first. The low `actimeo` value is for the
capability test; it makes metadata freshness visible at the cost of more metadata
requests. The production value should be measured and then raised if the journal
provides sufficient freshness:

```sh
sudo install -d -m 0755 /mnt/nas-sync-test
sudo mount -t cifs //NAS_LAN_IP/nas-sync-test /mnt/nas-sync-test \
  -o 'credentials=/etc/nas-sync-test.cred,vers=3.1.1,cache=strict,serverino,actimeo=1,uid=1000,gid=1000,file_mode=0600,dir_mode=0700,_netdev,nofail'
findmnt -T /mnt/nas-sync-test -o TARGET,SOURCE,FSTYPE,OPTIONS
```

The result must show `FSTYPE=cifs`, the expected `//NAS_LAN_IP/nas-sync-test`
source, `vers=3.1.1`, and no Multichannel option. Then create only the disposable
probe directory and run the explicit write test:

```sh
mkdir /mnt/nas-sync-test/nas-sync-capability-test
python3 scripts/test_real_nas.py --write \
  --mount-point /mnt/nas-sync-test \
  --test-dir /mnt/nas-sync-test/nas-sync-capability-test
```

`cache=strict` follows the CIFS/SMB protocol more closely; `actimeo` controls
attribute-cache duration. Both affect the traffic/freshness tradeoff and must be
included in the measured results. [Kernel CIFS documentation](https://kernel.org/doc/html/latest/admin-guide/cifs/usage.html)
and the [mount.cifs manual](https://www.man7.org/linux/man-pages/man8/mount.cifs.8.html)
describe these options.
