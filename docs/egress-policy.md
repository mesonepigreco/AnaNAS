# LAN egress validation in progress

Automatic production writes remain disabled. Direct interface/route checks,
private literal endpoints, TLS authentication and socket flags are implemented;
the full route-change and packet-capture acceptance gate is still open. The
isolated PC-kernel routing cases below now pass with captured packets.

## Socket restrictions

`internal/socketpolicy` now sets `SO_BINDTODEVICE` and `SO_DONTROUTE` before the
PC TCP connect or NAS bind/listen. It verifies IPv4, TCP, the device name and the
retained option values. The NAS helper validates the inherited listener and each
accepted connection before TLS/application I/O, refusing missing or changed flags.
There is no IPv6, hostname, proxy, redirect or unbound socket fallback.

The Linux socket interface documents `SO_DONTROUTE` as requesting direct delivery
without gateway routing. [Primary reference: socket(7)](https://man7.org/linux/man-pages/man7/socket.7.html).
That documented behavior motivates the restriction; successful option readback
and a normal direct connection do not prove behavior during route changes on a
vendor kernel. The option is per socket, not an immutable process-wide firewall.
It does not apply to kernel CIFS mounts. Existing application gates still check
the source address, physical carrier, exact prefix and direct return route.

Local tests reject unbound/gateway-enabled sockets, wrong devices, IPv6, UDP and
Unix sockets. Real loopback helper tests exercise inherited and accepted socket
flags alongside mutual TLS, diff upload/download and notification delivery.
The QNAP's Linux 4.2.8/aarch64 also passed that direct-LAN fixture; its exact binary
hashes and results are in (raw evidence retained privately; see the publication audit).
Those direct-connection tests do not mutate routes. The subsequent isolated
PC-kernel test below adds route-change evidence; live VPN/NAT, alternate-interface
and QNAP vendor-kernel acceptance remain outstanding.

## Host and NAS capability inspection, 2026-09-09

Read-only commands through the authenticated admin setup socket were
`command -v ip iptables ip6tables nft` (as separate shell lookups),
`/bin/busybox ip rule show`, and reads of `/proc/net/ip_tables_names`,
`/proc/net/ip_tables_targets` and `/proc/net/ip_tables_matches`.
QNAP has `/usr/bin/ip`, `/sbin/iptables` and `/sbin/ip6tables`; `nft` was not found
on that session's PATH. Loaded tables include filter, mangle and nat, with MARK
and CONNMARK targets. Existing policy rules use marks `0x100`/`0x10000` and tables
34/42 in addition to main/local/default. These are live NAS networking rules;
none were replaced, flushed or otherwise modified.

The PC reports kernel `7.0.0-30-generic`, systemd `259.5-0ubuntu3.4`, and installed
`nft`, `ip` and `iptables`. These tool/version checks do not prove cgroup or firewall
enforcement for the existing user service. Filtering all UID 1000 traffic would
affect unrelated desktop applications and is not an appropriate deployment rule.

An empty isolated test namespace could not be started without additional host
authentication: `unshare --user --map-root-user --net true` failed writing
`/proc/self/uid_map` with `Operation not permitted`, and
`sudo -n /usr/bin/unshare --net /usr/bin/true` required interactive authentication.
Neither command changed host/NAS routes or installed network rules. This is an
isolation-test prerequisite, not evidence of packet blocking by anaNAS. A later
`pkexec` invocation successfully ran the prepared harness in anonymous namespaces.

## Isolated PC-kernel routing capture, 2026-09-09

`cmd/ananas-route-probe` exercises the actual `internal/socketpolicy` implementation
with fixed 64-byte TCP echoes. The ordinary-socket control retains device binding
but omits `SO_DONTROUTE`; constrained client and accepted server sockets apply and
verify the production package's flags. The executable refuses the host/initial
network namespace and non-root execution. It cannot select a production endpoint:
its addresses are fixed at `10.88.0.2`/`10.88.0.30`, ports 8742/8743, with source
ports 30001–30100 and test interfaces `lan0`/`nas0`.

`scripts/test_socket_routes.py` requires a fresh outer network namespace containing
only loopback. It creates two additional anonymous namespaces and virtual Ethernet
pairs connecting the test client, a bridge/router and a test server. The router's
MAC is `02:aa:00:00:00:01`. Direct frames cross the bridge addressed to the peer;
routed frames are addressed to the router MAC. IP forwarding is enabled only in
that isolated router, and redirects are disabled. No named namespace, host mount,
host route, NAS access or production file is involved.

The final run used these commands (the namespace identity came from a read-only
host `readlink /proc/self/ns/net`; replace it on another host/run):

```sh
GOCACHE=/tmp/nas-sync-go-cache GOMODCACHE=/tmp/nas-sync-go-mod-cache \
  go build -o /tmp/ananas-route-probe ./cmd/ananas-route-probe
pkexec /usr/bin/unshare --net /usr/bin/python3 \
  /home/example-user/Documents/Programming/nas-sync/scripts/test_socket_routes.py \
  --host-netns 'net:[4026531833]' --binary /tmp/ananas-route-probe \
  > /tmp/ananas-socket-routes-final-2026-09-09.json
```

All twelve cases passed on `7.0.0-30-generic` in 15.254244189 seconds. Every echo
completed, including constrained cases: the kernel used direct delivery despite
the gateway-selected route, rather than dropping the connection. Application
route checks still reject such route evidence; this tests the kernel restriction
that remains in effect between application checks.

| Case pair | Ordinary socket: gateway TCP packets | Constrained socket: gateway TCP packets |
|---|---:|---:|
| More-specific gateway route before connect | 6 | 0 |
| Gateway route added after first exchange | 2 | 0 |
| Connected route removed; gateway default remains | 6 | 0 |
| Higher-priority rule selecting a gateway table | 6 | 0 |
| Server reply route through gateway | 4 | 0 |
| Server reply route changed after first exchange | 3 | 0 |

The namespace-local `AF_PACKET` capture retained 128 test TCP frames. All fit the
160-byte snapshot limit; the largest was 130 bytes. Kernel packet statistics
reported 416 received packets and zero drops; that count includes packets outside
the filtered TCP records and bridge observations. The filtered records contain
handshakes, acknowledgements, payload and teardown. Each case uses a distinct
source port and a 1.25-second post-client capture window. Positive controls prove
the gateway path and capture were functioning. This is a short finite test, not a
long retransmission or sustained-transfer measurement.

(raw evidence retained privately; see the publication audit) includes frame hex, relative
capture times, child exit results, namespace identities and SHA-256 hashes of the
probe and runner. Reconstructing a temporary Ethernet PCAP from those records and
reading it with `tcpdump -nn -e -r /tmp/ananas-socket-routes.pcap
'ether dst 02:aa:00:00:00:01'` independently decoded 27 control packets addressed
to the gateway. Filtering for the six constrained flows returned none. The PCAP
used relative timestamps, not wall-clock capture dates; the JSON is authoritative.

All owned children were reaped. Independent exact-PID checks confirmed both
recorded namespace holders absent. Anonymous namespaces disappear with their last
process/reference; no global network cleanup command was used. Python unit tests
also verify host-namespace refusal before commands, fixture-only packet selection
and buffered event reading. The harness caps its lifetime at 90 seconds, capture
at 2048 records, pipe events at 2 MiB and echo-server connections at 32.

## Remaining acceptance

The PC fixture covers new gateway/default routes, established client/reply route
changes and priority routing rules. Complete interface/address removal, live
VPN/NAT behavior, longer loss/retransmission windows and actual transfer recovery.
Repeat applicable cases on the supported QNAP kernel without changing
unrelated NAS networking. Network-event cancellation is now implemented and tested
with read-only PC kernel subscription plus synthetic event/lifecycle checks and
the local TLS/inotify worker. Six actual kernel faults now also pass in an isolated
PC namespace with the real monitor/supervisor and lifecycle doubles: route/address/
link loss, interface recreation, a blocking rule and a gateway route. This adds
event/recovery evidence without content or packet capture. Validate physical/QNAP
transfer faults and the final
deployment's OS policy before closing M2 or claiming LAN-only autosync.

The earlier twelve-case capture does not exercise the new monitor. Its explicit
buffers, subscriptions, cancellation lifecycle and evidence are documented in
[network lifecycle](network-lifecycle.md). It is not a firewall/NAT monitor or a
replacement for the socket and deployment egress controls.
