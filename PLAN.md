# anaNAS implementation progress

[PROJECT.md](PROJECT.md) is the requirements source of truth. This public summary
describes implemented capabilities and remaining work, without private deployment
addresses, paths, machine identifiers or raw operational logs.

## Implemented

- Linux metadata observation using recursive inotify, bounded coalescing, paced
  scans and a durable index. Private state is separate from visible sync content.
- Native NAS helper and unprivileged runtime, device-bound LAN transport, pinned
  mutual TLS, bounded manifests/chunks, delta reconstruction and publication.
- Cooperative journals, upload recovery, remote-change inbox, acknowledged bases,
  regular-file create/edit, observed file deletion/recreation, and folder creation.
- Event-driven LAN supervision and durable pause/resume. Per-task resource
  controls and configurable scan/transfer limits.
- GNOME pineapple indicator, nested recent updates, and a reusable native control
  panel with transfer accounting, NAS capacity, indexed sizes and folder actions.
- Separation of action errors from connection status, first-expansion correction,
  default file-manager/terminal launching and legible menu styling.
- Notification reconnection replaces abandoned streams for the same authenticated
  identity; folder progress counts confirmed current files and pending files.
- Oversized local files remain pending without starving later eligible uploads.
- Native Pending files tab with paginated operations and blocker reasons,
  per-file and all-file confirmation, and durable priority scheduling.
  Pending rows also open their containing folder or a terminal through the same
  desktop actions as the folder panel.
- Recovery when a regular file is deleted independently on both sides, with
  durable absence receipts, recreation preservation and corrected retry classification.
- Per-source failure isolation with durable, generation-bound reasons in Pending
  files; partial-completion status, automatic changed-source/service retries and
  real TLS fault-injection coverage. See [reliability audit](docs/reliability-audit.md).
- Interactive source installer and native setup wizard: QNAP-only discovery,
  hostname beside address, SSH host-key verification, credential reuse, accessible
  shared-folder browsing, configured-first ordering, folder and pineapple icons.
- Independent new sync profiles, native helper provisioning, saved SMB mounting,
  autostart installation and existing-local-profile management.

## Verification scope

Go unit/race tests, vet and builds pass. Setup backend tests include a simulated
complete installation with real certificate generation. Native GTK regressions
cover folder browsing, selection, expansion, icons and failure presentation.
QNAP product discovery has been checked on a real LAN. Selected sync operations
and PC boot behavior were exercised in a private deployment; those results are
not full acceptance or a guarantee for another machine.

Raw evidence and deployment-specific recovery records are excluded from the
public repository. Public examples use fictitious values. See
[publication audit](docs/publication-audit.md) and [acceptance scope](docs/live-trial.md).

## Remaining work

- Full directory-tree deletion and never-observed journal-head reconciliation.
- Conflict decisions, selective-sync UI, per-file progress and rate/history UI.
- Safe history/cache reclamation and broader recovery/fault acceptance.
- Ordinary-user onboarding, QTS share/account creation, and enrollment of another
  PC into an existing helper without replacing its namespace or trust settings.
- Batch folder selection, graphical exclusions and resource customization.
- Automatic recovery/resumption of interrupted setup; certificate/password rotation.
- Fresh authenticated NAS setup, NAS reboot/remount and multi-folder end-to-end
  acceptance across supported hardware/firmware.

The complete application objective remains unfinished. The wizard implementation
must not be confused with completed fresh-install acceptance.
