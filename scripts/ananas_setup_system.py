#!/usr/bin/python3
"""Privileged setup entry point and boot-time mount gate for wizard instances.

Receives a bounded JSON plan on stdin through pkexec. It never sources user
shell text or prints secrets. Existing system installation paths are refused.
"""
import argparse
import ipaddress
import json
import os
from pathlib import Path
import pwd
import re
import stat
import subprocess
import sys


def execute(argv, timeout=30):
    return subprocess.run(argv, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout).stdout


def validate(plan):
    key = plan["id"]
    if not re.fullmatch(r"[a-f0-9]{16}", key):
        raise ValueError("invalid instance ID")
    uid, gid = plan["uid"], plan["gid"]
    if type(uid) is not int or type(gid) is not int or uid <= 0 or gid <= 0:
        raise ValueError("a non-root PC account is required")
    account = pwd.getpwuid(uid)
    if account.pw_name != plan["username"] or account.pw_gid != gid:
        raise ValueError("PC account changed")
    route = plan["route"]
    host, source = ipaddress.IPv4Address(plan["host"]), ipaddress.IPv4Address(route["source"])
    prefix = ipaddress.IPv4Network(route["prefix"])
    if not host.is_private or not source.is_private or host == source or host not in prefix or source not in prefix or host.is_loopback:
        raise ValueError("invalid direct LAN addresses")
    if route["host"] != str(host) or not re.fullmatch(r"[a-zA-Z0-9_.-]{1,15}", route["interface"]):
        raise ValueError("invalid interface")
    if not re.fullmatch(r"[^/\\\n\r\x00,]{1,80}", plan["share"]):
        raise ValueError("unsupported SMB share name")
    if plan["mountPoint"] != "/mnt/ananas-" + key or type(plan["port"]) is not int or not 18742 <= plan["port"] < 18842:
        raise ValueError("invalid mount or helper port")
    if "PKEXEC_UID" in os.environ and int(os.environ["PKEXEC_UID"]) != uid:
        raise ValueError("plan does not belong to the caller")
    return plan


def private_new(path, data, mode=0o600):
    fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW, mode)
    with os.fdopen(fd, "wb") as stream:
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())


def direct_lan(plan):
    expected = plan["route"]
    route = json.loads(execute(["ip", "-j", "-4", "route", "get", plan["host"]]))
    if len(route) != 1 or route[0].get("gateway") or route[0].get("via") or route[0].get("dev") != expected["interface"] or route[0].get("prefsrc") != expected["source"]:
        raise ValueError("configured direct LAN is unavailable")
    path = Path("/sys/class/net") / expected["interface"]
    if not (path / "device").exists() or "/virtual/" in str(path.resolve()):
        raise ValueError("physical LAN required")
    addresses = json.loads(execute(["ip", "-j", "-4", "address", "show", "dev", expected["interface"]]))
    prefix = ipaddress.IPv4Network(expected["prefix"])
    if len(addresses) != 1 or not any(a.get("local") == expected["source"] and a.get("prefixlen") == prefix.prefixlen for a in addresses[0].get("addr_info", [])):
        raise ValueError("LAN prefix changed")


def mount(plan):
    direct_lan(plan)
    target = Path(plan["mountPoint"])
    if target.resolve() != target or not target.is_dir():
        raise ValueError("mount target was changed")
    source = "//" + plan["host"] + "/" + plan["share"]
    for row in Path("/proc/self/mountinfo").read_text().splitlines():
        left, right = row.split(" - ", 1)
        if left.split()[4] == str(target):
            fs = right.split()
            decoded = re.sub(r"\\([0-7]{3})", lambda m: chr(int(m[1], 8)), fs[1])
            if fs[0] != "cifs" or decoded != source or "seal" not in fs[2].split(","):
                raise ValueError("another filesystem occupies the mount point")
            return
    credential = Path("/etc/ananas-setup") / (plan["id"] + ".cred")
    info = credential.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o600:
        raise ValueError("private root-owned credential required")
    execute(["mount", "-t", "cifs", source, str(target), "-o",
             f"credentials={credential},vers=3.1.1,seal,cache=strict,serverino,actimeo=1,uid={plan['uid']},gid={plan['gid']},file_mode=0600,dir_mode=0700,nosuid,nodev,noexec"], timeout=35)


