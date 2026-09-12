#!/usr/bin/python3
"""Verify native NAS folder creation and a PC edit in one new disposable child.

Requires --write and the existing authenticated SSH session. It creates only a
new uniquely named anaNAS-folders-* child, a nested 96 KiB file and an empty
directory as the non-admin NAS user. It leaves that child for user inspection;
no deletion, recursive NAS search or existing user file is involved.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import shlex
import stat
import time
import urllib.request

from run_native_probe import remote, session_options

ROOT = "/share/EXAMPLE_VOLUME/Nasdir"


def status():
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open("http://127.0.0.1:8721/api/status", timeout=5) as response:
        return json.load(response)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def run(args):
    ssh = session_options(args.session_dir)
    if remote(ssh, "id -u nas-sync-test; id -g nas-sync-test").splitlines() != ["1000", "100"]:
        raise ValueError("NAS runtime identity differs")
    initial = status()
    if initial.get("automaticWrites") is not True or initial.get("paused") or initial.get("syncReason") != "LAN sync active":
        raise ValueError("An active, unpaused LAN trial is required")
    name = ".ananas-tests/anaNAS-folders-" + secrets.token_hex(6)
    local = Path.home() / "NASdir" / name
    nas = ROOT + "/" + name
    if local.exists() or local.is_symlink():
        raise ValueError("Expected an unused local test child")
    print(json.dumps({"phase": "prepared", "local": str(local), "nas": nas,
                      "fileBytes": 96 * 1024, "write": args.write}), flush=True)
    if not args.write:
        return

    def as_user(command, data=None):
        return remote(ssh, shlex.join(["/bin/busybox", "start-stop-daemon", "-S", "-n",
                      "ananas-folder-test", "-c", "nas-sync-test:everyone", "-x", "/bin/sh", "--", "-c", command]), data=data)

    # mkdir is exclusive; only after success do we write inside our new child.
    as_user("set -eu; umask 077; mkdir -p " + ROOT + "/.ananas-tests; mkdir " + nas)
    payload = secrets.token_bytes(96 * 1024)
    started = time.monotonic()
    as_user("set -eu; umask 077; mkdir " + nas + "/inner " + nas + "/empty; set -C; cat > " + nas + "/inner/probe.bin", payload)
    path = local / "inner/probe.bin"
    while time.monotonic() - started < 30:
        if path.exists() and not path.is_symlink() and path.stat().st_size == len(payload):
            if path.read_bytes() == payload and (local / "empty").is_dir():
                break
        time.sleep(0.5)
    else:
        raise RuntimeError("Native NAS nested file and empty folder did not reach the PC within 30 s")
    download_seconds = time.monotonic() - started
    data = bytearray(payload)
    data[8192] ^= 1
    fd = os.open(path, os.O_RDWR | os.O_NOFOLLOW)
    with os.fdopen(fd, "r+b") as target:
        info = os.fstat(target.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_size != len(payload) or target.read() != payload:
            raise ValueError("Test file changed before the PC edit")
        target.seek(8192)
        target.write(bytes([data[8192]]))
        target.flush()
        os.fsync(target.fileno())
    started = time.monotonic()
    while time.monotonic() - started < 30:
        actual = as_user("sha256sum " + nas + "/inner/probe.bin").split()[0]
        if actual == digest(data):
            break
        time.sleep(0.75)
    else:
        raise RuntimeError("The nested PC edit did not reach the NAS within 30 s")
    result = {"local": str(local), "nas": nas, "fileBytes": len(payload),
              "nativeFolderDownloadSeconds": download_seconds,
              "nestedPCEditUploadSeconds": time.monotonic() - started,
              "initialSHA256": digest(payload), "finalSHA256": digest(data),
              "emptyFolderSynced": True, "status": status(),
              "scope": "One new native NAS nested folder/file and empty directory, then one-byte PC edit; test child retained. No deletion/conflict/reboot acceptance."}
    with args.evidence.open("x") as output:
        json.dump(result, output, indent=2)
        output.write("\n")
    print(json.dumps(result), flush=True)


if __name__ == "__main__":
    from legacy_profile import require_configured
    require_configured()
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--session-dir", type=Path, required=True)
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--write", action="store_true")
    run(parser.parse_args())
