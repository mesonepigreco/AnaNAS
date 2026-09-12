#!/usr/bin/python3
"""Read CPU counters for this user's GNOME Shell and nas-sync user services.

Does not start/stop services, inspect window contents or contact the NAS.
Percentages use one logical CPU as 100%; this is a short sample, not a guarantee.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import time


def sample(pid):
    fields = Path(f"/proc/{pid}/stat").read_text().rsplit(") ", 1)[1].split()
    return int(fields[11]) + int(fields[12]), fields[19]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--seconds", type=int, default=15)
    args = parser.parse_args()
    if not 1 <= args.seconds <= 60:
        parser.error("--seconds must be between 1 and 60")
    targets = {}
    shell = subprocess.run(["pgrep", "-u", str(os.getuid()), "-x", "gnome-shell"],
                           capture_output=True, text=True, check=False)
    for pid in shell.stdout.split():
        targets[f"gnome-shell:{pid}"] = int(pid)
    for unit in ("nas-sync.service", "nas-sync-indicator.service"):
        value = subprocess.check_output(["systemctl", "--user", "show", unit,
                                         "-p", "MainPID", "--value"], text=True).strip()
        if value and int(value) != 0:
            targets[unit] = int(value)
    before = {name: sample(pid) for name, pid in targets.items()}
    start = time.monotonic()
    time.sleep(args.seconds)
    seconds = time.monotonic() - start
    hz = os.sysconf("SC_CLK_TCK")
    rows = []
    for name, pid in targets.items():
        row = {"process": name, "pid": pid}
        try:
            ticks, birth = sample(pid)
            if before[name][1] != birth:
                raise ValueError("PID was reused")
            delta = ticks - before[name][0]
            row.update(cpu_seconds=delta / hz, cpu_percent_one_core=round(100 * delta / hz / seconds, 3))
        except (OSError, ValueError):
            row["unavailable"] = True
        rows.append(row)
    print(json.dumps({"seconds": seconds, "clock_ticks_per_second": hz, "processes": rows}, indent=2))


if __name__ == "__main__":
    main()
