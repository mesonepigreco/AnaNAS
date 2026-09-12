# Network sessions and recovery

`internal/netwatch.Monitor` now supplies a local Linux kernel notification source
to `daemon.RunWithNetwork`. The local TLS/inotify worker test uses this path.
The installed observer still has no transfer worker or network monitor; production
automatic writes remain disabled.

The monitor subscribes to routing multicast groups **before** resolving the exact
configured interface index through `SIOCGIFINDEX`. This ioctl uses a transient,
unconnected control socket; it sends no IP packet, performs no DNS lookup and
does not dump every interface. Subscription includes link, IPv4 address/route/rule
and next-hop object notifications. A missing interface subscribes with no index
and re-resolves after any link event, so remaining offline needs no interface poll.
Denied/unsupported subscriptions and invalid settings prevent transfers. Next-hop support is mandatory;
this path has been tested on the PC, not the QNAP's older vendor kernel.

The daemon starts local observation independently of network availability. Every
transfer session waits for subscription readiness, then calls the mandatory
`NetworkOptions.Check` local-only LAN gate before starting the worker or remote
stream. A denied check reports `offline` and waits for a kernel event or shutdown;
there is no timer, NAS request or repeated local check in this state. Per-operation
gates still apply after session admission.

A relevant notification or uncertainty cancels the session directly, including
a quiet remote stream. The worker, stream and monitor are joined before reuse;
the client is interrupted before and after joins to discard pooled sockets.
Notification-stream failure also cancels the session. Subscription/session errors
retry at 1, 2, 4, 8, 16, 32, then 60 seconds, capped there. An attempt lasting at
least 60 seconds resets the delay to one second. There is no retry timer during
a healthy session or denied-policy wait, and each new attempt subscribes and
checks policy again before any connection. Recovery after a reported NAS stream
failure does not require a PC route event. No jitter is implemented yet.

Local observation, the index and controls remain alive throughout these retries.
Local edits remain in the durable dirty index; remote replay uses its durable
cursor. Reconnect does not invoke observer startup or rescan the tree. A fatal
observer, status-server or worker-loop error still stops the whole daemon.
`Surface.Network` supplies checking/connecting/active/offline/retrying/stopped
transitions; its nonblocking callback is not yet wired to the deployed dashboard.
`active` means a session has received a valid initial hint, not that files have
finished synchronizing. Cancellation is cooperative; kernel filesystem calls can
still take their own timeout.

Relevant events include configured-interface link/IPv4 address changes, all IPv4
route/rule changes, and next-hop changes (including group objects without a family).
Unrelated interface link/address events and IPv6-only address/routes are ignored;
sync transport remains IPv4-only. Unknown message types/families, malformed framing,
receive errors, non-kernel sources, truncation or overrun require revalidation.
Route filtering is deliberately conservative: an unrelated IPv4 route change can
also restart the transfer session. Notification loss is never interpreted as an unchanged
network or repaired by assuming old policy evidence is current.

Resource bounds are explicit:

- one monitor goroutine, with no idle timer, ping or heartbeat;
- one netlink socket and one cancellation eventfd while watching;
- one 32 KiB userspace receive buffer, without a message queue;
- 64 KiB requested socket receive buffer, requiring reported size at most 128 KiB;
- at most 64 ignored datagrams per subscription before forcing revalidation.

The session supervisor adds one goroutine, one-slot completion/hint channels and
one retry timer only after failure. Failed attempts never overlap; controls and
local observation do not restart. Unsupported monitoring retries only local
subscription setup, with the same capped backoff, and cannot start transfers.

The last bound also applies to unrelated events accumulated over a long lifetime;
it is a conservative processing limit, not a measured drop counter. `poll` blocks
without a timeout and consumes no periodic wakeup. Context cancellation writes to
eventfd; a started cancellation callback is joined before descriptors close or
can be reused. No content, visible directory or NAS mount is inspected.

This is cancellation support, **not OS egress enforcement**. It does not observe
firewall/NAT changes, ARP/proxy behavior or a silent physical failure that the kernel
does not report. Existing source/device binding, `SO_DONTROUTE` and per-operation
LAN checks still apply. Actual fault injection, full recovery, final deployment policy
and QNAP acceptance remain open before M2 or LAN-only autosync can be claimed.

