#!/bin/sh
# Explicit, read-only NAS runtime inspection over an authenticated SSH session.
# No recursive listing, credential reads, hashing, installation or NAS writes.
set -eu
[ "${ANANAS_LEGACY_PROFILE_CONFIGURED:-}" = yes ] || { echo 'Historical template: configure and review targets before explicit opt-in.' >&2; exit 2; }
uname -srm
id
for tool in sh flock python3 rsync sha256sum sync; do
    command -v "$tool" || true
done
if test -d /share/Nasdir; then
    printf '%s\n' 'Dedicated /share/Nasdir directory exists'
    readlink /share/Nasdir || true
    df -k /share/Nasdir
fi
