# Startup and LAN mounting

The interactive installer enables the graphical indicator. After the user reviews
a folder setup and approves PC administrator access, the wizard installs an
independent user daemon, enables lingering for boot startup, and installs a CIFS
mount unit for that instance.

The mount helper verifies the configured direct physical LAN and expected mount.
It uses a root-owned private SMB credential file, with encryption and restrictive
mount options. Failed boot mounts retry; successful mount processes exit rather
than polling continuously. Normal synchronization uses provisioned TLS identities.

The NAS launcher supervises an unlimited helper run. If the helper exits or a
local network/bind check fails, it retries after 1, 2, 4, 8, 16, 32, then 60 seconds.
Each attempt repeats the LAN checks and waits for the preceding child to exit;
shutdown interrupts the retry delay. A run lasting at least a minute resets the
delay. Disposable write tests and explicit lifetime limits still run only once.
The mount verifies the NAS through the helper's pinned certificate, so a stopped
helper also prevents SMB mounting even when the NAS's SMB service is reachable.
Restarting the helper allows the existing PC mount retries to recover.

The launcher recovery regression tests cover failed and unexpected clean exits,
capped backoff, cancellation, and preservation of bounded test lifetimes. On
2026-11-08 an isolated QNAP probe used a temporary helper executable that exited
with status 1: the launcher started it three times as the non-root runtime user
and then exited cleanly on SIGTERM during backoff. The probe used a separate
listener port and no production configuration or data; its temporary files were
removed. The existing production helper also passed a 15-second startup/shutdown
check before the repaired launcher was installed, preserving the old binary.
The PC's existing retry then mounted the share with SMB 3.1.1 encryption. Its
installed daemon predated the mount-change watcher and remained offline despite
seeing the mount; replacing it with the current build restored synchronization
after startup indexing. Both previous binaries were retained for rollback. The
original helper exit trigger was not recoverable from the old service's logs.

Generated paths and units are described in [setup-wizard.md](setup-wizard.md).
Existing profiles and unrelated services are not replaced. Stopping a sync daemon
does not remove files. A failed installation retains a password-free progress
record and needs explicit recovery; automatic rollback/resume is not implemented.

Use the generic installer, not the old fixed-machine startup scripts. Public
examples contain fictitious addresses, accounts and interfaces and must be adapted
before use. Raw private remount/reboot logs are not published. Prior PC startup
checks are not a fresh-NAS or NAS-reboot acceptance result.
