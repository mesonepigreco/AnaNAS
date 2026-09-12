import subprocess
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import run_helper_test as runner


class HelperScopeTests(unittest.TestCase):
    def test_ownership_and_cleanup_paths_only_cover_new_child(self):
        parent = "/share/EXAMPLE_VOLUME/nas-sync-test/nas-sync-capability-test"
        work = parent+"/ananas-helper-test-"+"a"*32
        paths = runner.scope_paths(parent, work)
        self.assertNotIn(parent, paths)
        self.assertTrue(all(p == work or Path(p).parent == Path(work) for p in paths))
        for wrong in (parent, work+"/../other", parent+"/existing", "/share/Nasdir/"+Path(work).name):
            with self.assertRaises(ValueError):
                runner.scope_paths(parent, wrong)

    def test_local_ssh_exit_is_not_remote_completion_proof(self):
        event = {"helperProcess": {"event": "stopped", "uid": 1000, "gid": 100}}
        for status in (None, 1, 255):
            self.assertFalse(runner.confirmed_stopped(Mock(poll=lambda: status), event))
        self.assertFalse(runner.confirmed_stopped(Mock(poll=lambda: 0), {}))
        self.assertTrue(runner.confirmed_stopped(Mock(poll=lambda: 0), event))

    def test_read_only_preflight_does_not_generate_tls_or_write(self):
        args = SimpleNamespace(session_dir=Path("/tmp/session"), helper=Path("/tmp/helper"), launcher=Path("/tmp/launcher"), client=Path("/tmp/client"), write=False)
        with patch.object(runner, "session_options", return_value=["ssh"]), patch.object(runner, "binary_payload", return_value=b"binary"), patch.object(runner, "remote", side_effect=["ANANAS_TEST_DIR=/share/EXAMPLE_VOLUME/nas-sync-test/nas-sync-capability-test\n", "1000\n100\n"]) as remote, patch.object(runner, "certs") as certs, patch.object(runner.subprocess, "Popen") as popen:
            runner.run(args)
            self.assertEqual(remote.call_count, 2)
            certs.assert_not_called()
            popen.assert_not_called()


if __name__ == "__main__":
    unittest.main()