## Local evidence, 2026-09-09

With temporary Go caches and host permission for read-only netlink/loopback sockets,
`go test -race ./internal/netwatch ./internal/daemon ./internal/transferapi` passes.
Tests validate event framing/filtering, subscription refusal, canceled startup,
readiness before transfers, network-failure propagation and service joins. Real
kernel subscription/cancellation runs a warmup plus 32 cycles and leaves the same
`/proc/self/fd` count. The real TLS/inotify worker performs bidirectional transfers,
acknowledgements and spool cleanup with the monitor attached, and its existing
short idle test still passes. The missing-interface test subscribes to a name
verified absent by ioctl, then cancels it; synthetic link creation tests index
re-resolution selection.

Lifecycle tests now exercise offline controls, subscription and backoff shutdown,
policy denial without repeated checks, stream failure without a route event,
revalidation before reconnect and a deliberately delayed worker shutdown that
prevents any overlapping replacement session. The expanded real TLS/inotify test
injects lifecycle interruptions around the actual netlink monitor, makes a local
edit and a separate authenticated remote commit while the PC gate denies access,
then verifies both contents converge after recovery. Its observer Run count stays
one; the interruption alone causes no additional scan. File reconciliation can
still increment the observer's general scan counter. The disconnected client's
encrypted counters remain unchanged in the short offline window. These are local
temporary roots and a synthetic gate, not the physical LAN or a real NAS restart.

The checks above use synthetic lifecycle interruptions. The actual kernel fault
fixture below adds separate evidence. The earlier socket-routing capture predates
the monitor and does not test its cancellation path.

## Isolated kernel faults, 2026-09-09

`internal/daemon/network_isolated_test.go` runs only with an explicit host namespace
identity, effective root, a different namespace from both that identity and PID 1,
and an initial interface set containing only loopback. It creates one disposable
`lan0`/`peer0` veth pair with `10.88.0.2/24`. It never enters another namespace or
touches a NAS/mount. The regular test suite skips this test without its opt-in flag.

Read the original host identity in the host namespace, then build and invoke:

```sh
readlink /proc/self/ns/net
GOCACHE=/tmp/nas-sync-go-cache GOMODCACHE=/tmp/nas-sync-go-mod-cache \
  go test -race -c -o /tmp/ananas-network-isolated.test ./internal/daemon
pkexec /usr/bin/unshare --net /tmp/ananas-network-isolated.test \
  -test.run '^TestIsolatedKernelNetworkRecovery$' -test.v -test.timeout 80s \
  -ananas-host-netns 'net:[4026531833]'
```

The namespace value above is this run's identity, not a portable default. The
test has a 70-second context, two-second command limits and 16 KiB combined output
limit per `ip` command. It mutates only its veth pair and exact test routes/rule,
then verifies that only loopback remains. All commands are executed directly,
without a shell; no long-lived helper or named namespace is installed.

All six cases passed on PC kernel 7.0.0-30-generic in 24.71 seconds (Go's rounded
test duration): connected-route deletion, IPv4 address deletion, link-down,
interface deletion/recreation, a priority-100 blackhole rule and an explicit
gateway route. The actual monitor and session supervisor consume kernel events.
The observer/worker/remote are lifecycle doubles; the fixture gate checks the
virtual link, exact address and kernel route lookup without sending an IP probe.

Every case verifies gate denial, worker/remote stopping, no restart or repeated
gate check during 1.1 seconds offline, responsive controls, a continuously running
observer and recovery after restoring kernel state. Observed command-start to
service-stop times were 0.959–14.958 ms; restoration-to-restart was 2003.541–2005.820
ms, including the supervisor's two-second retry delay. These timings include
command execution and scheduling; they are not isolated kernel latency or a
worst-case guarantee. See (raw evidence retained privately; see the publication audit).

The same binary was explicitly invoked as root in the host namespace without
`unshare`; it refused at the namespace guard before mutations (expected exit 1).
The regular daemon race suite and repository-wide `go vet ./...` also pass.

This fixture does not transfer data or capture packets. The local TLS/inotify
recovery test remains separate. Physical LAN, QNAP vendor kernel, silent failure,
overflow, live VPN/NAT, final OS egress enforcement and sustained CPU/RSS remain
open before production automatic writes can be enabled.
