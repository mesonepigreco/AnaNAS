#!/usr/bin/python3
"""Explicit disposable QNAP TLS helper test through an authenticated setup master.

Without --write, inspect only exact parent/account/network metadata and local
binary files. With --write, create one unique private child, upload two bounded
executables and newly generated test TLS identity, run the helper for 45 seconds
as UID1000/GID100, then remove that child only after the launcher has exited.
This never reads a NAS password, changes production data or installs a daemon.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import select
import shlex
import subprocess
import tempfile
import time

from run_native_probe import binary_payload, canonical_test_path, remote, session_options


def scope_paths(parent, work):
    path = Path(work)
    if str(path) != work or path.parent != Path(parent) or not re.fullmatch(r"ananas-helper-test-[a-f0-9]{32}", path.name):
        raise ValueError("Helper writes must stay in the newly created disposable child")
    return [work] + [str(path / name) for name in ("root", "state", "helper", "launcher", "helper.json", "server.pem", "server.key", "ca.pem")]


def confirmed_stopped(process, report):
    # A failed/disconnected local SSH process does not prove that the remote
    # helper stopped. Require successful launcher exit plus the helper's report.
    event = report.get("helperProcess", {})
    return process is not None and process.poll() == 0 and event.get("event") == "stopped" and event.get("uid") == 1000 and event.get("gid") == 100


def certs(directory):
    def openssl(*args):
        return subprocess.check_output(["openssl", *args], cwd=directory, stderr=subprocess.PIPE, timeout=15)
    openssl("req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes", "-keyout", "ca.key", "-out", "ca.pem", "-subj", "/CN=anaNAS-disposable-test-CA", "-days", "1")
    for name, usage, address in (("server", "serverAuth", "10.23.42.30"), ("client", "clientAuth", "10.23.42.17")):
        openssl("req", "-new", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes", "-keyout", name+".key", "-out", name+".csr", "-subj", "/CN=anaNAS-disposable-"+name)
        (directory / (name+".ext")).write_text("subjectAltName=IP:"+address+"\nextendedKeyUsage="+usage+"\nkeyUsage=digitalSignature\n")
        openssl("x509", "-req", "-in", name+".csr", "-CA", "ca.pem", "-CAkey", "ca.key", "-CAcreateserial", "-out", name+".pem", "-days", "1", "-extfile", name+".ext")
    for name in ("server.pem", "server.key", "client.pem", "client.key", "ca.pem"):
        os.chmod(directory / name, 0o600)
    return {name: hashlib.sha256(openssl("x509", "-in", name+".pem", "-outform", "DER")).hexdigest() for name in ("server", "client")}


def wait_ready(process):
    deadline = time.monotonic()+20
    data = b""
    while time.monotonic() < deadline:
        if not select.select([process.stdout], [], [], max(0, deadline-time.monotonic()))[0]:
            break
        chunk = os.read(process.stdout.fileno(), 4096)
        if not chunk:
            break
        data += chunk
        if len(data) > 65536:
            raise ValueError("Helper startup output exceeded bound")
        for line in data.splitlines():
            try:
                event = json.loads(line)
            except (ValueError, UnicodeDecodeError):
                continue
            if event.get("event") == "serving":
                return data
    raise RuntimeError("Helper did not report serving: "+data.decode(errors="replace"))


def run(args):
    ssh = session_options(args.session_dir)
    helper = binary_payload(args.helper)
    launcher = binary_payload(args.launcher)
    parent = canonical_test_path(remote(ssh, "set -eu; cd /share/nas-sync-test/nas-sync-capability-test; printf 'ANANAS_TEST_DIR=%s\\n' \"$(pwd -P)\""))
    identity = remote(ssh, "id -u nas-sync-test; id -g nas-sync-test").splitlines()
    if identity != ["1000", "100"]:
        raise ValueError("Dedicated helper identity changed")
    print(json.dumps({"phase": "plan", "parent": parent, "helperBytes": len(helper), "launcherBytes": len(launcher), "estimatedTotalFixtureBytes": 32*1024*1024, "lifetimeSeconds": 45, "write": args.write}), flush=True)
    if not args.write:
        return
    nonce = secrets.token_hex(16)
    work = parent+"/ananas-helper-test-"+nonce
    owned = scope_paths(parent, work)
    namespace, client_id = secrets.token_hex(32), secrets.token_hex(32)
    report = {"scope": "Disposable QNAP native TLS helper; not production autosync acceptance", "work": work, "helperSHA256": hashlib.sha256(helper).hexdigest(), "launcherSHA256": hashlib.sha256(launcher).hexdigest(), "setupBinaryBytes": len(helper)+len(launcher), "network": {"NAS": "10.23.42.30", "NASInterface": "eth0", "client": "10.23.42.17", "clientInterface": "enp1s0", "prefix": "10.23.42.0/24", "listenerPort": 8742}, "limitations": ["Fixed explicit 512 KiB fixture, not production autosync acceptance", "Encrypted stream counters exclude TCP/IP headers and retransmissions", "Helper CPU/RSS exclude the launcher and SSH processes", "No disconnect, killed-process or power-loss test", "No production writes, egress rules, daemon installation or out-of-band NAS observation"]}
    process = None
    created = False
    with tempfile.TemporaryDirectory(prefix="ananas-helper-client-") as local:
        directory = Path(local)
        pins = certs(directory)
        profile = {"root": work+"/root", "stateDir": work+"/state", "namespace": namespace, "listen": "10.23.42.30:8742", "interface": "eth0", "prefix": "10.23.42.0/24", "peers": ["10.23.42.17"], "uid": 1000, "gid": 100, "groups": [100], "certificate": work+"/server.pem", "privateKey": work+"/server.key", "clientCA": work+"/ca.pem", "clients": {pins["client"]: client_id}, "exclusions": [], "maxConnections": 2, "maxFileBytes": 1024*1024, "maxBatchBytes": 2*1024*1024, "readBytesPerSecond": 2*1024*1024}
        profile.update(maxCacheBytes=32*1024*1024, maxCacheEntries=128)
        report["publicationBudget"] = {"maxBytes": profile["maxCacheBytes"], "maxEntries": profile["maxCacheEntries"]}
        print(json.dumps({"phase": "disposable-scope", "paths": owned}), flush=True)
        try:
            remote(ssh, "umask 077; mkdir "+shlex.quote(work)+" && mkdir "+shlex.quote(work+"/root")+" "+shlex.quote(work+"/state"))
            created = True
            payloads = {"helper": helper, "launcher": launcher, "helper.json": json.dumps(profile).encode()}
            payloads.update({name: (directory/name).read_bytes() for name in ("server.pem", "server.key", "ca.pem")})
            for name, data in payloads.items():
                path = work+"/"+name
                mode = "0700" if name in ("helper", "launcher") else "0600"
                remote(ssh, "umask 077; set -C; cat > "+shlex.quote(path)+" && chmod "+mode+" "+shlex.quote(path), data=data)
                # Hash only these exact newly uploaded setup files. Key digests
                # are compared privately and never printed or recorded.
                digest = remote(ssh, "sha256sum "+shlex.quote(path)).split()[0]
                if digest != hashlib.sha256(data).hexdigest():
                    raise ValueError("Uploaded helper setup file digest mismatch")
            remote(ssh, "chown 1000:100 "+" ".join(map(shlex.quote, owned)))
            check = ["/bin/busybox", "start-stop-daemon", "-S", "-c", "nas-sync-test:everyone", "-x", work+"/helper", "--", "-config", work+"/helper.json", "-check-config"]
            report["nativePreflight"] = json.loads(remote(ssh, shlex.join(check)))
            command = [work+"/launcher", "-helper", work+"/helper", "-config", work+"/helper.json", "-listen", profile["listen"], "-interface", "eth0", "-prefix", profile["prefix"], "-peer", "10.23.42.17", "-uid", "1000", "-gid", "100", "-write-test", "-lifetime", "45s"]
            process = subprocess.Popen(ssh+[shlex.join(command)], stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
            startup = wait_ready(process)
            client = [str(args.client), "-endpoint", profile["listen"], "-source", "10.23.42.17", "-interface", "enp1s0", "-prefix", profile["prefix"], "-namespace", namespace, "-client-id", client_id, "-server-pin", pins["server"], "-certificate", str(directory/"client.pem"), "-key", str(directory/"client.key"), "-ca", str(directory/"ca.pem")]
            for phase, extra in (("readOnly", []), ("write", ["-write"])):
                result = subprocess.run(client+extra, check=True, capture_output=True, timeout=50)
                report[phase] = json.loads(result.stdout)
                print(json.dumps({"phase": phase, "result": report[phase]}), flush=True)
            tail = process.communicate(timeout=55)[0]
            if process.returncode != 0:
                raise RuntimeError("Helper launcher did not exit successfully: "+tail.decode(errors="replace"))
            for line in (startup+tail).splitlines():
                try:
                    event = json.loads(line)
                except (ValueError, UnicodeDecodeError):
                    continue
                if event.get("event") == "stopped":
                    report["helperProcess"] = event
            if "helperProcess" not in report:
                raise RuntimeError("Helper did not report process completion")
        finally:
            if created and (process is None or confirmed_stopped(process, report)):
                # This is the one new private child, never its pre-existing
                # parent or a production directory. A live/uncertain process
                # preserves the child for explicit recovery instead of deleting.
                scope_paths(parent, work)
                remote(ssh, "rm -rf "+shlex.quote(work)+" && test ! -e "+shlex.quote(work))
                report["cleaned"] = True
            elif created:
                report["cleaned"] = False
                print(json.dumps({"phase": "preserved-live-helper", "work": work, "localSSHProcess": process.pid}), flush=True)
    print(json.dumps(report), flush=True)
    if args.report:
        args.report.write_text(json.dumps(report, indent=2)+"\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--session-dir", type=Path, required=True)
    parser.add_argument("--helper", type=Path, required=True)
    parser.add_argument("--launcher", type=Path, required=True)
    parser.add_argument("--client", type=Path, required=True)
    parser.add_argument("--write", action="store_true")
    parser.add_argument("--report", type=Path)
    run(parser.parse_args())


if __name__ == "__main__":
    from legacy_profile import require_configured
    require_configured()
    main()
