#!/usr/bin/env python3
"""Trace a disposable local observer run; fail if it makes network syscalls.

Requires Linux strace and permission to trace a child process. This verifies only
local startup/idle observation, not future NAS transport or deployment routing.
"""
import argparse
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import tempfile
import time

p = argparse.ArgumentParser(description=__doc__)
p.add_argument("--binary", required=True)
args = p.parse_args()
binary = str(Path(args.binary).resolve())
with tempfile.TemporaryDirectory(prefix="nas-sync-network-") as tmp:
    base = Path(tmp)
    root = base / "local"
    root.mkdir()
    (root / "a").write_text("local only")
    config = base / "config.json"
    config.write_text(json.dumps({"local": {"root": str(root)}, "nas": {"mountPoint": str(base / "unmounted")}, "stateDir": str(base / "state")}))
    trace = base / "network.trace"
    log = base / "daemon.log"
    with log.open("w") as output:
        proc = subprocess.Popen(["strace", "-f", "-e", "trace=network", "-o", str(trace), binary, "-config", str(config)], stdout=output, stderr=output, start_new_session=True)
        try:
            deadline = time.monotonic() + 10
            while "local index ready" not in log.read_text():
                if proc.poll() is not None:
                    raise RuntimeError(log.read_text())
                if time.monotonic() > deadline:
                    raise RuntimeError("observer did not become ready")
                time.sleep(0.05)
            time.sleep(2)
        finally:
            if proc.poll() is None:
                os.killpg(proc.pid, signal.SIGTERM)
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.wait()
    # In a network-only syscall trace, any syscall-shaped line is unexpected.
    calls = [line for line in trace.read_text().splitlines() if re.search(r"\b[a-z][a-z0-9_]*\(", line)]
    print(json.dumps({"network_syscalls": len(calls), "scope": "local startup and two seconds idle", "calls": calls}, indent=2))
    if calls:
        raise SystemExit(1)
