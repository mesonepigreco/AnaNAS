#!/usr/bin/python3
"""Verify startup and both sync directions in a disposable trial child.

Reads no credentials. Creates exclusive tiny files inside the known earlier
deletion-test child, verifies PC-to-NAS and NAS-to-PC propagation through CIFS,
then removes only those owned files and verifies propagated cleanup. Never lists
either sync root. Mutating checks require --write.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import time

from ananas_mount import check_lan, mounted
from test_live_directories import status


def run(args):
    local = args.test_dir
    recorded = json.loads(Path("docs/evidence/live-file-deletions-2026-09-09.json").read_text())
    if str(local) != recorded["local"] or local.parent != Path.home()/"NASdir" or not re.fullmatch(r"anaNAS-deletions-[a-f0-9]{12}", local.name):
        raise ValueError("Exact pre-created disposable deletion-test child required")
    if not check_lan() or not mounted():
        raise ValueError("Configured LAN and exact CIFS mount required")
    nas_dir = Path("/mnt/nasdir")/local.name
    for directory in (local, nas_dir):
        if directory.is_symlink() or not directory.is_dir():
            raise ValueError("Pre-created disposable child is unavailable")
    initial = status()
    if not initial["automaticWrites"] or initial["paused"] or initial["syncReason"] != "LAN sync active":
        raise ValueError("Active unpaused sync required")
    print(json.dumps({"phase": "prepared", "local": str(local), "nas": str(nas_dir), "write": args.write}), flush=True)
    if not args.write:
        return
    name = "startup-"+secrets.token_hex(6)+".txt"
    target, nas = local/name, nas_dir/name
    payload = ("anaNAS unattended startup verification\n"+secrets.token_hex(32)+"\n").encode()
    fd = os.open(target, os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write(payload)
        output.flush()
        os.fsync(output.fileno())
    start = time.monotonic()
    while time.monotonic()-start < 30:
        try:
            fd = os.open(nas, os.O_RDONLY|os.O_NOFOLLOW)
            with os.fdopen(fd, "rb") as source:
                data = source.read(len(payload)+1)
            if data == payload:
                break
        except FileNotFoundError:
            pass
        time.sleep(0.5)
    else:
        raise RuntimeError("Post-remount file did not sync within 30 s")
    seconds = time.monotonic()-start
    if target.is_symlink() or target.read_bytes() != payload:
        raise ValueError("Owned fixture changed; retained for inspection")
    target.unlink()
    start = time.monotonic()
    while nas.exists() or nas.is_symlink():
        if time.monotonic()-start > 30:
            raise RuntimeError("Owned verification file cleanup did not synchronize")
        time.sleep(0.5)
    cleanup_seconds = time.monotonic()-start

    nas_name = "startup-nas-"+secrets.token_hex(6)+".txt"
    nas_target, local_copy = nas_dir/nas_name, local/nas_name
    nas_payload = ("anaNAS post-boot NAS observation verification\n"+secrets.token_hex(32)+"\n").encode()
    fd = os.open(nas_target, os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write(nas_payload)
        output.flush()
        os.fsync(output.fileno())
    start = time.monotonic()
    while time.monotonic()-start < 30:
        try:
            if not local_copy.is_symlink() and local_copy.read_bytes() == nas_payload:
                break
        except FileNotFoundError:
            pass
        time.sleep(0.5)
    else:
        raise RuntimeError("NAS-side file did not sync to the PC within 30 s")
    nas_seconds = time.monotonic()-start
    if nas_target.is_symlink() or nas_target.read_bytes() != nas_payload:
        raise ValueError("Owned NAS fixture changed; retained for inspection")
    nas_target.unlink()
    start = time.monotonic()
    while local_copy.exists() or local_copy.is_symlink():
        if time.monotonic()-start > 30:
            raise RuntimeError("Owned NAS verification file cleanup did not synchronize")
        time.sleep(0.5)
    nas_cleanup_seconds = time.monotonic()-start

    result = {"testDirectory": str(local), "file": name, "bytes": len(payload),
              "sha256": hashlib.sha256(payload).hexdigest(), "syncSecondsAfterWrite": seconds,
              "cleanupSyncSeconds": cleanup_seconds,
              "nasFile": nas_name, "nasBytes": len(nas_payload),
              "nasSHA256": hashlib.sha256(nas_payload).hexdigest(),
              "nasToPCSyncSeconds": nas_seconds,
              "nasCleanupSyncSeconds": nas_cleanup_seconds,
              "cleanupSynced": True,
              "bootId": Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
              "linger": subprocess.check_output(["loginctl", "show-user", "example-user", "-p", "Linger", "--value"], text=True).strip(),
              "startupUnits": subprocess.check_output(["systemctl", "is-enabled", "ananas-mount.service", "ananas-mount-network.service"], text=True).splitlines(),
              "mountService": subprocess.check_output(["systemctl", "show", "ananas-mount.service", "-p", "Result", "-p", "ExecMainStatus", "-p", "ActiveState"], text=True),
              "daemonActiveEnter": subprocess.check_output(["systemctl", "--user", "show", "nas-sync.service", "-p", "ActiveEnterTimestamp", "--value"], text=True).strip(),
              "status": status(), "scope": "Post-boot production state with the saved-credential CIFS mount active. Exact disposable PC-to-NAS and NAS-to-PC file sync and cleanup verified through CIFS; no LAN disconnect performed."}
    if result["linger"] != "yes" or result["startupUnits"] != ["enabled", "enabled"]:
        raise ValueError("Boot startup is not enabled")
    with args.evidence.open("x") as output:
        json.dump(result, output, indent=2)
        output.write("\n")
    print(json.dumps(result), flush=True)


if __name__ == "__main__":
    from legacy_profile import require_configured
    require_configured()
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--test-dir", type=Path, required=True)
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--write", action="store_true")
    run(parser.parse_args())
