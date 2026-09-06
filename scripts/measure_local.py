#!/usr/bin/env python3
"""Reproducible Linux local-observation measurement, using only disposable data.

Network counters are namespace-wide, not attributable to nas-sync. For transport
acceptance additionally capture traffic in a dedicated network namespace on the
real deployment. This harness never contacts or writes a NAS.
"""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import time


def sample(pid):
    text = Path(f"/proc/{pid}/stat").read_text()
    fields = text[text.rfind(")") + 2:].split()
    tick = os.sysconf("SC_CLK_TCK")
    io = dict(line.split(": ") for line in Path(f"/proc/{pid}/io").read_text().splitlines())
    return {
        "cpu_seconds": (int(fields[11]) + int(fields[12])) / tick,
        "rss_bytes": int(fields[21]) * os.sysconf("SC_PAGE_SIZE"),
        "read_bytes": int(io["read_bytes"]),
        "write_bytes": int(io["write_bytes"]),
    }


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--binary", required=True)
    p.add_argument("--idle-seconds", type=float, default=30)
    p.add_argument("--files", type=int, default=1000)
    args = p.parse_args()
    if args.idle_seconds <= 0 or not 1 <= args.files <= 100000:
        p.error("positive idle duration and 1–100000 files required")
    binary = str(Path(args.binary).resolve())
    with tempfile.TemporaryDirectory(prefix="nas-sync-measure-") as tmp:
        base = Path(tmp)
        root = base / "local"
        root.mkdir()
        for i in range(args.files):
            (root / f"file-{i:06}.txt").write_text("small payload\n")
        cfg = {
            "local": {"root": str(root)},
            "nas": {"mountPoint": str(base / "unmounted-nas")},
            "stateDir": str(base / "state"),
            # Accelerates setup only; idle measurement begins after scan completes.
            "limits": {"scanOpsPerSecond": 10000},
        }
        config = base / "config.json"
        config.write_text(json.dumps(cfg))
        snapshot = subprocess.run([binary, "-config", str(config), "-scan-once"], check=True, text=True, capture_output=True)
        result = {"files": args.files, "snapshot": json.loads(snapshot.stdout)}
        log = base / "daemon.log"
        with log.open("w") as output:
            proc = subprocess.Popen([binary, "-config", str(config)], stdout=output, stderr=output)
            try:
                deadline = time.monotonic() + max(30, args.files / 500)
                while "local index ready" not in log.read_text():
                    if proc.poll() is not None:
                        raise RuntimeError(log.read_text())
                    if time.monotonic() > deadline:
                        raise RuntimeError("startup scan did not complete before timeout")
                    time.sleep(0.1)
                before = sample(proc.pid)
                start = time.monotonic()
                time.sleep(args.idle_seconds)
                after = sample(proc.pid)
                elapsed = time.monotonic() - start
                result["idle"] = {"seconds": elapsed, "cpu_percent_one_core": 100 * (after["cpu_seconds"] - before["cpu_seconds"]) / elapsed,
                                  "rss_bytes": after["rss_bytes"], "disk_read_bytes": after["read_bytes"] - before["read_bytes"],
                                  "disk_write_bytes": after["write_bytes"] - before["write_bytes"]}
                before = sample(proc.pid)
                start = time.monotonic()
                for i in range(500):
                    (root / "hot.txt").write_text(str(i))
                time.sleep(2)
                after = sample(proc.pid)
                result["burst"] = {"writes": 500, "seconds": time.monotonic() - start,
                                   "cpu_seconds": after["cpu_seconds"] - before["cpu_seconds"], "rss_bytes": after["rss_bytes"]}
            finally:
                proc.send_signal(signal.SIGTERM)
                try:
                    proc.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()
        result["limits"] = "Local metadata only; no content hashing or transport. Disk counters exclude page-cache reads. No wire-traffic claim."
        print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
