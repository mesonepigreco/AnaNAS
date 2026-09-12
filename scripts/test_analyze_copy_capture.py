from pathlib import Path
import struct
import tempfile
import unittest

from analyze_copy_capture import summarize


def packet(source, destination, payload):
    ethernet = bytes(12) + b"\x08\x00"
    ip = bytearray(20)
    ip[0], ip[9] = 0x45, 6
    ip[2:4] = (40 + payload).to_bytes(2, "big")
    ip[12:16], ip[16:20] = bytes(source), bytes(destination)
    tcp = bytearray(20)
    tcp[:2], tcp[2:4], tcp[12] = (12345).to_bytes(2, "big"), (445).to_bytes(2, "big"), 0x50
    return ethernet + ip + tcp + bytes(payload)


class CaptureTests(unittest.TestCase):
    def test_counts_original_lengths_with_truncated_payload(self):
        client, nas = (10, 23, 42, 17), (10, 23, 42, 30)
        pcap = struct.pack("<IHHIIII", 0xA1B2C3D4, 2, 4, 0, 0, 160, 1)
        for seconds, source, target, payload in ((1, client, nas, 300), (2, client, nas, 200),
                                                  (2, nas, client, 400), (3, nas, client, 100)):
            frame = packet(source, target, payload)
            pcap += struct.pack("<IIII", seconds, 0, min(len(frame), 160), len(frame)) + frame[:160]
        probe = {"copy_window": {"start_unix_ns": 1_500_000_000, "end_unix_ns": 2_500_000_000},
                 "payload_bytes": 106593, "copy_mode": "copy_file_range"}
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "test.pcap"
            path.write_bytes(pcap)
            result = summarize(path, probe, "10.23.42.17", "10.23.42.30")
            self.assertEqual(result["total_tcp_payload_bytes"], 600)
            self.assertEqual(result["directions"]["client_to_nas"]["frame_bytes"], 254)
            self.assertEqual(result["directions"]["nas_to_client"]["frame_bytes"], 454)
            path.write_bytes(pcap[:-1])
            with self.assertRaises(ValueError):
                summarize(path, probe, "10.23.42.17", "10.23.42.30")
            path.write_bytes(pcap)
            probe["copy_window"]["end_unix_ns"] = 4_000_000_000
            with self.assertRaises(ValueError):
                summarize(path, probe, "10.23.42.17", "10.23.42.30")


if __name__ == "__main__":
    unittest.main()
