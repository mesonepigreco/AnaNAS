# Startup and LAN mounting

The interactive installer enables the graphical indicator. After the user reviews
a folder setup and approves PC administrator access, the wizard installs an
independent user daemon, enables lingering for boot startup, and installs a CIFS
mount unit for that instance.

The mount helper verifies the configured direct physical LAN and expected mount.
It uses a root-owned private SMB credential file, with encryption and restrictive
mount options. Failed boot mounts retry; successful mount processes exit rather
than polling continuously. Normal synchronization uses provisioned TLS identities.

Generated paths and units are described in [setup-wizard.md](setup-wizard.md).
Existing profiles and unrelated services are not replaced. Stopping a sync daemon
does not remove files. A failed installation retains a password-free progress
record and needs explicit recovery; automatic rollback/resume is not implemented.

Use the generic installer, not the old fixed-machine startup scripts. Public
examples contain fictitious addresses, accounts and interfaces and must be adapted
before use. Raw private remount/reboot logs are not published. Prior PC startup
checks are not a fresh-NAS or NAS-reboot acceptance result.
