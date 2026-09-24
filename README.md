# anaNAS 🍍

An experimental Linux/QNAP folder synchronizer with a native desktop control panel
and interactive setup wizard. Synchronization uses a native NAS helper, delta
transfers and mutual TLS, and is restricted to the configured directly connected
LAN. SSH is used for setup, not routine synchronization.

## Install

Choose a Linux PC and a QNAP on the same directly connected IPv4 LAN. You will
need a QNAP account with administrator/SSH access, an accessible SMB share, and
a new or empty local destination. Keep existing backups while evaluating anaNAS.
Your share, folder names, network interface and mount location can be different
from every example in this repository.

From a source checkout, as your desktop user (not root):

```sh
/usr/bin/python3 scripts/install_ananas.py
```

Use `--cli` for terminal prompts or `--install-only` to install without opening the
wizard. Prerequisites are Go 1.27+, Python GObject/GTK3, OpenSSH, smbclient,
cifs-utils, nftables, iproute2, OpenSSL, systemd and pkexec. Missing prerequisites
are reported; the installer does not silently download or install dependencies.

The installer builds the PC daemon and ARM64, ARMv7 and x86-64 NAS helpers, installs
application files under `~/.local`, and enables the desktop indicator. Existing
running daemons and configurations are preserved.

Open **Set up QNAP / Manage sync folders** from the pineapple menu:

1. Discover a QNAP or enter its local address. Its name appears beside the address.
2. Sign in and verify the SSH host key. QNAP administrator/SSH access is currently
   required for provisioning.
3. Browse accessible shared folders and their immediate contents. Configured sync
   roots appear first, with pineapple markers; ordinary shares remain selectable.
4. Select a directory, choose its local destination and review the installation.
   New directories can be created inside accessible shares. Existing local sync
   configurations are reused rather than duplicated.
5. Approve saved SMB credentials and the separate PC administrator prompt for
   LAN mounting and boot autostart. Routine synchronization uses certificates.

See [setup details and limitations](docs/setup-wizard.md). Fresh-NAS installation,
reboot and multi-folder acceptance remain incomplete; this is not a production
readiness claim.

## Desktop controls

The native control panel shows session/rolling-24-hour transfer totals, NAS volume
capacity, indexed folder sizes and recent activity. Recent updates live in a
submenu rather than crowding the main menu. Folder rows support double-click to
open the file manager, and right-click to open the directory or a terminal there.
Failed desktop actions do not falsely report a daemon crash.
Indicator menu labels stay on one line and are shortened to 72 characters;
long synchronization errors direct you to the control panel for full details.

“Synced to NAS (files)” shows the percentage of included local files whose current
observed versions have been confirmed synchronized, plus the pending file count.
It counts files equally, including empty files; same-size edits remain pending
until confirmed. Refresh the folder view to update these indexed figures. It is
not a live NAS scan or a measure of pending directory operations/deletions.

The **Pending files** tab lists pending paths, sizes, operations and known blockers
in pages of 200 entries. Select a row and use **Confirm sync** to prioritize it
after the current operation, or **Confirm all** for every eligible pending item,
including later pages. New parent folders are queued before their files.
Confirmations survive daemon restarts. They do not mark files as synchronized,
resume paused sync, choose conflict versions or override transfer limits.
Automatic sync remains enabled. Use Refresh for an immediate update; a visible
Pending files tab also refreshes its current page once a minute.
While visible, it also refreshes after daemon file/worker notifications, coalescing
bursts for 500 ms and preserving the selected path. External edits and deletions
therefore update the list without pressing Refresh.
Double-click a pending row to open its containing folder. Right-click (or use the
keyboard context-menu key) for **Open containing folder** and **Open terminal
here**, including blocked files and deleted entries. These open the local parent
folder so you can edit, rename or remove files using your usual desktop tools.
The pending-row menu also offers **Delete…**, which asks before moving the local
item to Trash and defaults to **Cancel**. It never falls back to permanent deletion.
If the item is already absent, the panel refreshes without a Trash error. Deleted
unsupported names with no retained sync history leave the pending queue; normal
NAS deletion acknowledgements remain required for synchronized files.
For tracked content, local deletion follows the normal NAS synchronization rules.
Internal trial records are omitted from pending rows/counts and Confirm all;
their index history remains available for recovery. The panel displays a short
status title with selectable, fully wrapped details below it.

Unsupported names/types and source-specific access failures stay pending with
an explanation while unrelated local files continue. When eligible work finishes,
the status reports that some paths still need attention. Fix the named source and
use Confirm sync to retry; observed changes also release its old issue. Files that
change during preparation and temporary NAS service errors retry automatically
with bounded backoff. See the [reliability audit](docs/reliability-audit.md) for
tested recovery paths and remaining acceptance work.

Traffic accounting includes encrypted transport/protocol overhead and retries.
History is checkpointed during activity and flushed at shutdown; an abrupt crash
can lose the latest unflushed samples. Recording cannot reconstruct older traffic.
Folder sizes use local metadata and last-confirmed versions, not a recursive NAS
scan. NAS capacity is available only through a verified mounted share.

