#!/usr/bin/env python3
"""Summarize a bounded copy probe's classic pcap without displaying payload.

Reads Ethernet/IPv4/TCP headers only. Counts captured original frame lengths
and TCP payload lengths (including protocol/encryption overhead and retries)
during the probe's copy window. This is host capture evidence, not a physical
Ethernet-byte guarantee: segmentation/receive offload may aggregate packets.
"""

import argparse
import ipaddress
import json
from pathlib import Path
import struct


def summarize(pcap_path, probe, client_ip, nas_ip):
    window = probe["copy_window"]
    start, end = window["start_unix_ns"], window["end_unix_ns"]
    if not isinstance(start, int) or not isinstance(end, int) or start >= end:
        raise ValueError("invalid probe copy window")
    client = ipaddress.IPv4Address(client_ip).packed
    nas = ipaddress.IPv4Address(nas_ip).packed
    counts = {direction: {"packets": 0, "frame_bytes": 0, "tcp_payload_bytes": 0}
              for direction in ("client_to_nas", "nas_to_client")}
    first, last = None, None
    with Path(pcap_path).open("rb") as stream:
        header = stream.read(24)
        if len(header) != 24:
            raise ValueError("truncated capture header")
        formats = {b"\xd4\xc3\xb2\xa1": ("<", 1000), b"\xa1\xb2\xc3\xd4": (">", 1000),
                   b"\x4d\x3c\xb2\xa1": ("<", 1), b"\xa1\xb2\x3c\x4d": (">", 1)}
        if header[:4] not in formats:
            raise ValueError("only classic pcap is supported")
        endian, multiplier = formats[header[:4]]
        major, minor, _, _, snaplen, linktype = struct.unpack(endian + "HHIIII", header[4:])
        if (major, minor) != (2, 4) or linktype != 1 or not 54 <= snaplen <= 262144:
            raise ValueError("unsupported capture version, link type or snapshot size")
        while True:
            record = stream.read(16)
            if not record:
                break
            if len(record) != 16:
                raise ValueError("truncated packet header")
            seconds, fraction, captured, original = struct.unpack(endian + "IIII", record)
            if captured > snaplen or captured > original or fraction * multiplier >= 1_000_000_000:
                raise ValueError("invalid packet lengths or timestamp")
            packet = stream.read(captured)
            if len(packet) != captured:
                raise ValueError("truncated captured packet")
            timestamp = seconds * 1_000_000_000 + fraction * multiplier
            first = timestamp if first is None else min(first, timestamp)
            last = timestamp if last is None else max(last, timestamp)
            if not start <= timestamp <= end:
                continue
            if len(packet) < 14:
                raise ValueError("truncated Ethernet header in copy window")
            offset = 14
            ethertype = int.from_bytes(packet[12:14], "big")
            for _ in range(2):
                if ethertype not in (0x8100, 0x88A8):
                    break
                if len(packet) < offset + 4:
                    raise ValueError("truncated VLAN header")
                ethertype = int.from_bytes(packet[offset + 2:offset + 4], "big")
                offset += 4
            if ethertype != 0x0800:
                continue
            if len(packet) < offset + 20:
                raise ValueError("truncated IPv4 header")
            ip = packet[offset:]
            ihl = (ip[0] & 15) * 4
            if ip[0] >> 4 != 4 or ihl < 20 or len(ip) < ihl:
                raise ValueError("invalid IPv4 header")
            if ip[9] != 6:
                continue
            pair = (ip[12:16], ip[16:20])
            if pair == (client, nas):
                direction = "client_to_nas"
            elif pair == (nas, client):
                direction = "nas_to_client"
            else:
                continue
            if int.from_bytes(ip[6:8], "big") & 0x3FFF:
                raise ValueError("fragmented IPv4 prevents complete TCP accounting")
            tcp = ip[ihl:]
            if len(tcp) < 20:
                raise ValueError("truncated TCP header")
            if 445 not in (int.from_bytes(tcp[:2], "big"), int.from_bytes(tcp[2:4], "big")):
                continue
            thl = (tcp[12] >> 4) * 4
            total = int.from_bytes(ip[2:4], "big")
            if thl < 20 or len(tcp) < thl or total < ihl + thl or original < offset + total:
                raise ValueError("invalid TCP/IP lengths")
            counts[direction]["packets"] += 1
            counts[direction]["frame_bytes"] += original
            counts[direction]["tcp_payload_bytes"] += total - ihl - thl
    if first is None or first > start or last < end:
        raise ValueError("capture does not bracket the complete probe copy window")
    total_payload = sum(c["tcp_payload_bytes"] for c in counts.values())
    if not all(c["packets"] > 0 for c in counts.values()):
        raise ValueError("copy window lacks traffic in both directions")
    return {"copy_window": window, "directions": counts, "total_tcp_payload_bytes": total_payload,
            "copied_file_bytes": probe["payload_bytes"], "copy_mode": probe["copy_mode"],
            "less_than_one_file_payload": total_payload < probe["payload_bytes"],
            "limitations": "Counts one host-captured copy interval across SMB connections to this NAS, including overhead/retries; not server crash durability, fencing, WAN safety, or arbitrary-range diff validation."}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pcap", required=True)
    parser.add_argument("--probe-json", required=True)
    parser.add_argument("--client-ip", required=True)
    parser.add_argument("--nas-ip", required=True)
    args = parser.parse_args()
    probe_path = Path(args.probe_json)
    if probe_path.stat().st_size > 1 << 20:
        parser.error("probe JSON exceeds 1 MiB")
    result = summarize(args.pcap, json.loads(probe_path.read_text()), args.client_ip, args.nas_ip)
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
