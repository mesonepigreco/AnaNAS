#!/usr/bin/python3
"""Event- and timer-triggered LAN mount for the authorized anaNAS installation.

--check-lan reads only local route/interface metadata. --mount finds the NAS on
the direct subnet by its pinned helper certificate (its DHCP address may change),
remounts if it moved, and lets mount.cifs use the existing root-only credential
file. This program never opens or prints credential contents.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import socket
import ssl
import stat
import subprocess

NAS = "10.23.42.30"  # last known address; the NAS may move within PREFIX
DEVICE = "enp1s0"
PREFIX = "10.23.42.0/24"
TARGET = "/mnt/nasdir"
SHARE = "Nasdir"
HELPER_PORT = 8742
# SHA-256 of the NAS helper's DER leaf certificate (sync.serverFingerprint).
PIN = "0" * 64


def output(*command):
    return subprocess.check_output(command, text=True, timeout=5)


def held_addresses(interface):
    prefix = ipaddress.IPv4Network(PREFIX)
    return {a.get("local") for a in interface.get("addr_info", [])
            if a.get("family") == "inet" and a.get("prefixlen") == prefix.prefixlen and ipaddress.IPv4Address(a.get("local")) in prefix}


def valid_lan(interfaces, connected):
    """Both PC and NAS addresses are DHCP-assigned: require the physical
    interface to hold some address in the direct subnet and a link route."""
    if len(interfaces) != 1:
        return False
    interface = interfaces[0]
    flags = set(interface.get("flags", []))
    if interface.get("ifname") != DEVICE or interface.get("operstate") != "UP" or not {"UP", "LOWER_UP"} <= flags or "POINTOPOINT" in flags:
        return False
    if not held_addresses(interface):
        return False
    return any(r.get("dst") == PREFIX and r.get("dev") == DEVICE and r.get("scope") == "link" and not r.get("gateway") and not r.get("via") and r.get("type", "unicast") == "unicast" for r in connected)


def direct_route(interfaces, routes):
    """The route to the chosen NAS address stays on the direct link."""
    if len(interfaces) != 1 or len(routes) != 1:
        return False
    route = routes[0]
    return route.get("dev") == DEVICE and route.get("prefsrc") in held_addresses(interfaces[0]) and not route.get("gateway") and not route.get("via") and route.get("type", "unicast") == "unicast"


def interfaces():
    return json.loads(output("/usr/sbin/ip", "-j", "-4", "address", "show", "dev", DEVICE))


def check_lan():
    physical = Path("/sys/class/net/" + DEVICE)
    if not (physical / "device").exists() or "/virtual/" in str(physical.resolve()):
        return False
    # ip omits the dev field when output is filtered by dev. Read the local
    # main table so policy can verify it explicitly.
    return valid_lan(interfaces(), json.loads(output("/usr/sbin/ip", "-j", "-4", "route", "show")))


def is_nas(address, timeout=0.8):
    """True only when address presents the pinned helper certificate. The TLS
    1.3 server certificate is verified before any client credential is sent,
    so this needs no private key. SMB credentials go only to this address."""
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    context.check_hostname = False
    context.verify_mode = ssl.CERT_NONE
    context.minimum_version = ssl.TLSVersion.TLSv1_3
    try:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as raw:
            raw.setsockopt(socket.SOL_SOCKET, socket.SO_BINDTODEVICE, DEVICE.encode())
            raw.settimeout(timeout)
            raw.connect((str(address), HELPER_PORT))
            with context.wrap_socket(raw) as tls:
                certificate = tls.getpeercert(binary_form=True)
    except (OSError, ValueError):
        return False
    return certificate is not None and hashlib.sha256(certificate).hexdigest() == PIN


def find_nas(preferred, candidates=None, probe=is_nas):
    """Try known addresses first, then sweep the direct subnet."""
    for address in dict.fromkeys(a for a in preferred if a):
        if probe(address):
            return address
    if candidates is None:
        own = held_addresses(interfaces()[0])
        candidates = [str(a) for a in ipaddress.IPv4Network(PREFIX).hosts() if str(a) not in own]
    candidates = [a for a in candidates if a not in preferred]
    with ThreadPoolExecutor(max_workers=32) as pool:
        for address, found in zip(candidates, pool.map(probe, candidates)):
            if found:
                return address
    return None


def parse_mount(mountinfo):
    """Return the NAS address of the Nasdir mount, or None when unmounted."""
    matches = []
    for line in mountinfo.splitlines():
        left, right = line.split(" - ", 1)
        fields, fs = left.split(), right.split()
        if fields[4] == TARGET:
            matches.append((fields, fs))
    if not matches:
        return None
    if len(matches) != 1:
        raise ValueError("Ambiguous Nasdir mount")
    fields, fs = matches[0]
    options, super_options = set(fields[5].split(",")), set(fs[2].split(","))
    server, _, share = fs[1].removeprefix("//").partition("/")
    try:
        in_prefix = fs[1].startswith("//") and ipaddress.IPv4Address(server) in ipaddress.IPv4Network(PREFIX)
    except ValueError:
        in_prefix = False
    if fs[0] != "cifs" or not in_prefix or share != SHARE or not {"rw", "nosuid", "nodev", "noexec"} <= options or not {"rw", "vers=3.1.1", "seal", "addr=" + server} <= super_options:
        raise ValueError("Existing Nasdir mount does not match the configured LAN share/options")
    return server


def mounted():
    return parse_mount(Path("/proc/self/mountinfo").read_text())


def mount():
    if os.geteuid() != 0:
        raise ValueError("Mount setup requires root")
    if not check_lan():
        raise ValueError("Configured direct physical LAN is unavailable")
    current = mounted()
    nas = find_nas([current, NAS])
    if nas is None:
        raise ValueError("NAS with the pinned certificate was not found on the LAN")
    if not direct_route(interfaces(), json.loads(output("/usr/sbin/ip", "-j", "-4", "route", "get", nas))):
        raise ValueError("Route to the NAS leaves the direct LAN")
    if current == nas:
        print("anaNAS LAN mount is already ready")
        return
    credential = Path("/etc/nas-sync-test.cred").lstat()
    if not stat.S_ISREG(credential.st_mode) or credential.st_uid != 0 or stat.S_IMODE(credential.st_mode) != 0o600:
        raise ValueError("Expected existing root-only NAS credential file")
    target = Path(TARGET)
    if target.resolve() != target or not target.is_dir():
        raise ValueError("Expected existing canonical mount directory")
    subprocess.run(["/usr/bin/systemctl", "is-active", "--quiet", "ananas-mount-network.service"], check=True, timeout=5)
    if current:
        # The NAS moved: detach the mount of the old address (which may hang)
        # before mounting the verified new one.
        subprocess.run(["/usr/bin/umount", "-l", TARGET], check=True, timeout=30)
    subprocess.run(["/usr/bin/mount", "-t", "cifs", "//" + nas + "/" + SHARE, TARGET, "-o",
                    "credentials=/etc/nas-sync-test.cred,vers=3.1.1,seal,cache=strict,serverino,actimeo=1,uid=1000,gid=1000,file_mode=0600,dir_mode=0700,nosuid,nodev,noexec"], check=True, timeout=30)
    if mounted() != nas:
        raise ValueError("Mount command completed without the required mount")
    # Mount changes do not emit routing netlink events. Restart only the sync
    # daemon if its user manager already started, so its LAN gate re-evaluates.
    if Path("/run/user/1000/bus").exists():
        subprocess.run(["/usr/sbin/runuser", "-u", "example-user", "--", "/usr/bin/env",
                        "XDG_RUNTIME_DIR=/run/user/1000", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus",
                        "/usr/bin/systemctl", "--user", "--no-block", "try-restart", "nas-sync.service"], check=True, timeout=5)
    print("anaNAS LAN mount is ready at " + nas + "; saved credentials used by mount.cifs")


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
            raise SystemExit(0 if mounted() and is_nas(mounted()) else 1)
        mount()
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        print("anaNAS mount:", error)
        raise SystemExit(1)