After a disconnect or PC restart, an authenticated notification connection
replaces an abandoned connection for the same client identity. Other identities
remain subject to the configured stream limit.
Files above the configured transfer limit remain pending while other eligible
local files continue syncing; the daemon reports the limit error after the pass.
If a regular file has already been deleted on both sides, replay confirms its
absence against the retained version and continues. A recreated local file is
preserved. Filesystem errors include operation/path context and are not treated
as transient merely because the operating-system error type also supports network
error methods.

## Configure your own PC and NAS

Use the setup wizard above for a working installation: it discovers network
settings, creates identities/certificates and installs the helper and services.
The example JSON files are templates, not ready-to-run synchronization profiles.
In particular, enabling `sync.enabled` alone does not provision a NAS helper.

For manual inspection or an advanced installation, start with
[config.example.json](config.example.json) and replace the following values:

| Setting | Value to supply |
| --- | --- |
| `local.root` | Absolute path to your local sync folder |
| `nas.host`, `nas.share` | Your NAS's last known private IPv4 address and SMB share name. If DHCP moves the NAS, it is found again on `nas.prefix` by its pinned certificate |
| `nas.interface`, `nas.prefix` | PC network interface and directly connected subnet; inspect with `ip -brief address` and `ip route` |
| `nas.mountPoint` | Local mount path for that share |
| `stateDir` | Separate writable state/cache directory outside all sync roots |
| `webPort` | An unused local control-panel port; use a distinct port per profile |
| `selectiveSync` | Local and remote exclusion patterns for your content |
| `limits` | Resource and cache limits suitable for your PC and available storage |
| `sync` | Provisioned helper endpoint, namespace identities, TLS paths, certificate pin and matching transfer limits. Omit `sync.source` so a DHCP-assigned PC address is followed automatically; set it only to pin a static address |

The helper's native NAS path, interface, runtime UID/GID and TLS identities also
need to match that NAS; [config.helper.example.json](config.helper.example.json)
documents its fields; `peers` may list the direct client subnet so the PC keeps
access when DHCP changes its address. Keep helper state outside the synchronized data. The legacy
`remote` SSH section is not the native content-sync transport. The wizard configures
the native TLS transport automatically.


The conventional primary configuration is `~/.config/nas-sync/config.json`.
Wizard instances use separate profiles under `~/.config/nas-sync/profiles/` and
separate state under `~/.local/state/nas-sync/`. State and TLS keys must stay outside
the visible sync roots. Never commit real configuration or credential files.

```sh
go build -o nas-sync ./cmd/nas-sync
./nas-sync -config /path/to/config.json -print-config
./nas-sync -config /path/to/config.json -discover-nas
./nas-sync -config /path/to/config.json -check-lan
```

Copy and edit an example configuration before use. Example machine names, paths,
interfaces and addresses are fictitious; they are not usable deployment settings.
The generic installer is the supported setup entry point. Older fixed-profile
maintenance scripts are reference templates, not installation instructions.

For troubleshooting, open the control panel for your profile and check its full
status and Pending files reasons. Check the daemon service selected by the wizard
with `systemctl --user status <service-name>` and
`journalctl --user -u <service-name>`. Service names and configuration paths depend
on the profile; use **Manage sync folders** to locate it. Never share raw config,
logs or screenshots without removing credentials and personal paths.

## Development and tests

```sh
GOCACHE=/tmp/nas-sync-go-cache GOMODCACHE=/tmp/nas-sync-go-mod-cache go test ./...
GOCACHE=/tmp/nas-sync-go-cache GOMODCACHE=/tmp/nas-sync-go-mod-cache go test -race ./...
GOCACHE=/tmp/nas-sync-go-cache GOMODCACHE=/tmp/nas-sync-go-mod-cache go vet ./...
GOCACHE=/tmp/nas-sync-go-cache GOMODCACHE=/tmp/nas-sync-go-mod-cache go build ./...
PYTHONWARNINGS=ignore /usr/bin/python3 -m unittest discover -s scripts -p test_ananas_setup.py
git diff --check
```

Exercise an actual 3 GiB streaming encode/reconstruction without a disk fixture:

```sh
ANANAS_LARGE_FILE_TEST=1 GOCACHE=/tmp/nas-sync-go-cache GOMODCACHE=/tmp/nas-sync-go-mod-cache go test ./internal/delta -run TestMultiGiBStreamingRoundTrip -count=1
```

Native GUI tests need a desktop display. Hardware/provisioning tests are separate
from unit tests and must be configured for the intended environment.

## Current limits

The implementation includes regular-file synchronization, delta updates, observed
file deletion/recreation, folder creation, durable pause, LAN policy checks and
separate native NAS observation. General directory-tree deletion, conflict-choice
UI, never-observed head reconciliation, history reclamation, enrollment into an
existing helper from another PC, and resumable setup transactions remain open.
The wizard sets an 8 GiB per-file and per-batch limit (the protocol maximum).
Content is streamed through disk-backed snapshots and delta spools; free local
and NAS cache space is required. Helper certificates need renewal;
there is not yet a renewal UI. Do not treat this as a backup replacement.

See [requirements](PROJECT.md), [progress](PLAN.md), [acceptance scope](docs/live-trial.md),
[resource measurements](docs/performance.md) and [publication/privacy notes](docs/publication-audit.md).
