#!/usr/bin/python3
"""Update only the installed Nasdir helper through a user-authenticated SSH session.

Default mode checks the exact installed executable, identity and service paths.
--install uploads a new exclusive candidate, validates it as the runtime user,
backs up the old binary and restarts only anaNAS. Configuration, keys, firewall
and visible share contents are preserved. Passwords are never accessed.
"""
import argparse
import hashlib
import json
from pathlib import Path
import re
import secrets
import shlex
import time

from run_native_probe import binary_payload, remote, session_options

BASE = "/share/EXAMPLE_VOLUME/.ananas-service"
PID = "/var/run/ananas-nasdir.pid"


def run(args):
    if not re.fullmatch(r"[a-f0-9]{64}", args.expected_sha256):
        raise ValueError("Expected installed SHA-256 required")
    ssh = session_options(args.session_dir)
    data = binary_payload(args.helper)
    digest = hashlib.sha256(data).hexdigest()
    check = remote(ssh, "set -eu; test -d " + BASE + "; test ! -L " + BASE +
                   "; test -f " + BASE + "/helper; test ! -L " + BASE + "/helper; "
                   "test -d /share/EXAMPLE_VOLUME/.ananas-observer-state; "
                   "id -u nas-sync-test; id -g nas-sync-test; sha256sum " + BASE + "/helper")
    lines = check.splitlines()
    if len(lines) != 3 or lines[:2] != ["1000", "100"] or lines[2].split()[0] != args.expected_sha256:
        raise ValueError("Installed NAS identity or helper differs; no update made")
    remote(ssh, BASE + "/service.sh status")
    print(json.dumps({"phase": "prepared", "oldSHA256": args.expected_sha256,
                      "newSHA256": digest, "bytes": len(data), "install": args.install}), flush=True)
    if not args.install:
        return
    suffix = time.strftime("%Y%m%d-%H%M%S") + "-" + secrets.token_hex(4)
    candidate = BASE + "/helper.candidate-" + suffix
    backup = BASE + "/helper.before-" + suffix
    remote(ssh, "set -eu; umask 077; set -C; cat > " + candidate + "; chmod 0755 " + candidate, data=data)
    if remote(ssh, "sha256sum " + candidate).split()[0] != digest:
        raise ValueError("Uploaded candidate differs; original helper remains running")
    preflight = remote(ssh, shlex.join(["/bin/busybox", "start-stop-daemon", "-S", "-c",
                       "nas-sync-test:everyone", "-x", candidate, "--", "-config", BASE + "/helper.json", "-check-config"]))
    print(json.dumps({"phase": "candidate-preflight", "result": json.loads(preflight)}), flush=True)
    # Preserve the old inode, and verify it again before stopping the service.
    remote(ssh, "set -eu; test \"$(sha256sum " + BASE + "/helper | cut -d ' ' -f 1)\" = " +
           args.expected_sha256 + "; ln " + BASE + "/helper " + backup)
    old_pid = remote(ssh, "cat " + PID).strip()
    if not re.fullmatch(r"[1-9][0-9]*", old_pid):
        raise ValueError("Unexpected service PID; original helper remains running")
    remote(ssh, BASE + "/service.sh stop")
    # The launcher joins its child before exiting. Do not replace a live binary
    # or restart while the old process may still own the publication store.
    for _ in range(20):
        gone = remote(ssh, "if kill -0 " + old_pid + " 2>/dev/null; then echo running; else echo gone; fi").strip()
        if gone == "gone":
            break
        time.sleep(0.5)
    else:
        raise RuntimeError("Old launcher has not exited; candidate and backup retained for inspection")
    remote(ssh, "set -eu; mv " + candidate + " " + BASE + "/helper; " + BASE + "/service.sh start")
    time.sleep(1)
    remote(ssh, BASE + "/service.sh status")
    print(json.dumps({"phase": "updated", "sha256": digest, "backup": backup,
                      "launcherPID": remote(ssh, "cat " + PID).strip()}), flush=True)


if __name__ == "__main__":
    from legacy_profile import require_configured
    require_configured()
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--session-dir", type=Path, required=True)
    parser.add_argument("--helper", type=Path, required=True)
    parser.add_argument("--expected-sha256", required=True)
    parser.add_argument("--install", action="store_true")
    run(parser.parse_args())
