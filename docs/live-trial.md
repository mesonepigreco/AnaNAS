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

The desktop panel was checked for folder/terminal actions, pending-file
confirmation, automatic refresh and complete wrapped status details. Indicator
errors and paths use compact menu labels. Closing a panel hides its persistent
process, so installed module updates require restarting that process.

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

Full Go unit/race suites, vet and builds passed for the reliability changes.
Subsequent absence/refresh changes passed affected index/replica/web unit/race
tests, vet/build checks and six native panel tests. Fourteen indicator tests
passed. The opt-in streaming codec test processes over 3 GiB without allocating
a whole-file fixture; it does not establish multi-gigabyte NAS throughput.
Desktop launch and Trash calls are mocked in widget tests; production files were
not deleted to test the confirmation button.

See [reliability audit](reliability-audit.md) for the failure matrix and
[README](../README.md#development-and-tests) for reproduction commands. Hardware
tests and long soak tests need separate, explicitly configured environments.

## Outstanding acceptance

Fresh-NAS provisioning, NAS reboot, multi-folder production synchronization,
interrupted-install recovery, broader firmware compatibility, power-loss recovery,
full directory-tree deletion, conflict choices, never-observed head reconciliation
and history reclamation remain incomplete. This application is not a backup
replacement.

Keep private deployment settings, credentials, indexes and recovery evidence
outside Git. Never restore a live index from an old test snapshot. Historical
fixed-profile scripts are illustrative templates; the generic
[setup wizard](setup-wizard.md) is the supported installation entry point.
