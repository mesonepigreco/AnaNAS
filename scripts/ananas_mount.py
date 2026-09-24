#!/usr/bin/python3
"""Fixed, event-triggered LAN mount for the authorized anaNAS installation.

--check-lan reads only local route/interface metadata. --mount additionally
checks kernel mount metadata and lets mount.cifs use the existing root-only
credential file. This program never opens or prints credential contents.
"""
import argparse
import ipaddress
import json
import os
from pathlib import Path
import stat
import subprocess

NAS = "10.23.42.30"
DEVICE = "enp1s0"
PREFIX = "10.23.42.0/24"
TARGET = "/mnt/nasdir"
SHARE = "//10.23.42.30/Nasdir"


def output(*command):
    return subprocess.check_output(command, text=True, timeout=5)


def valid_lan(interfaces, routes, connected):
    if len(interfaces) != 1 or len(routes) != 1:
        return False
    interface, route = interfaces[0], routes[0]
    flags = set(interface.get("flags", []))
    if interface.get("ifname") != DEVICE or interface.get("operstate") != "UP" or not {"UP", "LOWER_UP"} <= flags or "POINTOPOINT" in flags:
        return False
    # The PC address is DHCP-assigned: accept whichever address the interface
    # currently holds in the direct subnet, provided the route to the NAS uses it.
    prefix = ipaddress.IPv4Network(PREFIX)
    held = {a.get("local") for a in interface.get("addr_info", [])
            if a.get("family") == "inet" and a.get("prefixlen") == prefix.prefixlen and ipaddress.IPv4Address(a.get("local")) in prefix}
    if route.get("dev") != DEVICE or route.get("prefsrc") not in held or route.get("gateway") or route.get("via") or route.get("type", "unicast") != "unicast":
        return False
    return any(r.get("dst") == PREFIX and r.get("dev") == DEVICE and r.get("scope") == "link" and not r.get("gateway") and not r.get("via") and r.get("type", "unicast") == "unicast" for r in connected)


def check_lan():
    physical = Path("/sys/class/net/" + DEVICE)
    if not (physical / "device").exists() or "/virtual/" in str(physical.resolve()):
        return False
    return valid_lan(json.loads(output("/usr/sbin/ip", "-j", "-4", "address", "show", "dev", DEVICE)),
                     json.loads(output("/usr/sbin/ip", "-j", "-4", "route", "get", NAS)),
                     # ip omits the dev field when output is filtered by dev.
                     # Read the local main table so policy can verify it explicitly.
                     json.loads(output("/usr/sbin/ip", "-j", "-4", "route", "show")))


def mounted():
    matches = []
    for line in Path("/proc/self/mountinfo").read_text().splitlines():
        left, right = line.split(" - ", 1)
        fields, fs = left.split(), right.split()
        if fields[4] == TARGET:
            matches.append((fields, fs))
    if not matches:
        return False
    if len(matches) != 1:
        raise ValueError("Ambiguous Nasdir mount")
    fields, fs = matches[0]
    options, super_options = set(fields[5].split(",")), set(fs[2].split(","))
    if fs[:2] != ["cifs", SHARE] or not {"rw", "nosuid", "nodev", "noexec"} <= options or not {"rw", "vers=3.1.1", "seal", "addr=" + NAS} <= super_options:
        raise ValueError("Existing Nasdir mount does not match the configured LAN share/options")
    return True


def mount():
    if os.geteuid() != 0:
        raise ValueError("Mount setup requires root")
    if not check_lan():
        raise ValueError("Configured direct physical LAN is unavailable")
    if mounted():
        print("anaNAS LAN mount is already ready")
        return
    credential = Path("/etc/nas-sync-test.cred").lstat()
    if not stat.S_ISREG(credential.st_mode) or credential.st_uid != 0 or stat.S_IMODE(credential.st_mode) != 0o600:
        raise ValueError("Expected existing root-only NAS credential file")
    target = Path(TARGET)
    if target.resolve() != target or not target.is_dir():
        raise ValueError("Expected existing canonical mount directory")
    subprocess.run(["/usr/bin/systemctl", "is-active", "--quiet", "ananas-mount-network.service"], check=True, timeout=5)
    subprocess.run(["/usr/bin/mount", "-t", "cifs", SHARE, TARGET, "-o",
                    "credentials=/etc/nas-sync-test.cred,vers=3.1.1,seal,cache=strict,serverino,actimeo=1,uid=1000,gid=1000,file_mode=0600,dir_mode=0700,nosuid,nodev,noexec"], check=True, timeout=30)
    if not mounted():
        raise ValueError("Mount command completed without the required mount")
    # Mount changes do not emit routing netlink events. Restart only the sync
    # daemon if its user manager already started, so its LAN gate re-evaluates.
    if Path("/run/user/1000/bus").exists():
        subprocess.run(["/usr/sbin/runuser", "-u", "example-user", "--", "/usr/bin/env",
                        "XDG_RUNTIME_DIR=/run/user/1000", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus",
                        "/usr/bin/systemctl", "--user", "--no-block", "try-restart", "nas-sync.service"], check=True, timeout=5)
    print("anaNAS LAN mount is ready; saved credentials used by mount.cifs")


if __name__ == "__main__":
    from legacy_profile import require_configured
    require_configured()
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_mutually_exclusive_group(required=True)
    modes.add_argument("--check-lan", action="store_true")
    modes.add_argument("--check-mount", action="store_true")
    modes.add_argument("--mount", action="store_true")
    args = parser.parse_args()
    try:
        if args.check_lan:
            raise SystemExit(0 if check_lan() else 1)
        if args.check_mount:
            raise SystemExit(0 if mounted() else 1)
        mount()
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        print("anaNAS mount:", error)
        raise SystemExit(1)
