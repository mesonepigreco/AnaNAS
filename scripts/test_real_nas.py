#!/usr/bin/env python3
"""Run bounded capability checks against an explicitly selected NAS directory.

Read-only by default. The write phase requires --write and an existing disposable
--test-dir. Every object created by the write phase has a unique prefix and is
removed in a finally block unless --keep is given. This tests the mounted
filesystem as seen by the client; it does not prove SMB wire offload, crash
durability, multi-client fencing, or the absence of WAN routes.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import time
import uuid


def mount_info(path: Path) -> dict:
    result = subprocess.run(
        ["findmnt", "-T", str(path), "-J", "-o", "TARGET,SOURCE,FSTYPE,OPTIONS"],
        check=False,
        text=True,
        capture_output=True,
    )
    if result.returncode == 0:
        try:
            filesystems = json.loads(result.stdout).get("filesystems", [])
            if filesystems:
                return filesystems[0]
        except json.JSONDecodeError:
            pass
    return {"target": str(path), "source": "unknown", "fstype": "unknown", "options": "unknown"}


def fail(message: str) -> None:
    raise RuntimeError(message)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mount-point", type=Path, required=True, help="the mounted NAS root, not a local source directory")
    parser.add_argument("--test-dir", type=Path, required=True, help="pre-created disposable directory inside the selected NAS mount")
    parser.add_argument("--write", action="store_true", help="run create/write/rename/copy tests")
    parser.add_argument("--keep", action="store_true", help="keep the unique test objects after a successful write test")
    parser.add_argument("--json", action="store_true", help="emit machine-readable results")
    args = parser.parse_args()

    mount_point = args.mount_point.resolve(strict=True)
    test_dir = args.test_dir.resolve(strict=True)
    if not mount_point.is_dir() or not test_dir.is_dir():
        fail("mount-point and test-dir must be existing directories")
    try:
        test_dir.relative_to(mount_point)
    except ValueError:
        fail("test-dir must be inside mount-point")
    if test_dir == mount_point:
        fail("use a disposable child directory, never the mount root")
    if ".nas-sync" in test_dir.parts:
        fail("do not run the compatibility probe inside .nas-sync")

    info = mount_info(test_dir)
    result = {
        "mount_point": str(mount_point),
        "test_dir": str(test_dir),
        "mount": info,
        "readable": os.access(test_dir, os.R_OK | os.X_OK),
        "write_phase": args.write,
        "automatic_sync_supported": False,
        "limitations": [
            "This is a client-side filesystem probe, not a wire capture.",
            "GVFS/FUSE mounts are inspection/test-only for nas-sync automatic synchronization.",
            "Automatic writes remain disabled until the native CIFS/helper protocol is implemented and validated.",
        ],
    }
    if not result["readable"]:
        fail("test-dir is not readable")
    if not args.write:
        result["status"] = "read-only checks passed"
        print(json.dumps(result, indent=2) if args.json else json.dumps(result, indent=2))
        return 0

    if info.get("fstype") == "fuse.gvfsd-fuse" or str(mount_point).startswith("/run/user/"):
        result["warning"] = "the selected path is a user-space GVFS/FUSE path; results do not qualify it for automatic sync"
    token = f".nas-sync-capability-{os.getpid()}-{uuid.uuid4().hex}"
    source = test_dir / f"{token}.source"
    renamed = test_dir / f"{token}.renamed"
    copied = test_dir / f"{token}.copied"
    lock = test_dir / f"{token}.lock"
    payload = (b"nas-sync capability probe\n" * 4096) + os.urandom(97)
    payload_hash = hashlib.blake2b(payload, digest_size=32).hexdigest()
    created = [source, renamed, copied, lock]
    try:
        source.write_bytes(payload)
        with source.open("rb") as handle:
            os.fsync(handle.fileno())
        os.replace(source, renamed)
        if renamed.read_bytes() != payload:
            fail("atomic rename/readback changed payload")
        fd = os.open(lock, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        os.close(fd)
        try:
            os.open(lock, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        except FileExistsError:
            exclusive = True
        else:
            exclusive = False
        if hasattr(os, "copy_file_range"):
            with renamed.open("rb") as src, copied.open("wb") as dst:
                remaining = len(payload)
                while remaining:
                    moved = os.copy_file_range(src.fileno(), dst.fileno(), remaining)
                    if moved <= 0:
                        fail("copy_file_range made no progress")
                    remaining -= moved
            copy_mode = "copy_file_range"
        else:
            shutil.copyfile(renamed, copied)
            copy_mode = "userspace copyfile fallback"
        copied_hash = hashlib.blake2b(copied.read_bytes(), digest_size=32).hexdigest()
        result.update({
            "status": "write/readback checks passed",
            "payload_bytes": len(payload),
            "payload_blake2b": payload_hash,
            "copy_mode": copy_mode,
            "copy_blake2b": copied_hash,
            "exclusive_create": exclusive,
            "same_filesystem": os.stat(renamed).st_dev == os.stat(copied).st_dev,
        })
        if not exclusive or copied_hash != payload_hash:
            fail("exclusive-create or copied payload check failed")
        print(json.dumps(result, indent=2))
        return 0
    finally:
        if not args.keep:
            for path in created:
                try:
                    path.unlink()
                except FileNotFoundError:
                    pass


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, RuntimeError) as error:
        print(f"real NAS probe failed: {error}", file=sys.stderr)
        raise SystemExit(2)
