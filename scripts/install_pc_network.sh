#!/bin/sh
# Install only the fixed anaNAS endpoint rule and its boot-time unit.
set -eu
[ "${ANANAS_LEGACY_PROFILE_CONFIGURED:-}" = yes ] || { echo 'Historical template: configure and review targets before explicit opt-in.' >&2; exit 2; }
[ "$(/usr/bin/id -u)" = 0 ] || exit 1
ANANAS_REPO=/home/example-user/Documents/Programming/nas-sync
test ! -e /etc/ananas-network.nft
test ! -e /etc/systemd/system/ananas-network.service
if /usr/sbin/nft list table inet ananas >/dev/null 2>&1; then
  echo 'Existing anaNAS network table requires inspection' >&2
  exit 1
fi
/usr/sbin/nft --check -f "$ANANAS_REPO/deploy/ananas-pc-network.nft"
/usr/bin/install -m 0644 "$ANANAS_REPO/deploy/ananas-pc-network.nft" /etc/ananas-network.nft
/usr/bin/install -m 0644 "$ANANAS_REPO/deploy/ananas-network.service" /etc/systemd/system/ananas-network.service
/usr/bin/systemctl daemon-reload
/usr/bin/systemctl enable --now ananas-network.service
/usr/sbin/nft list table inet ananas
