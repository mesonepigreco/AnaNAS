# Private deployment acceptance summary

This is a sanitized scope summary, not a reproducible record of a particular
machine. Raw screenshots, directory lists, identifiers, logs and recovery records
are intentionally excluded from the public repository.

## Exercised behavior

Selected regular-file creation, delta editing, observed deletion/recreation,
folder creation, pause/resume, native NAS observation and PC boot startup were
checked against a QNAP. The native control panel and folder actions were also
checked. A desktop launch failure was distinguished from a daemon failure;
connection state no longer changes merely because opening a folder fails.

The setup interface has unit/widget coverage for QNAP identification, host-key
handling, credential cleanup, configured-first folder ordering, bounded directory
preview, name/address consistency, icons, selection and error recovery. Its
installation transaction is tested with simulated NAS/system operations and real
locally generated certificates. Credential-free QNAP discovery was also checked
on a LAN. These are distinct from authenticated fresh-install acceptance.

## Not established

Fresh-NAS provisioning, NAS reboot, multi-folder production synchronization,
interrupted-install recovery, broad firmware compatibility, power-loss recovery,
full directory deletion and conflict-choice acceptance remain incomplete.

Operational state must be inspected on the target machine; this repository does
not contain current deployment settings, credentials or safe-to-restore indexes.
Never overwrite a live index with an old test snapshot. Historical fixed-profile
scripts are templates only; use the generic [setup wizard](setup-wizard.md).
