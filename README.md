# anaNAS 🍍

An experimental Linux/QNAP folder synchronizer with a native desktop control panel
and interactive setup wizard. Synchronization uses a native NAS helper, delta
transfers and mutual TLS, and is restricted to the configured directly connected
LAN. SSH is used for setup, not routine synchronization.

## Install

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

Traffic accounting includes encrypted transport/protocol overhead and retries.
History is checkpointed during activity and flushed at shutdown; an abrupt crash
can lose the latest unflushed samples. Recording cannot reconstruct older traffic.
Folder sizes use local metadata and last-confirmed versions, not a recursive NAS
scan. NAS capacity is available only through a verified mounted share.

## Configuration and development

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

```sh
GOCACHE=/tmp/nas-sync-go-cache GOMODCACHE=/tmp/nas-sync-go-mod-cache go test ./...
GOCACHE=/tmp/nas-sync-go-cache GOMODCACHE=/tmp/nas-sync-go-mod-cache go test -race ./...
GOCACHE=/tmp/nas-sync-go-cache GOMODCACHE=/tmp/nas-sync-go-mod-cache go vet ./...
GOCACHE=/tmp/nas-sync-go-cache GOMODCACHE=/tmp/nas-sync-go-mod-cache go build ./...
PYTHONWARNINGS=ignore /usr/bin/python3 -m unittest discover -s scripts -p test_ananas_setup.py
git diff --check
```

Native GUI tests need a desktop display. Hardware/provisioning tests are separate
from unit tests and must be configured for the intended environment.

## Current limits

The implementation includes regular-file synchronization, delta updates, observed
file deletion/recreation, folder creation, durable pause, LAN policy checks and
separate native NAS observation. General directory-tree deletion, conflict-choice
UI, never-observed head reconciliation, history reclamation, enrollment into an
existing helper from another PC, and resumable setup transactions remain open.
The wizard currently sets a 64 MiB per-file limit. Helper certificates need renewal;
there is not yet a renewal UI. Do not treat this as a backup replacement.

See [requirements](PROJECT.md), [progress](PLAN.md), [acceptance scope](docs/live-trial.md),
[resource measurements](docs/performance.md) and [publication/privacy notes](docs/publication-audit.md).
