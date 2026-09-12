# QNAP end-to-end validation

Use [setup-wizard.md](setup-wizard.md) to understand the current setup workflow and
[live-trial.md](live-trial.md) for its acceptance limits. Examples in this repository
are anonymized templates, not an operational NAS configuration.

## Validate on the intended target

1. Verify the NAS address, SSH host key, accessible share and runtime permissions.
2. Check the exact physical interface/subnet, pinned TLS endpoint and encrypted
   CIFS mount. Confirm off-LAN and wrong-mount cases refuse synchronization.
3. Verify regular-file creation, changes, delta transfer, observed deletion and
   recreation in both directions, using purpose-created test data.
4. Check directory creation, pause/resume, disconnect/reconnect and process restart.
5. Separately validate PC/NAS boot, saved mounting and recovery from interrupted
   operations. Do not infer those results from a successful unit test or build.
6. Record the firmware, test scope and results privately. Review any material for
   identifying data or credentials before publishing a summary.

Do not run historical fixed-profile maintenance scripts on a live share without
adapting and inspecting their exact targets. General tree deletion, conflict
choices, history reclamation and complete reconciliation acceptance remain open.
Raw deployment evidence and recovery files are intentionally not distributed.
