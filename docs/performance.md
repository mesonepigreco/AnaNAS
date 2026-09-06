# Local performance measurements

Measured 2026-09-06, Linux/amd64, Go 1.27.1, Intel Core Ultra 7 270K Plus.
These are local-development results, not NAS transfer acceptance. Test data and
state were disposable; no NAS endpoint was used.

## Local observer

```sh
go build -o /tmp/nas-sync-observer ./cmd/nas-sync
python3 scripts/measure_local.py --binary /tmp/nas-sync-observer --idle-seconds 10 --files 1000
```

| Measurement | Observed |
|---|---:|
| Tracked regular files | 1,000 |
| Idle window | 10.000 s |
| Idle CPU time / CPU percentage | No measurable ticks / 0.0% of one core at `/proc` resolution |
| Idle resident memory | 9,019,392 bytes (~8.6 MiB) |
| Idle disk bytes read/written | 0 / 0 |
| 500 writes to one path, including 2 s settling | 0.01 s process CPU; ~9.0 MiB RSS |

The harness accelerates initial metadata enumeration with `scanOpsPerSecond=10000`;
shipping defaults pace at 50 operations/s. No CPU quota was applied to this run.
Disk counters omit reads served by the page cache. The idle result is not evidence
of literally zero CPU consumption or of passing the planned ten-minute churn budget.

## 100,000-file check

The same harness with `--files 100000 --idle-seconds 10` recorded:

- 61,538,304 bytes RSS (~58.7 MiB), below the proposed 128 MiB idle budget.
- No measurable idle CPU ticks, and zero disk bytes read/written during the window.
- About 0.01 seconds process CPU for a 500-write burst, with RSS unchanged.

This fixture used one directory containing 100,000 regular files. It does **not**
validate kernel memory for 100,000 directory watches or the ten-minute churn budget.
Initial and restart scans remain paced; the default 50 operations/s intentionally
makes a tree this large take much longer to enumerate than the harness profile.

## Hashing

16 MiB in-memory input, fixed 64 KiB blocks, `go test -bench . -benchmem`:

| API | Throughput | Allocated bytes/op | Allocations/op |
|---|---:|---:|---:|
| `Stream` | ~5,189 MB/s | 65,632 | 3 |
| `BlockDigests` | ~5,217 MB/s | 81,984 | 12 |

The first implementation allocated a reader wrapper per block (259 allocations
for this input); moving it outside the loop removed that cost. Streaming retains
one block buffer. Collecting digest APIs retain O(number of blocks) metadata.
These unpaced microbenchmarks measure library throughput, not daemon CPU budgets.
The local observer does not invoke the hash library.

## Network check

```sh
python3 scripts/check_no_network.py --binary /tmp/nas-sync-observer
```

A child-process `strace -f -e trace=network` recorded **zero network syscalls**
during local daemon startup and two seconds idle. This includes no outbound DNS
or SSH requests. The trace requires permission to trace a child process. It covers
only the current local observer; it is not a substitute for WAN packet capture
and route-change tests once NAS transports exist.

## Remaining acceptance work

Repeat the 100,000-path memory test and run ten-minute idle/churn workloads on
the target machine. Test scan storms, large directory moves and slow/full storage. Use the
systemd example to enforce a measured CPU ceiling if desired. NAS-side CPU, wire
bytes (both directions), offload, SFTP latency and route loss require the real
NAS/deployment described in [e2e-qnap.md](e2e-qnap.md).
