# QNAP capability validation — CIFS mount ready; protocol gate pending

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
| Same-share server-side copy/offload | Strong CIFS-counter evidence from a 106,593-byte copy; packet capture still required |
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

The automation account cannot open a packet socket because `tcpdump` requires the
host's interactive sudo authorization. To close the offload gate, a host operator
must run a narrow capture around the disposable probe, then stop it immediately:

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
