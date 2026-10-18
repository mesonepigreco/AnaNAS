from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

import install_ananas


class InstallTests(unittest.TestCase):
    def test_closes_only_control_panels_of_this_installation(self):
        """A panel from the previous installation would swallow the next click."""
        with tempfile.TemporaryDirectory() as temporary:
            indicator = Path(temporary) / "nas-sync-indicator"
            sleep = [sys.executable, "-c", "import time; time.sleep(60)"]
            panel = subprocess.Popen(sleep + [str(indicator), "--control-panel"])
            tray = subprocess.Popen(sleep + [str(indicator), "--config", "config.json"])
            other = subprocess.Popen(sleep + [str(Path(temporary) / "other"), "--control-panel"])
            try:
                self.assertEqual(install_ananas.close_control_panels(indicator), 1)
                self.assertNotEqual(panel.wait(timeout=10), 0)
                self.assertIsNone(tray.poll())
                self.assertIsNone(other.poll())
            finally:
                for process in (panel, tray, other):
                    process.kill()
                    process.wait()


if __name__ == "__main__":
    unittest.main()
