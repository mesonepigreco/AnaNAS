#!/usr/bin/python3
"""User-operated temporary SSH setup session for the selected QNAP deployment.

Run in a desktop terminal. Password entry is handled by ssh directly in the TTY;
this script neither receives nor records it. The session has no forwarding and
expires after 30 minutes without a client. Close sooner with ssh -S SOCKET -O exit.
No remote command or installation is performed by opening the session.
"""
import argparse
import json
import os
from pathlib import Path
import stat
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--session-dir", type=Path, required=True)
    args = parser.parse_args()
    directory = args.session_dir
    info = directory.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
        raise ValueError("Session directory must be an owned private directory")
    if not directory.is_absolute() or (directory / "control").exists():
        raise ValueError("A new absolute session directory is required")
    # No DNS/proxy/user SSH configuration; reject a gateway or changed interface.
    routes = json.loads(subprocess.check_output(["ip", "-j", "-4", "route", "get", "10.23.42.30"], text=True))
    if len(routes) != 1 or routes[0].get("dev") != "enp1s0" or routes[0].get("gateway") or routes[0].get("prefsrc") != "10.23.42.17":
        raise ValueError("Expected direct enp1s0 route to 10.23.42.30 is unavailable")
    print("NAS setup: sign in as admin directly below. Password entry stays in this terminal.", flush=True)
    command = ["ssh", "-F", "/dev/null", "-M", "-S", str(directory / "control"),
               "-o", "ControlPersist=1800", "-o", "ForwardAgent=no", "-o", "ForwardX11=no",
               "-o", "ClearAllForwardings=yes", "-o", "StrictHostKeyChecking=ask",
               "-o", "BindInterface=enp1s0", "-o", "ConnectTimeout=10",
               "-b", "10.23.42.17", "-fN", "admin@10.23.42.30"]
    result = subprocess.run(command, check=False)
    if result.returncode == 0:
        print("NAS setup session is ready. Tell Codex the login succeeded.", flush=True)
    else:
        print("NAS login did not complete. Tell Codex what happened (without any password).", flush=True)
    input("Press Enter to close this terminal window. ")
    return result.returncode


if __name__ == "__main__":
    from legacy_profile import require_configured
    require_configured()
    raise SystemExit(main())