def install(plan):
    validate(plan)
    for name in ("nasUsername", "password"):
        if not isinstance(plan[name], str) or not plan[name] or any(c in plan[name] for c in "\n\r\0"):
            raise ValueError("invalid SMB credential")
    direct_lan(plan)
    key = plan["id"]
    base = Path("/etc/ananas-setup")
    base.mkdir(mode=0o700, exist_ok=True)
    info = base.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o700:
        raise ValueError("private root-owned setup directory required")
    name = "ananas-mount-" + key
    paths = [base / (key + suffix) for suffix in (".json", ".cred", ".py", ".nft")]
    unit = Path("/etc/systemd/system") / (name + ".service")
    if any(path.exists() for path in [*paths, unit, Path(plan["mountPoint"])]):
        raise ValueError("this installation already has staged system files; inspect them before retrying")
    execute(["nft", "--version"])
    execute(["mount.cifs", "-V"])
    rules = f'''table inet ananas_{key} {{
 chain output {{
  type filter hook output priority -10; policy accept;
  meta skuid {plan['uid']} tcp dport {plan['port']} ip daddr {plan['host']} oifname "{plan['route']['interface']}" accept
  meta skuid {plan['uid']} tcp dport {plan['port']} reject
  ip daddr {plan['host']} tcp dport 445 oifname "{plan['route']['interface']}" accept
  ip daddr {plan['host']} tcp dport 445 reject
 }}
}}
'''
    private_new(paths[3], rules.encode())
    execute(["nft", "--check", "-f", str(paths[3])])
    credential = f"username={plan.pop('nasUsername')}\npassword={plan.pop('password')}\n".encode()
    private_new(paths[1], credential)
    private_new(paths[0], json.dumps(plan).encode())
    private_new(paths[2], Path(__file__).read_bytes(), 0o700)
    Path(plan["mountPoint"]).mkdir(mode=0o700)
    # Each boot loads the dedicated rules once, then the bounded failed mount
    # retries until this exact LAN is available. Successful mounts have no timer.
    service = f'''[Unit]
Description=anaNAS LAN mount {key}
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0
[Service]
Type=oneshot
ExecStart=/usr/bin/python3 {paths[2]} --mount {paths[0]}
RemainAfterExit=yes
Restart=on-failure
RestartSec=60
TimeoutStartSec=45
UMask=0077
[Install]
WantedBy=multi-user.target
'''
    private_new(unit, service.encode(), 0o644)
    execute(["systemctl", "daemon-reload"])
    execute(["systemctl", "enable", "--now", name + ".service"], timeout=60)
    execute(["loginctl", "enable-linger", plan["username"]])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--install", action="store_true")
    mode.add_argument("--mount", type=Path)
    args = parser.parse_args()
    if os.geteuid() != 0:
        raise ValueError("administrator authentication required")
    if args.install:
        raw = sys.stdin.buffer.read(65537)
        if len(raw) > 65536:
            raise ValueError("setup request is too large")
        install(json.loads(raw))
    else:
        info = args.mount.lstat()
        if info.st_uid != 0 or not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600:
            raise ValueError("private system configuration required")
        plan = validate(json.loads(args.mount.read_bytes()))
        table = "ananas_" + plan["id"]
        if subprocess.run(["nft", "list", "table", "inet", table], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode:
            execute(["nft", "-f", str(args.mount.with_suffix(".nft"))])
        mount(plan)


if __name__ == "__main__":
    try:
        main()
    except Exception:
        # Deliberately omit subprocess output/JSON, which can include passwords.
        print("anaNAS setup: system preparation failed. Check the LAN, required packages and existing instance files; no existing sync configuration was replaced.", file=sys.stderr)
        raise SystemExit(1)
