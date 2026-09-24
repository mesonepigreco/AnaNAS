#!/bin/sh
# Install the explicitly requested unattended startup, and verify a real remount.
# The existing credential file is used only by mount.cifs, never opened here.
set -eu
[ "${ANANAS_LEGACY_PROFILE_CONFIGURED:-}" = yes ] || { echo 'Historical template: configure and review targets before explicit opt-in.' >&2; exit 2; }
[ "$(/usr/bin/id -u)" = 0 ] || { echo 'Administrator authentication is required' >&2; exit 1; }
[ "${1:-}" = --install-and-verify ] || exit 2
ANANAS_REPO=/home/example-user/Documents/Programming/nas-sync
for ANANAS_FILE in /usr/local/libexec/ananas-mount \
    /etc/ananas-mount-network.nft \
    /etc/systemd/system/ananas-mount-network.service \
    /etc/systemd/system/ananas-mount.service \
    /etc/systemd/system/ananas-mount.timer \
    /etc/NetworkManager/dispatcher.d/90-ananas-mount; do
    test ! -e "$ANANAS_FILE" || { echo "Existing installation path requires inspection: $ANANAS_FILE" >&2; exit 1; }
done
if /usr/sbin/nft list table inet ananas_mount >/dev/null 2>&1; then
    echo 'Existing anaNAS mount firewall requires inspection' >&2
    exit 1
fi
echo 'Checking direct LAN and existing mount'
/usr/bin/python3 "$ANANAS_REPO/scripts/ananas_mount.py" --check-lan || { echo 'Direct LAN preflight did not pass' >&2; exit 1; }
/usr/bin/python3 "$ANANAS_REPO/scripts/ananas_mount.py" --check-mount || { echo 'Existing mount preflight did not pass' >&2; exit 1; }
/usr/sbin/nft --check -f "$ANANAS_REPO/deploy/ananas-mount-network.nft"
echo 'Installing anaNAS startup services'
/usr/bin/install -d -m 0755 /usr/local/libexec
/usr/bin/install -m 0755 "$ANANAS_REPO/scripts/ananas_mount.py" /usr/local/libexec/ananas-mount
/usr/bin/install -m 0644 "$ANANAS_REPO/deploy/ananas-mount-network.nft" /etc/ananas-mount-network.nft
/usr/bin/install -m 0644 "$ANANAS_REPO/deploy/ananas-mount-network.service" /etc/systemd/system/ananas-mount-network.service
/usr/bin/install -m 0644 "$ANANAS_REPO/deploy/ananas-mount.service" /etc/systemd/system/ananas-mount.service
/usr/bin/install -m 0644 "$ANANAS_REPO/deploy/ananas-mount.timer" /etc/systemd/system/ananas-mount.timer
/usr/bin/install -m 0755 "$ANANAS_REPO/deploy/90-ananas-mount" /etc/NetworkManager/dispatcher.d/90-ananas-mount
/usr/bin/systemd-analyze verify /etc/systemd/system/ananas-mount.service /etc/systemd/system/ananas-mount-network.service /etc/systemd/system/ananas-mount.timer
/usr/bin/systemctl daemon-reload
/usr/bin/systemctl enable --now ananas-mount-network.service
/usr/bin/systemctl enable ananas-mount.service
/usr/bin/systemctl enable --now ananas-mount.timer
/usr/bin/loginctl enable-linger example-user
ananas_user() {
    /usr/sbin/runuser -u example-user -- /usr/bin/env \
        XDG_RUNTIME_DIR=/run/user/1000 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus \
        /usr/bin/systemctl --user "$@"
}
ananas_user enable nas-sync.service
# Stop only this sync daemon for a normal unmount. No force/lazy unmount: an
# unrelated open user handle must refuse the verification rather than lose I/O.
ananas_user stop nas-sync.service
trap 'ananas_user start nas-sync.service' EXIT
/usr/bin/umount /mnt/nasdir
/usr/bin/systemctl start ananas-mount.service
/usr/local/libexec/ananas-mount --check-mount
ananas_user start nas-sync.service
trap - EXIT
echo 'anaNAS boot startup and saved-credential remount verified'
/usr/bin/loginctl show-user example-user -p Linger
/usr/bin/systemctl is-enabled ananas-mount.service ananas-mount-network.service ananas-mount.timer
