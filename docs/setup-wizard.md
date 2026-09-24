# Interactive setup wizard — 2026-09-12

The source installer and native GTK wizard are a first setup implementation,
not yet a fresh-QNAP end-to-end acceptance result. Existing installations remain
independent and are not migrated or overwritten.

## Start

Run as the desktop user, not root:

```sh
/usr/bin/python3 scripts/install_ananas.py
```

`--cli` uses terminal prompts and `getpass`; `--install-only` installs application
files without starting the wizard. A source checkout, Go 1.27+, Python GObject/
GTK3, OpenSSH client, smbclient, cifs-utils, nftables, iproute2, OpenSSL, systemd
and pkexec are prerequisites. On Debian/Ubuntu the non-Go packages are
`python3-gi gir1.2-gtk-3.0 openssh-client smbclient cifs-utils nftables
policykit-1 openssl iproute2`. Missing dependencies produce an actionable error;
the script does not download arbitrary binaries or silently install packages.

The installer builds the PC daemon and Linux ARM64, ARMv7 and x86-64 QNAP helper/
launcher pairs, installs them under `~/.local`, enables the graphical indicator,
and adds an **anaNAS Setup** desktop launcher. Existing running daemons are not
restarted. The first overwritten PC binary is backed up as
`~/.local/bin/nas-sync.before-setup`. Configurations and indexes are not replaced.

The pineapple menu includes **Set up QNAP…**, or **Manage sync folders…** when
transfers are enabled. An installation without a configuration says **setup
required**, not **daemon unavailable**, and does not poll a nonexistent daemon.
The setup window is single-instance. Additional control-panel windows are keyed
by their profile's local root rather than accidentally opening the first root.

## Flow

