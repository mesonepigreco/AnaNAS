# Hardware validation scope

This public summary records exercised behavior and remaining acceptance work.
Deployment addresses, paths, fixture identifiers, timestamps, traffic measurements,
raw logs and screenshots are excluded. Configure hardware tests for your own
environment; previous results do not establish readiness on another NAS.

## Exercised behavior

Selected file creation, delta editing, observed deletion/recreation, folder
creation, pause/resume, native NAS observation and PC boot startup were checked
against a QNAP. Notification reconnection and already-deleted file recovery were
also exercised. After source-failure isolation was deployed, eligible work
continued past unsupported names; deleting an unsupported, never-synchronized
file cleared its stale pending entry. Retained history was preserved.

A DHCP lease change on the PC previously took sync offline because the PC source,
mount check, NAS helper peer list and NAS firewall each pinned the old address.
After the fix, the daemon follows the interface's current address in the direct
prefix and the NAS helper, launcher and firewall authorize that prefix. The
deployed QNAP helper was updated in place with backups. The share remounted
automatically on the new address, the daemon reconnected and the queued backlog
synchronized.

The NAS also uses DHCP. After the follow-up change, the helper listens on its
current address and the PC finds a moved NAS by its pinned certificate. On the
live deployment, the daemon was started with a wrong last-known NAS address. It
found the NAS by searching the subnet and resumed sync, and a file create and
delete then propagated. A real NAS renumbering (and the timer-driven remount)
has not yet been observed.

The PC-side fix regressed when the helper configuration was later rewritten from a
pre-DHCP snapshot that pinned two PC addresses. After the next PC reboot, its new
lease was refused with "NAS protocol status 403: Forbidden". The helper now admits
its whole direct prefix unless `lockPeerAddresses` is set; setup enrollment no
longer pins addresses or keeps stale `.before-enroll` snapshots. On the deployed
QNAP, the updated helper admitted the renumbered PC with the pinned configuration
still in place. With `lockPeerAddresses` enabled, it refused the PC and named the
address. The subnet configuration was then restored. The daemon reconnected
without a restart, and a file create and delete reached the NAS within seconds.

The desktop panel was checked for folder/terminal actions, pending-file
confirmation, automatic refresh and complete wrapped status details. Indicator
errors and paths use compact menu labels. Closing a panel hides its persistent
process, so installed module updates require restarting that process.

An incoming remote rename exposed missing directory-identity receipts on folders
originally uploaded by the receiving PC. The old publisher required those receipts
to retire the old names, and its missing-file error blocked later incoming work.
Directory uploads now retain the receipt before transmission. A separate regression
found that a downloaded path could be renamed before watcher indexing; download
completion now durably seeds its observation before clearing the inbox operation.

The PC update was deployed against the existing blocked journal. It retained three
path decisions, drained unrelated work, and recovered the two old parent folders
using **Save local copy and follow NAS**. Their local remainders were moved outside
the sync root. All 25 files under the three new directory names matched NAS hashes,
and the production pending/problem queues became empty. The live indicator switched
from the broken pineapple back to normal. Notification opt-out persisted through
a daemon restart, and the original preference was restored after the check.

A native NAS/SMB rename hardware check found a further observer limitation: missing
ancestor paths and directory deletions were refused. Children-first reconciliation
now checks live journal descendants and verifies the first missing component under
an accessible pinned parent. Its TLS rename/deletion regression passes in both
lexical directions. Deployment and hardware recheck of this NAS-side extension
still require an authenticated NAS administrator session.

Setup has unit/widget coverage for QNAP identification, host-key handling,
credential cleanup, folder ordering, bounded previews and failure recovery.
Its installation transaction is tested with simulated NAS/system operations and
real locally generated certificates. Credential-free discovery was checked on a
LAN; authenticated fresh-install acceptance remains separate.

## Regression coverage

- TLS stream replacement, temporary service failure and bounded reconnect/retry.
- Unsupported names/types, oversized sources, permission failures, content changes
  during preparation, selection growth, safe abort and unrelated queue progress.
- Retained absence receipts, restart/recreation, missing history/root boundaries,
  exact-generation cleanup and preservation of real NAS deletion obligations.
- Pending pagination, durable confirmation, internal-fixture filtering, current
  file percentages and automatic visible-tab refresh.
- GTK mouse/keyboard menus, Cancel-default Trash confirmation, missing-item and
  permission-error handling, short menu labels and uncropped status details.
- Two independent PC identities using a real TLS helper: remote directory renames
  in both lexical directions, concurrent local edit preservation, unrelated upload
  progress during a conflict, and subtree recovery with the exact edit saved.
- Incoming decisions across index restart, backup replay, never-published child
  restoration, an already-prepared legacy directory deletion, stale/overlapping
  request rejection, separated-subtree inheritance, and CSRF/origin enforcement.
- Native recovery dialog choices and Cancel default, notification suppression and
  deduplication, persistent checkbox settings, and visual contrast/layout checks.

Full Go unit/race suites, vet and builds passed for the reliability changes.
Subsequent absence/refresh changes passed affected index/replica/web unit/race
tests, vet/build checks and six native panel tests. Fourteen indicator tests
passed. The opt-in streaming codec test processes over 3 GiB without allocating
a whole-file fixture; it does not establish multi-gigabyte NAS throughput.
Desktop launch and Trash calls are mocked in widget tests; production files were
not deleted to test the confirmation button.

The rename/recovery changes passed `go test ./...`, `go test -race ./...`,
`go vet ./...`, `go build ./...` and `git diff --check`, using the temporary Go
caches documented in README. Native Python tests passed for recovery (3), control
panel (6) and indicator (18), plus the existing setup coverage. GTK visual checks
used synthetic conflicts; production recovery used the same authenticated API
called by the dialog. An ARM64 helper build was also produced for deployment.

See [reliability audit](reliability-audit.md) for the failure matrix and
[README](../README.md#development-and-tests) for reproduction commands. Hardware
tests and long soak tests need separate, explicitly configured environments.

## Outstanding acceptance

Fresh-NAS provisioning, NAS reboot, multi-folder production synchronization,
interrupted-install recovery, broader firmware compatibility, power-loss recovery,
broader directory-tree deletion and conflict-choice acceptance, never-observed head reconciliation
and history reclamation remain incomplete. This application is not a backup
replacement.

Keep private deployment settings, credentials, indexes and recovery evidence
outside Git. Never restore a live index from an old test snapshot. Historical
fixed-profile scripts are illustrative templates; the generic
[setup wizard](setup-wizard.md) is the supported installation entry point.
