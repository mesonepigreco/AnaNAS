import json
from pathlib import Path
import socket
import struct
import subprocess
import sys
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import test_socket_routes as routes


class SocketRouteHarnessTests(unittest.TestCase):
    def test_host_or_unprivileged_namespace_refused_before_commands(self):
        for uid, current, host, init in ((1000, "net:[2]", "net:[1]", "net:[1]"), (0, "net:[1]", "net:[1]", "net:[1]"), (0, "net:[1]", "net:[99]", "net:[1]")):
            with patch.object(routes.os, "geteuid", return_value=uid), patch.object(routes.os, "readlink", side_effect=[current, init]), patch.object(routes, "run") as run:
                with self.assertRaises(RuntimeError):
                    routes.test(SimpleNamespace(host_netns=host, binary=Path("/tmp/probe")))
                run.assert_not_called()

    def test_capture_only_matches_fixed_fixture_ingress(self):
        data = bytearray(118)
        data[:6] = bytes.fromhex(routes.ROUTER_MAC)
        data[12:14] = b"\x08\x00"
        data[14], data[23] = 0x45, 6
        struct.pack_into("!H", data, 16, 104)
        data[26:30] = socket.inet_aton(routes.CLIENT)
        data[30:34] = socket.inet_aton(routes.NAS)
        struct.pack_into("!HH", data, 34, 30001, 8742)
        data[47] = 2
        address = ("frompc", 0, socket.PACKET_HOST)
        packet = routes.decode_packet(data, address)
        self.assertEqual(packet["destinationMAC"], routes.ROUTER_MAC)
        self.assertEqual(packet["ipBytes"], 104)
        self.assertIsNone(routes.decode_packet(data, ("tonas", 0, socket.PACKET_OUTGOING)))
        self.assertIsNone(routes.decode_packet(data, ("eth0", 0, socket.PACKET_HOST)))
        struct.pack_into("!HH", data, 34, 30001, 443)
        self.assertIsNone(routes.decode_packet(data, address))
        struct.pack_into("!HH", data, 34, 30001, 8742)
        data[30:34] = socket.inet_aton("10.23.42.30")
        self.assertIsNone(routes.decode_packet(data, address))

    def test_event_reader_retains_prefetched_lines(self):
        p = subprocess.Popen([sys.executable, "-c", 'print("{\\"event\\":1}\\n{\\"event\\":2}", flush=True)'], stdout=subprocess.PIPE, bufsize=0)
        try:
            self.assertEqual(routes.event(p), {"event": 1})
            self.assertEqual(routes.event(p), {"event": 2})
        finally:
            p.communicate(timeout=2)


if __name__ == "__main__":
    unittest.main()
