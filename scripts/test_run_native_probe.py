import tempfile
from pathlib import Path
import unittest
import contextlib
import hashlib
import io
import subprocess
import shlex
from types import SimpleNamespace
from unittest.mock import patch
from run_native_probe import binary_payload, canonical_test_path, ownership_paths, run


class ScopeTests(unittest.TestCase):
    def test_canonical_disposable_path_only(self):
        path = "/share/EXAMPLE_VOLUME/nas-sync-test/nas-sync-capability-test"
        self.assertEqual(canonical_test_path("ANANAS_TEST_DIR=" + path + "\nLinux 4.2.8 aarch64\n"), path)
        for bad in ("/share/home/nas-sync-capability-test", "/share/nas-sync-test", "../nas-sync-capability-test", "/share/../nas-sync-test/nas-sync-capability-test", path + "/.."):
            with self.assertRaises(ValueError):
                canonical_test_path("ANANAS_TEST_DIR=" + bad)
        with self.assertRaises(ValueError):
            canonical_test_path("ANANAS_TEST_DIR=" + path + "\nANANAS_TEST_DIR=" + path)

    def test_binary_type_and_symlink_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "file"
            path.write_bytes(b"not an ELF executable" * 4)
            with self.assertRaises(ValueError):
                binary_payload(path)
            link = Path(directory) / "link"
            link.symlink_to(path)
            with self.assertRaises(OSError):
                binary_payload(link)

    def test_no_upload_without_write_flag(self):
        path = "/share/EXAMPLE_VOLUME/nas-sync-test/nas-sync-capability-test"
        args = SimpleNamespace(session_dir=Path("/unused"), binary=Path("/unused"), write=False, as_sync_user=False)
        with patch("run_native_probe.session_options", return_value=["ssh"]), patch("run_native_probe.binary_payload", return_value=b"fixture"), patch("run_native_probe.remote", return_value="ANANAS_TEST_DIR=" + path) as remote, contextlib.redirect_stdout(io.StringIO()):
            run(args)
        self.assertEqual(remote.call_count, 1)
        self.assertNotIn("mktemp", remote.call_args.args[1])
        self.assertNotIn("cat >", remote.call_args.args[1])

    def test_timeout_does_not_clean_up_possibly_live_probe(self):
        path = "/share/EXAMPLE_VOLUME/nas-sync-test/nas-sync-capability-test"
        args = SimpleNamespace(session_dir=Path("/unused"), binary=Path("/unused"), write=True, as_sync_user=False)
        responses = ["ANANAS_TEST_DIR=" + path, path + "/ananas-probe-tool.ABC123", "", hashlib.sha256(b"fixture").hexdigest() + "  probe", subprocess.TimeoutExpired("probe", 75)]
        with patch("run_native_probe.session_options", return_value=["ssh"]), patch("run_native_probe.binary_payload", return_value=b"fixture"), patch("run_native_probe.remote", side_effect=responses) as remote, contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaises(subprocess.TimeoutExpired):
                run(args)
        self.assertEqual(remote.call_count, 5)
        self.assertFalse(any("rm -f" in call.args[1] for call in remote.call_args_list))

    def test_non_admin_probe_is_foreground_and_identity_checked(self):
        path = "/share/EXAMPLE_VOLUME/nas-sync-test/nas-sync-capability-test"
        args = SimpleNamespace(session_dir=Path("/unused"), binary=Path("/unused"), write=True, as_sync_user=True)
        report = '{"uid":1000,"gid":100,"groups":[100]}'
        responses = ["ANANAS_TEST_DIR=" + path, "1000\n100\n", path + "/ananas-probe-tool.ABC123", "", hashlib.sha256(b"fixture").hexdigest() + "  probe", "", report, report, ""]
        with patch("run_native_probe.session_options", return_value=["ssh"]), patch("run_native_probe.binary_payload", return_value=b"fixture"), patch("run_native_probe.remote", side_effect=responses) as remote, contextlib.redirect_stdout(io.StringIO()):
            run(args)
        commands = [call.args[1] for call in remote.call_args_list]
        runs = [command for command in commands if "start-stop-daemon" in command]
        self.assertEqual(len(runs), 2)
        for command in runs:
            self.assertIn("-c nas-sync-test:everyone", command)
            self.assertIn("-expect-uid 1000 -expect-gid 100", command)
            self.assertNotIn(" -b ", command)
            self.assertNotIn(" -K ", command)
        ownership = [shlex.split(command) for command in commands if command.startswith("chown ")]
        self.assertEqual(ownership, [["chown", "1000:100", path + "/ananas-probe-tool.ABC123", path + "/ananas-probe-tool.ABC123/probe"]])

    def test_existing_parent_cannot_be_chowned(self):
        path = "/share/EXAMPLE_VOLUME/nas-sync-test/nas-sync-capability-test"
        for wrong in (path, str(Path(path).parent), path + "/ananas-probe-tool.ABC123/..", path + "/other.ABC123", path + "/ananas-probe-tool.ABC123/nested"):
            with self.assertRaises(ValueError):
                ownership_paths(path, wrong)


if __name__ == "__main__":
    unittest.main()
