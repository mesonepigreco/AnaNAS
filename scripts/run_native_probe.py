#!/usr/bin/python3
"""Run the native anaNAS probe through an existing user-authenticated SSH master.

Without --write this performs only route/session and exact NAS directory metadata
checks and prints the scoped plan. --write uploads one bounded ARM64 executable
to a new private child, runs its read-only check first, then its disposable write
probe, and removes that exact uploaded file/directory. No password is requested,
received or read by this script. This is setup validation, never background sync.
"""
import argparse
import hashlib
import json
import os
import re
from pathlib import Path
import shlex
import stat
import struct
import subprocess
import sys

NAS = "admin@10.23.42.30"
TEST_LINK = "/share/nas-sync-test/nas-sync-capability-test"
MAX_BINARY = 16 * 1024 * 1024


def ownership_paths(test_dir, tool_dir):
    """Only the mktemp-created immediate child and its probe, never its parent."""
    parent = Path(test_dir)
    child = Path(tool_dir)
    if str(child) != tool_dir or child.parent != parent or not re.fullmatch(r"ananas-probe-tool\.[A-Za-z0-9]{6}", child.name):
        raise ValueError("Ownership change must target only the new private tool child")
    return [str(child), str(child / "probe")]


def session_options(directory):
    info = directory.lstat()
    if not directory.is_absolute() or not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
        raise ValueError("Session must be an owned absolute private directory")
    control = directory / "control"
    info = control.lstat()
    if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.getuid():
        raise ValueError("User-authenticated SSH control socket is unavailable")
    routes = json.loads(subprocess.check_output(["ip", "-j", "-4", "route", "get", "10.23.42.30"], text=True))
    if len(routes) != 1 or routes[0].get("dev") != "enp1s0" or routes[0].get("gateway") or routes[0].get("prefsrc") != "10.23.42.17":
        raise ValueError("Expected direct enp1s0 route is unavailable")
    base = ["ssh", "-F", "/dev/null", "-S", str(control)]
    subprocess.run(base + ["-O", "check", NAS], check=True, capture_output=True, timeout=5)
    return base + ["-o", "BatchMode=yes", "-o", "ProxyCommand=false", "-o", "ClearAllForwardings=yes", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", NAS]


def canonical_test_path(output):
    paths = [line.removeprefix("ANANAS_TEST_DIR=") for line in output.splitlines() if line.startswith("ANANAS_TEST_DIR=")]
    if len(paths) != 1:
        raise ValueError("NAS did not return one canonical disposable directory")
    path = Path(paths[0])
    if not path.is_absolute() or str(path) != paths[0] or path.parts[:2] != ("/", "share") or path.parts[-2:] != ("nas-sync-test", "nas-sync-capability-test") or ".." in path.parts:
        raise ValueError("Unexpected native NAS test path")
    return str(path)


def binary_payload(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, "rb") as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or not 64 <= info.st_size <= MAX_BINARY:
            raise ValueError("Expected an owned ARM64 probe executable no larger than 16 MiB")
        data = source.read(MAX_BINARY + 1)
    if len(data) != info.st_size or data[:6] != b"\x7fELF\x02\x01" or struct.unpack("<H", data[18:20])[0] != 183:
        raise ValueError("Probe must be a stable Linux little-endian ARM64 ELF executable")
    return data


def remote(ssh, command, *, data=None, timeout=75):
    return subprocess.run(ssh + [command], input=data, check=True, capture_output=True, timeout=timeout).stdout.decode("utf-8")


def run(args):
    ssh = session_options(args.session_dir)
    binary = binary_payload(args.binary)
    preflight = "set -eu; cd " + shlex.quote(TEST_LINK) + "; printf 'ANANAS_TEST_DIR=%s\\n' \"$(pwd -P)\"; uname -srm"
    test_dir = canonical_test_path(remote(ssh, preflight))
    if args.as_sync_user:
        identity = remote(ssh, "id -u nas-sync-test; id -g nas-sync-test").splitlines()
        if identity != ["1000", "100"]:
            raise ValueError("Dedicated NAS sync account identity changed")
    print(json.dumps({"phase": "plan", "testDir": test_dir, "binaryBytes": len(binary), "estimatedFixtureBytes": 8 * 1024 * 1024, "write": args.write, "asSyncUser": args.as_sync_user}), flush=True)
    if not args.write:
        return
    template = test_dir + "/ananas-probe-tool.XXXXXX"
    tool_dir = remote(ssh, "umask 077; mktemp -d " + shlex.quote(template)).strip()
    # Never use an unexpected remote string in cleanup, even if creation output
    # was contaminated by a shell banner or another process.
    if not tool_dir.startswith(test_dir + "/ananas-probe-tool.") or "/" in tool_dir[len(test_dir) + 1:] or len(tool_dir) != len(template):
        raise ValueError("Unexpected private upload directory; inspect the returned setup operation")
    executable = tool_dir + "/probe"
    cleanup_allowed = True
    try:
        remote(ssh, "umask 077; set -C; cat > " + shlex.quote(executable) + " && chmod 0700 " + shlex.quote(executable), data=binary)
        returned = remote(ssh, "sha256sum " + shlex.quote(executable)).split()
        if not returned or returned[0] != hashlib.sha256(binary).hexdigest():
            raise ValueError("Uploaded executable digest mismatch")
        command = shlex.quote(executable) + " -test-dir " + shlex.quote(test_dir)
        if args.as_sync_user:
            owned = ownership_paths(test_dir, tool_dir)
            print(json.dumps({"phase": "private-tool-ownership", "paths": owned, "uid": 1000, "gid": 100}), flush=True)
            remote(ssh, "chown 1000:100 " + " ".join(shlex.quote(path) for path in owned))
            command = "/bin/busybox start-stop-daemon -S -c nas-sync-test:everyone -x " + shlex.quote(executable) + " -- -expect-uid 1000 -expect-gid 100 -test-dir " + shlex.quote(test_dir)
        for phase, flags in (("native-read-only", ""), ("native-write", " -write")):
            output = remote(ssh, command + flags)
            report = json.loads(output)
            print(json.dumps({"phase": phase, "report": report}), flush=True)
            if report.get("error"):
                raise ValueError("Native probe reported failure")
            if args.as_sync_user and (report.get("uid") != 1000 or report.get("gid") != 100 or any(group != 100 for group in report.get("groups", []))):
                raise ValueError("Native probe did not run as the dedicated non-admin account")
    except subprocess.TimeoutExpired:
        # An expired local observation is not proof that the remote process
        # exited. Keep its exact executable directory for investigation; do not
        # remove/restart a possibly live probe based only on that timeout.
        cleanup_allowed = False
        print(json.dumps({"phase": "remote-completion-unverified", "toolDir": tool_dir}), flush=True)
        raise
    finally:
        # No recursive cleanup and no production/share-root writes. The probe
        # itself owns cleanup of the unique fixture child it creates.
        if cleanup_allowed:
            remote(ssh, "rm -f " + shlex.quote(executable) + " && rmdir " + shlex.quote(tool_dir))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--session-dir", type=Path, required=True)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--write", action="store_true")
    parser.add_argument("--as-sync-user", action="store_true", help="run the scoped probe in the foreground as nas-sync-test (UID 1000/GID 100), never as an administrator")
    args = parser.parse_args()
    try:
        run(args)
    except subprocess.CalledProcessError as error:
        # Commands are fixed setup/probe operations; stdout/stderr contain no
        # password because BatchMode forbids interactive authentication.
        print(json.dumps({"error": "NAS setup/probe command failed", "exitCode": error.returncode, "stdout": (error.stdout or b"").decode("utf-8", "replace")[:8192], "stderr": (error.stderr or b"").decode("utf-8", "replace")[:4096]}), file=sys.stderr)
        return 1
    except (OSError, ValueError, subprocess.TimeoutExpired) as error:
        print(json.dumps({"error": str(error)}), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    from legacy_profile import require_configured
    require_configured()
    raise SystemExit(main())