1. Enter a NAS IP or select **Find QNAP on LAN**. Discovery checks configured hosts,
   neighbours and physical-interface subnets up to 1,024 addresses, with sixteen
   workers and short TCP probes of ports 8080, 443 and 80. Only devices returning
   the QNAP QTS device-information XML are shown, with hostname and address;
   open ports alone never qualify a router or PC. This credential-free request uses
   the [QTS authentication endpoint](https://download.qnap.com/dev/API_QNAP_QTS_Authentication.pdf)
   with **no authentication parameters**, verified against the deployed NAS.
   XML size/entities are bounded, proxies are ignored and redirects are not followed.
   HTTPS certificate verification is skipped **only for credential-free product
   discovery**; SSH host-key verification still protects the actual login. Larger
   subnets/custom web ports may require manual IP entry. Off-LAN, VPN, gateway and
   nonphysical routes are refused.
   The detected hostname is displayed immediately beside/below the address, in
   the device dropdown and in the folder-browser header. Manually entered IPs get
   a debounced, credential-free name lookup; changing the IP clears the previous
   name immediately and stale responses cannot relabel another address.
2. Enter the QNAP administrator login once and verify its first SSH host-key
   fingerprint. SSH must already be enabled. Subsequent changed keys are rejected.
   The authenticated session verifies QNAP configuration files and inspects shares,
   accessible SMB roots, non-root runtime accounts, and existing anaNAS QPKGs.
   SMB access is tested with encryption; an incomplete/timed-out inventory fails
   rather than presenting a misleading partial count.
3. The **Choose QNAP folders to sync** browser shows all accessible shared folders,
   including ones never used with anaNAS. Configured folders have a device-count
   tag and sort first; nested configured roots are promoted as shortcuts without
   hiding their ordinary parent shares. Expand any directory to preview immediate
   subfolders and file names/sizes. This reads directory metadata, not file contents;
   symlinks are not followed and each preview displays up to 1,000 entries.
   Folder/file icons distinguish entries, and a bundled anaNAS pineapple appears
   beside configured sync roots and folders inside them. A parent merely containing
   a sync root is labelled accordingly, without falsely marking its whole tree synced.
   Select an existing folder and **Continue**, or choose **New folder** to prepare a
   new directory within the selection. Then choose a new/empty local destination.
   The non-admin runtime account and saved-mount credential option are under Advanced
   setup options. A folder already configured on this PC reuses its existing daemon
   and certificates rather than attempting to install a second helper. No existing data is
   merged into a non-empty PC folder during setup. Overlapping local/NAS sync roots
   and existing helper installations are rejected.
4. Review the exact NAS/PC paths and saved-credential consent. Installing mounting
   and boot autostart requires a separate **PC administrator** confirmation. This
   is distinct from the QNAP password. Runtime synchronization uses mutual TLS.
5. Setup stages an independent native helper, hidden state and pinned certificates,
   verifies uploaded hashes and native configuration, installs a direct-LAN CIFS
   mount and endpoint firewall, registers a QNAP startup service, enables user
   lingering and an independent PC daemon, then checks readiness. The result says
   **installed but not ready** if the daemon has not become active within 30 seconds.
   Additional folders can reuse the same SSH login while this window remains open.

The management view shows configured PC roots, checks their service states, and
offers enable/start, stop/disable-autostart and the corresponding control panel.
Stopping does not delete files. Incomplete staged configurations cannot be started
from this view. The first wizard profile also supplies the primary panel config;
deduplication prevents advertising or starting two daemons on its index.

## Credentials, state and recovery

Passwords are not included in argv, logs, daemon/helper JSON, or source files.
The SSH askpass response lives briefly in a 0600 file under a private temporary
directory and is removed on both successful and failed authentication. The SSH
master disables forwarding and closes when setup closes. The password remains in
process memory while the user adds folders; this is not a memory-zeroization claim.
Only the explicitly approved SMB credential persists in a root-owned 0600 file.

New local profiles: `~/.config/nas-sync/profiles/<id>/config.json`, with private TLS
files and a password-free `setup.json` progress/recovery record beside it. Local
state: `~/.local/state/nas-sync/<id>`. Service: `ananas-sync-<id>.service`.
New NAS runtime files: `.ananas-<id>` beside the share, outside visible sync data.
System mounting: `/etc/ananas-setup/<id>.*`, `/mnt/ananas-<id>` and
`ananas-mount-<id>.service`. A successful mount exits and has no idle timer;
failed boot mounts retry every 60 seconds, checking the exact direct LAN first.
Existing system paths are never overwritten by the privileged installer.

An interrupted installation retains its exact progress record and staged files,
and names that record in the error. It is **not automatically rolled back or
resumed**. Inspect that record and the exact new paths before recovery; do not
create a competing helper, blindly restore an old index, or delete broad trees.
The old deployment-specific installer scripts remain historical tools; the new
wizard does not invoke them.

## Remaining limitations and acceptance

- An administrator with SSH access is required; ordinary-account onboarding via
  a QTS authentication API is not implemented. SSH/sudo-ineligible accounts are
  rejected with an explanation. Two-factor interactive SSH challenges beyond the
  supplied password are not supported.
- No accessible share or non-admin account: the wizard explains the needed QTS
  setup. It creates directories *within* shares, not new QTS SMB shares/accounts.
- A directory already managed on another PC cannot yet be enrolled automatically.
  Existing helper certificate allowlists and journal namespaces are preserved.
  Management of the existing local profile works without a new NAS login.
- Settings beyond root/runtime-account selection use the generated JSON profile;
  there is not yet a graphical exclusion editor, batch folder selection, password
  rotation, certificate renewal UI, or automatic partial-install recovery.
- Helper certificates expire after one year. The PC address may change with
  DHCP: the daemon omits `sync.source` and follows the interface's current
  address in the direct prefix, and the NAS helper, launcher `-peer` and iptables
  rule authorize that prefix rather than one lease. NAS address, interface or
  subnet changes still require configuration maintenance.
- Engine limitations still apply: 8 GiB file/batch cap, unfinished general
  directory deletion, conflict choices and broader reconciliation acceptance.
- Automated checks cover QNAP-only identity filtering, friendly connection-error
  classification, configured-first ordering, nested-root shortcuts, bounded folder
  preview parsing, validation, temporary credential cleanup, host-key
  pinning, generated certificates, systemd escaping, duplicate prevention, and a
  full **simulated** new-instance transaction with real private files/certificates.
  GTK widget tests cover login/folder presentation, first expansion, files vs folder
  selection, Continue/destination routing and failure recovery. ARMv7
  cross-compilation exposed and fixed an int32/int64 RSS-reporting conversion.
- Production checks: the corrected discovery returns **only example-nas at
  `10.23.42.30`**, excluding the router `.1` and other PC `.5`;
  an existing daemon remained running without restarts. The wizard was opened
  for user login testing. **A fresh authenticated NAS install, reboot/remount,
  multi-directory production sync and failed-install recovery remain unverified.**
