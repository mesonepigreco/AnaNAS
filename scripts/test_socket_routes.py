#!/usr/bin/python3
"""Explicit isolated TCP route test. Never run network mutations in the host namespace.

Launch through: pkexec unshare --net python3 THIS --host-netns ORIGINAL --binary PROBE
All interfaces, bridge settings and routes are created in three anonymous network
namespaces. No named namespace, mount, host route, NAS endpoint or user file is used.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import select
import signal
import socket
import struct
import subprocess
import sys
import time

IP = "/usr/sbin/ip"
NSENTER = "/usr/bin/nsenter"
UNSHARE = "/usr/bin/unshare"
ROUTER_MAC = "02aa00000001"
CLIENT, NAS = "10.88.0.2", "10.88.0.30"


def guard(host, parent=None):
    current = os.readlink("/proc/self/ns/net")
    init = os.readlink("/proc/1/ns/net")
    if os.geteuid() != 0 or not re.fullmatch(r"net:\[[0-9]+\]", host) or current in (host, init, parent):
        raise RuntimeError("Refusing network test outside a separate privileged namespace")
    return current


def run(command, **kwargs):
    return subprocess.check_output(command, stderr=subprocess.PIPE, timeout=8, **kwargs).decode()


def event(process, timeout=8):
    deadline = time.monotonic()+timeout
    data = getattr(process, "ananas_buffer", b"")
    while time.monotonic() < deadline:
        if b"\n" in data:
            line, process.ananas_buffer = data.split(b"\n", 1)
            return json.loads(line)
        if not select.select([process.stdout], [], [], max(0, deadline-time.monotonic()))[0]:
            break
        chunk = os.read(process.stdout.fileno(), 4096)
        if not chunk:
            raise RuntimeError("Fixture process ended before its event")
        data += chunk
        if len(data) > 2*1024*1024:
            raise RuntimeError("Fixture event exceeded bound")
    raise TimeoutError("Fixture event timeout")


def decode_packet(data, address):
    if address[0] not in ("frompc", "tonas") or address[2] == socket.PACKET_OUTGOING or len(data) < 54 or data[12:14] != b"\x08\x00":
        return None
    ihl = (data[14] & 15)*4
    if data[14] >> 4 != 4 or ihl < 20 or data[23] != 6 or len(data) < 14+ihl+20 or struct.unpack_from("!H", data, 20)[0] & 0x1fff:
        return None
    source, target = socket.inet_ntoa(data[26:30]), socket.inet_ntoa(data[30:34])
    if (source, target) not in ((CLIENT, NAS), (NAS, CLIENT)):
        return None
    sport, dport = struct.unpack_from("!HH", data, 14+ihl)
    if not ({sport, dport} & {8742, 8743}):
        return None
    return {"interface": address[0], "source": source, "target": target, "sourcePort": sport, "targetPort": dport, "destinationMAC": data[:6].hex(), "tcpFlags": data[14+ihl+13], "ipBytes": struct.unpack_from("!H", data, 16)[0], "snapshotHex": data.hex()}


def holder(args):
    current = guard(args.host_netns, args.parent_netns)
    signal.alarm(90)
    raw = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(3)) if args.capture else None
    if raw:
        raw.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 1024*1024)
    records = []
    received, dropped = 0, 0
    started = time.monotonic()
    print(json.dumps({"event": "namespace", "namespace": current}), flush=True)
    while True:
        ready = select.select(([raw] if raw else [])+[sys.stdin.buffer], [], [], 90)[0]
        if raw and raw in ready:
            data, address = raw.recvfrom(160)
            packet = decode_packet(data, address)
            if packet:
                if len(records) >= 2048:
                    raise RuntimeError("Isolated capture exceeded 2048-packet bound")
                packet["elapsedSeconds"] = time.monotonic()-started
                records.append(packet)
        if sys.stdin.buffer in ready:
            command = sys.stdin.buffer.readline(16)
            if command == b"snapshot\n":
                stats = raw.getsockopt(263, 6, 12) # SOL_PACKET / PACKET_STATISTICS
                count, loss = struct.unpack_from("II", stats)
                received, dropped = received+count, dropped+loss
                print(json.dumps({"event": "capture", "packets": records, "receivedPackets": received, "droppedPackets": dropped}), flush=True)
            else:
                return


def test(args):
    started = time.monotonic()
    current = guard(args.host_netns)
    links = json.loads(run([IP, "-j", "link", "show"]))
    if [link["ifname"] for link in links] != ["lo"]:
        raise RuntimeError("Test requires a fresh namespace containing only loopback")
    binary = args.binary.resolve(strict=True)
    if not binary.is_file() or not os.access(binary, os.X_OK) or binary.stat().st_size > 16*1024*1024:
        raise RuntimeError("Built fixture executable required")
    children, captures, results = [], [], []
    script = str(Path(__file__).resolve())

    def spawn(command):
        p = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, bufsize=0)
        children.append(p)
        return p

    def ns(pid, command):
        return [NSENTER, "--net=/proc/"+str(pid)+"/ns/net", "--"]+command

    report = {"scope": "Isolated PC-kernel socket routing fixture, not production or QNAP egress acceptance", "kernel": os.uname().release, "hostNamespace": args.host_netns, "testNamespace": current, "binarySHA256": hashlib.sha256(binary.read_bytes()).hexdigest(), "runnerSHA256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(), "passed": False, "cases": results, "limitations": ["Virtual Ethernet/bridge fixture, not physical LAN or QNAP vendor kernel", "TCP echo with the actual socketpolicy package, not TLS/autosync", "Priority-rule case is not a live VPN/NAT test", "No IPv6, interface/address removal or long retransmission window", "Capture limited to test TCP flows, 160-byte snapshots and 2048 packets"]}
    try:
        common = [UNSHARE, "--net", "/usr/bin/python3", script, "--hold-namespace", "--host-netns", args.host_netns, "--parent-netns", current]
        router = spawn(common+["--capture"])
        serverNS = spawn(common)
        for p in (router, serverNS):
            e = event(p)
            if e.get("event") != "namespace" or e["namespace"] != os.readlink("/proc/"+str(p.pid)+"/ns/net") or e["namespace"] in (current, args.host_netns):
                raise RuntimeError("Child network namespace identity differs")
        report["namespaceHolders"] = [{"pid": p.pid, "namespace": os.readlink("/proc/"+str(p.pid)+"/ns/net")} for p in (router, serverNS)]
        R = lambda *cmd: run(ns(router.pid, [IP, *cmd]))
        S = lambda *cmd: run(ns(serverNS.pid, [IP, *cmd]))
        A = lambda *cmd: run([IP, *cmd])
        A("link", "add", "lan0", "type", "veth", "peer", "name", "frompc")
        A("link", "set", "frompc", "netns", str(router.pid))
        A("link", "add", "tonas", "type", "veth", "peer", "name", "nas0")
        A("link", "set", "tonas", "netns", str(router.pid))
        A("link", "set", "nas0", "netns", str(serverNS.pid))
        R("link", "add", "br0", "type", "bridge")
        R("link", "set", "br0", "address", "02:aa:00:00:00:01")
        for dev in ("frompc", "tonas"):
            R("link", "set", dev, "master", "br0")
            R("link", "set", dev, "up")
        R("addr", "add", "10.88.0.1/24", "dev", "br0")
        R("link", "set", "br0", "up")
        A("addr", "add", CLIENT+"/24", "dev", "lan0")
        A("link", "set", "lan0", "up")
        S("addr", "add", NAS+"/24", "dev", "nas0")
        S("link", "set", "nas0", "up")
        for pid, dev in ((os.getpid(), "lan0"), (router.pid, "br0"), (serverNS.pid, "nas0")):
            run(ns(pid, [IP, "link", "set", "lo", "up"]))
            run(ns(pid, ["/usr/sbin/sysctl", "-qw", "net.ipv4.conf.all.accept_redirects=0", "net.ipv4.conf."+dev+".accept_redirects=0", "net.ipv4.conf.all.send_redirects=0", "net.ipv4.conf."+dev+".send_redirects=0"]))
        run(ns(router.pid, ["/usr/sbin/sysctl", "-qw", "net.ipv4.ip_forward=1"]))
        # ip_forward changes reset some host/router defaults; enforce again.
        run(ns(router.pid, ["/usr/sbin/sysctl", "-qw", "net.ipv4.conf.all.send_redirects=0", "net.ipv4.conf.br0.send_redirects=0"]))
        for port, plain in ((8742, True), (8743, False)):
            p = spawn(ns(serverNS.pid, [str(binary), "-host-netns", args.host_netns, "-serve", "-port", str(port)]+(["-plain-control"] if plain else [])))
            if event(p).get("event") != "serving":
                raise RuntimeError("Echo fixture failed to listen")

        def capture():
            router.stdin.write(b"snapshot\n")
            e = event(router)
            if e.get("event") != "capture":
                raise RuntimeError("Capture helper failed")
            report["captureCounters"] = {"receivedPackets": e["receivedPackets"], "droppedPackets": e["droppedPackets"]}
            if e["droppedPackets"] != 0:
                raise RuntimeError("Packet loss invalidates capture evidence")
            return e["packets"]

        def scenario(name, source_port, plain, direction="client", change=None, server_port=8742):
            command = [str(binary), "-host-netns", args.host_netns, "-source-port", str(source_port), "-port", str(server_port)]
            if plain:
                command.append("-plain-control")
            if change:
                command.append("-hold")
            p = spawn(command)
            first = event(p)
            if change:
                if first.get("event") != "exchange":
                    raise RuntimeError("Direct established-connection precondition failed")
                change()
                p.stdin.write(b"again\n")
            tail, errors = p.communicate(timeout=7)
            events = [first]+[json.loads(line) for line in (getattr(p, "ananas_buffer", b"")+tail).splitlines()]
            time.sleep(1.25) # explicit bounded post-close/retransmission capture
            all_packets = capture()
            flow = [packet for packet in all_packets if source_port in (packet["sourcePort"], packet["targetPort"])]
            sender = CLIENT if direction == "client" else NAS
            routed = [packet for packet in flow if packet["source"] == sender and packet["destinationMAC"] == ROUTER_MAC]
            constrained = not plain if direction == "client" else server_port == 8743
            passed = not routed if constrained else p.returncode == 0 and bool(routed)
            result = {"name": name, "sourcePort": source_port, "direction": direction, "constrained": constrained, "exitCode": p.returncode, "gatewayPackets": len(routed), "capturedFlowPackets": len(flow), "events": events, "passed": passed}
            results.append(result)
            print(json.dumps({"phase": name, **result}), file=sys.stderr, flush=True)
            if not passed:
                raise RuntimeError("Socket routing acceptance failed: "+name)

        gateway = lambda: A("route", "replace", NAS+"/32", "via", "10.88.0.1", "dev", "lan0")
        direct = lambda: A("route", "del", NAS+"/32")
        gateway()
        scenario("gateway-control", 30001, True)
        scenario("gateway-constrained", 30002, False)
        direct()
        for number, plain in ((3, True), (4, False)):
            scenario("established-"+("control" if plain else "constrained"), 30000+number, plain, change=gateway)
            direct()
        A("route", "del", "10.88.0.0/24", "dev", "lan0")
        A("route", "add", "10.88.0.1/32", "dev", "lan0", "src", CLIENT)
        A("route", "add", "default", "via", "10.88.0.1", "dev", "lan0")
        scenario("default-control", 30005, True)
        scenario("default-constrained", 30006, False)
        A("route", "del", "default")
        A("route", "del", "10.88.0.1/32")
        A("route", "add", "10.88.0.0/24", "dev", "lan0", "src", CLIENT)
        A("route", "add", "table", "120", "10.88.0.1/32", "dev", "lan0", "src", CLIENT)
        A("route", "add", "table", "120", "default", "via", "10.88.0.1", "dev", "lan0")
        A("rule", "add", "priority", "100", "lookup", "120")
        scenario("priority-rule-control", 30011, True)
        scenario("priority-rule-constrained", 30012, False)
        A("rule", "del", "priority", "100", "lookup", "120")
        S("route", "replace", CLIENT+"/32", "via", "10.88.0.1", "dev", "nas0")
        scenario("reply-control", 30007, True, direction="nas")
        scenario("reply-constrained", 30008, True, direction="nas", server_port=8743)
        S("route", "del", CLIENT+"/32")
        for number, port in ((9, 8742), (10, 8743)):
            scenario("established-reply-"+("control" if port == 8742 else "constrained"), 30000+number, True, direction="nas", server_port=port, change=lambda: S("route", "replace", CLIENT+"/32", "via", "10.88.0.1", "dev", "nas0"))
            S("route", "del", CLIENT+"/32")
        captures = capture()
        report["passed"] = True
    except Exception as error:
        report["error"] = str(error)
        if isinstance(error, subprocess.CalledProcessError):
            report["commandError"] = error.stderr.decode(errors="replace")[:4096]
        try:
            captures = capture()
        except Exception:
            pass
    finally:
        # Only child handles created here are terminated. Anonymous namespaces
        # disappear as their final processes exit; no host cleanup is necessary.
        for p in reversed(children):
            if p.poll() is None:
                p.terminate()
        for p in reversed(children):
            try:
                p.communicate(timeout=4)
            except subprocess.TimeoutExpired:
                p.kill()
                p.communicate(timeout=4)
        report["childrenStopped"] = all(p.poll() is not None for p in children)
        report["childProcesses"] = [{"pid": p.pid, "exitCode": p.returncode} for p in children]
        report["seconds"] = time.monotonic()-started
        report["packets"] = captures
        print(json.dumps(report), flush=True)
    return 0 if report["passed"] and report["childrenStopped"] else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host-netns", required=True)
    parser.add_argument("--parent-netns")
    parser.add_argument("--binary", type=Path)
    parser.add_argument("--hold-namespace", action="store_true")
    parser.add_argument("--capture", action="store_true")
    args = parser.parse_args()
    if args.hold_namespace:
        holder(args)
        return 0
    if args.binary is None or args.capture or args.parent_netns:
        parser.error("explicit binary and original namespace required")
    def expired(_signal, _frame):
        raise TimeoutError("Isolated test exceeded 90-second lifetime")
    signal.signal(signal.SIGALRM, expired)
    signal.alarm(90)
    return test(args)


if __name__ == "__main__":
    sys.exit(main())
