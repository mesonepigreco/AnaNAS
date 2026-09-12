#!/usr/bin/python3
"""Test file deletion/recreation in one new disposable child of the Nasdir trial.

--write creates a unique child as the non-admin NAS user and one 32 KiB fixture.
It verifies NAS deletion reaches the PC, then PC re-creation and deletion reach
the NAS. Every removal is limited to the exact test file after a hash check;
the empty test directory is retained. No credentials or user files are read.
"""
import argparse
import json
import os
from pathlib import Path
import secrets
import shlex
import time

from run_native_probe import remote, session_options
from test_live_directories import digest, status


def wait_for(check, description):
    start = time.monotonic()
    while time.monotonic() - start < 30:
        if check():
            return time.monotonic() - start
        time.sleep(0.5)
    raise RuntimeError(description + " did not converge within 30 s")


def run(args):
    ssh = session_options(args.session_dir)
    if remote(ssh, "id -u nas-sync-test; id -g nas-sync-test").splitlines() != ["1000", "100"]:
        raise ValueError("NAS runtime identity differs")
    initial = status()
    if not initial.get("automaticWrites") or initial.get("paused") or initial.get("syncReason") != "LAN sync active":
        raise ValueError("Active LAN trial required")
    if args.evidence.exists():
        raise ValueError("Evidence path already exists")
    name = ".ananas-tests/anaNAS-deletions-" + secrets.token_hex(6)
    nas = "/share/EXAMPLE_VOLUME/Nasdir/" + name
    local = Path.home() / "NASdir" / name
    path = local / "probe.bin"
    if local.exists() or local.is_symlink():
        raise ValueError("New disposable child required")
    print(json.dumps({"phase": "prepared", "local": str(local), "nas": nas, "write": args.write}), flush=True)
    if not args.write:
        return

    def as_user(command, data=None):
        return remote(ssh, shlex.join(["/bin/busybox", "start-stop-daemon", "-S", "-n",
                      "ananas-delete-test", "-c", "nas-sync-test:everyone", "-x", "/bin/sh", "--", "-c", command]), data=data)

    payload = secrets.token_bytes(32 * 1024)
    as_user("set -eu; umask 077; mkdir -p /share/EXAMPLE_VOLUME/Nasdir/.ananas-tests; mkdir " + nas)
    as_user("set -eu; umask 077; set -C; cat > " + nas + "/probe.bin", payload)

    def local_matches(data):
        return path.is_file() and not path.is_symlink() and path.stat().st_size == len(data) and path.read_bytes() == data

    created = wait_for(lambda: local_matches(payload), "NAS fixture creation")
    as_user("set -eu; test \"$(sha256sum " + nas + "/probe.bin | cut -d ' ' -f 1)\" = " + digest(payload) + "; rm " + nas + "/probe.bin")
    removed = wait_for(lambda: not path.exists() and not path.is_symlink(), "NAS file deletion")
    if not local.is_dir() or local.is_symlink():
        raise ValueError("File deletion changed the containing directory")
    next_payload = secrets.token_bytes(32 * 1024)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write(next_payload)
        output.flush()
        os.fsync(output.fileno())

    def remote_matches():
        result = as_user("if test -f " + nas + "/probe.bin; then sha256sum " + nas + "/probe.bin; else echo absent; fi").split()[0]
        return result == digest(next_payload)

    recreated = wait_for(remote_matches, "PC file recreation")
    if not local_matches(next_payload):
        raise ValueError("Owned PC fixture changed before deletion")
    path.unlink()
    pc_removed = wait_for(lambda: as_user("if test -e " + nas + "/probe.bin || test -L " + nas + "/probe.bin; then echo present; else echo absent; fi").strip() == "absent", "PC file deletion")
    result = {"local": str(local), "nas": nas, "fileBytes": len(payload), "nasCreateSeconds": created,
              "nasDeleteSeconds": removed, "pcRecreateSeconds": recreated, "pcDeleteSeconds": pc_removed,
              "initialSHA256": digest(payload), "recreatedSHA256": digest(next_payload), "status": status(),
              "scope": "One observed regular file with an accessible parent, deleted on NAS, recreated on PC then deleted on PC. Empty disposable directory retained; directory-tree deletion/conflict/reboot acceptance excluded."}
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
