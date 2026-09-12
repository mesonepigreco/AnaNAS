#!/usr/bin/python3
"""Verify only the previously prepared anaNAS-live-check.bin with --write.

Uses the running daemon, loopback controls and an existing authenticated setup
session. No recursive listing, new credentials, service restart or user-file
cleanup. Two single-byte edits remain in the named verification file.
"""
import argparse
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import shlex
import time

from run_native_probe import remote, session_options

ORIGINAL = "35ac9691bb90cc8edd0242ac105008dc4d00215a16d76509ff6fb3e8d83acdd8"
LOCAL = Path("/home/example-user/NASdir/.ananas-tests/anaNAS-live-check.bin")
NAS = "/share/EXAMPLE_VOLUME/Nasdir/.ananas-tests/anaNAS-live-check.bin"


def request(path, body=None, headers=None):
    conn = http.client.HTTPConnection("127.0.0.1", 8721, timeout=5)
    try:
        conn.request("GET" if body is None else "POST", path, body, headers or {})
        response = conn.getresponse()
        data = response.read(65537)
        if response.status != (200 if body is None else 204) or len(data) > 65536:
            raise ValueError("Unexpected local control response")
        return data
    finally:
        conn.close()


def status():
    return json.loads(request("/api/status"))


def pause(value):
    token = re.search(rb'<meta name="nas-sync-csrf" content="([a-f0-9]{64})">', request("/"))
    if token is None:
        raise ValueError("Local control token unavailable")
    request("/api/pause", json.dumps({"paused": value}), {"Content-Type":"application/json", "X-Nas-Sync-CSRF":token[1].decode()})


def digest(data):
    return hashlib.sha256(data).hexdigest()


def nas_hash(ssh):
    return remote(ssh, "sha256sum " + shlex.quote(NAS), timeout=10).split()[0]


def wait_for(check, seconds=35):
    start = time.monotonic()
    while time.monotonic() - start < seconds:
        if check():
            return round(time.monotonic() - start, 3)
        time.sleep(0.5)
    raise TimeoutError("The bounded live verification did not converge")


def run(args):
    ssh = session_options(args.session_dir)
    fd = os.open(LOCAL, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, "rb") as stream:
        info = os.fstat(stream.fileno())
        if info.st_size != 262144:
            raise ValueError("Prepared verification size differs")
        original = stream.read(262145)
    if digest(original) != ORIGINAL or nas_hash(ssh) != ORIGINAL:
        raise ValueError("Only the exact original verification file may be changed")
    initial = status()
    if not initial.get("automaticWrites") or initial.get("paused") or initial.get("syncReason") != "LAN sync active":
        raise ValueError("Running trial must be active and unpaused")
    report = {"local":str(LOCAL), "nas":NAS, "size":len(original), "initialSHA256":ORIGINAL, "before":initial}
    if not args.write:
        print(json.dumps(report, indent=2))
        return
    changed = bytearray(original)
    changed[8192] ^= 1
    pause(True)
    try:
        # Revalidate the exact inode's content before changing one byte.
        fd = os.open(LOCAL, os.O_RDWR | os.O_NOFOLLOW)
        with os.fdopen(fd, "r+b") as stream:
            if digest(stream.read(262145)) != ORIGINAL:
                raise ValueError("Verification file changed before local edit")
            stream.seek(8192)
            stream.write(changed[8192:8193])
            stream.flush()
            os.fsync(stream.fileno())
        time.sleep(5)
        if nas_hash(ssh) != ORIGINAL or not status()["paused"]:
            raise ValueError("Pause did not retain the old NAS version")
        report["pauseHeldSeconds"] = 5
    finally:
        pause(False)
    local_hash = digest(changed)
    report["uploadConvergedSeconds"] = wait_for(lambda: nas_hash(ssh) == local_hash)
    time.sleep(1)
    uploaded = status()
    report["afterUpload"] = uploaded
    report["uploadEncryptedBytes"] = uploaded["uploadBytes"] - initial["uploadBytes"]
    if report["uploadEncryptedBytes"] >= 131072:
        raise ValueError("Single-byte upload exceeded the 128 KiB verification bound")
    changed[131072] ^= 1
    command = ("set -eu; test \"$(sha256sum " + shlex.quote(NAS) + " | cut -d ' ' -f 1)\" = " + local_hash +
               "; printf '\\%03o' | /bin/busybox dd of=%s bs=1 seek=131072 count=1 conv=notrunc 2>/dev/null" % (changed[131072], shlex.quote(NAS)))
    remote(ssh, shlex.join(["/bin/busybox", "start-stop-daemon", "-S", "-n", "ananas-live-edit", "-c", "nas-sync-test:everyone", "-x", "/bin/sh", "--", "-c", command]))
    final_hash = digest(changed)
    report["downloadConvergedSeconds"] = wait_for(lambda: digest(LOCAL.read_bytes()) == final_hash)
    if nas_hash(ssh) != final_hash:
        raise ValueError("Final NAS bytes differ")
    time.sleep(1)
    downloaded = status()
    report["afterDownload"] = downloaded
    report["downloadEncryptedBytes"] = downloaded["downloadBytes"] - uploaded["downloadBytes"]
    if report["downloadEncryptedBytes"] >= 131072:
        raise ValueError("Single-byte download exceeded the 128 KiB verification bound")
    report["finalSHA256"] = final_hash
    report["scope"] = "One 256 KiB regular file, two one-byte edits; encrypted daemon TCP totals include TLS/metadata, exclude SSH verification and packet overhead. No conflict, directory, disconnect or power-failure proof."
    encoded = json.dumps(report, indent=2)+"\n"
    if args.evidence:
        with args.evidence.open("x") as out:
            out.write(encoded)
    print(encoded, end="")


if __name__ == "__main__":
    from legacy_profile import require_configured
    require_configured()
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--session-dir", type=Path, required=True)
    parser.add_argument("--write", action="store_true")
    parser.add_argument("--evidence", type=Path)
    run(parser.parse_args())
