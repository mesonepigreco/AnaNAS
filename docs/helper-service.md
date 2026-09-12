# Native anaNAS helper

The user has now requested a usable live installation ahead of broader fault
acceptance. Explicit `liveWrites: true` is implemented for that LAN trial; it is
mutually exclusive with `-write-test` and defaults false. It retains native path,
non-admin identity, TLS allowlist, direct-route/socket checks and cache admission.
The new local live-mode test verifies actual publication; default read-only and
disposable modes still pass. The historical disabled-production statements below
describe earlier builds. The trial is now installed; [live file evidence](live-trial.md)
records real transfers, pause/resume and helper/PC service restarts.

With `observerStateDir` configured, the helper also runs native inotify metadata
observation and a single serial capture worker. Its 0700 metadata directory must
be separate from both visible root and publication state. It scans metadata once
at startup, then consumes bounded coalesced events (50 operations/s, 8192 watches,
1.5 s quiet/5 s maximum wait). Regular-file creates/edits become immutable journal
versions without rewriting their visible paths. Comparison avoids feedback commits;
equal-size changed files may incur two paced source passes. New external
directories now seal stable identity receipts. Confirmed deletion of an observed
regular-file leaf seals an absence receipt after verifying and flushing its
accessible parent. These imports retain prior versions without rewriting visible
paths. Directory-tree deletion and never-observed journal-head reconciliation
remain pending. Actual local inotify/TLS tests cover creates, edits, directories,
deletion/re-creation, no feedback versions and preservation of a conflicting PC
edit; real NAS file/folder/deletion evidence is in `live-trial.md`.

The native API now includes authenticated, policy-bound `POST /v1/ack`, guarded by
the helper's writes setting. It records at most 32 committed batches per request
for the certificate-mapped client, with no content reads or idle traffic. Local
TLS/inotify tests pass; the recorded QNAP runs below predate this endpoint.
[Client progress and retention limits](acknowledgements.md) describe its scope.

`cmd/ananas-helper` now assembles the native journal, staging, publication and
authenticated transfer API. The installed PC daemon still has no production
transfer worker. **Production writes remain disabled.** This executable has only
been exercised as a bounded disposable QNAP service, not installed permanently.

The helper accepts an inherited TCP listener, checks its exact IPv4 address,
port and `SO_BINDTODEVICE`, and requires the configured non-root real/effective
UID, GID and supplementary groups. The small root launcher binds the socket,
starts that non-root child and waits for it. The launcher never reads the helper
configuration or TLS key contents. On this QNAP it used UID 1000, GID 100 and
supplementary group 100; the API did not run as the SSH administrator.

Configuration is strict JSON, at most 64 KiB, with explicit bounds and no default
write activation. [The example](../config.helper.example.json) contains placeholder
identities and paths, not an installable production profile. Configuration and TLS
files must be private, owned, single-link regular files on native storage, reached
without symlinks. Root and state must be disjoint native directories on the same
mount; staging state must be private. Preflight with `-check-config` validates
identity, TLS, roots and network without creating a journal or listener.

`maxCacheBytes` and `maxCacheEntries` are now mandatory (example: 32 MiB and 128).
Service opening establishes or verifies a durable publication budget before the
API is exposed. Fresh budget initialization refuses existing unaccounted payload
or history. Exact retries preserve their charge; full capacity rejects new work
with HTTP 507 before staging. Committed-history credit remains unfinished; PC mode
now separately accounts for upload spools/downloads and verified preparation abort.
[Accounting scope and local evidence](cache-budget.md) distinguish
these reservations from a physical disk quota. The QNAP runs below predate this
change and do not validate budget exhaustion or restart.

The service uses TLS 1.3, a private client CA and leaf-certificate allowlist. The
PC also pins the server certificate and binds its source address/interface. Local
netlink checks require the configured address/prefix, physical carrier and direct
route without a gateway, multipath or alternate interface. Checks run at startup
and application gates, without DNS, ping, directory scanning or idle polling.
These checks and socket binding do **not** provide the required OS egress policy
or prove behavior across route changes; that acceptance gate remains open.

`-write-test` requires a positive lifetime no greater than five minutes and exact
`root`/`state` children of a private `ananas-helper-test-<32 lowercase hex>` directory
under `nas-sync-capability-test`. The API advertises `scope: disposable-test` only
in this mode. The test client additionally requires a fresh namespace before
writing. No flag or configuration field enables production publication.

Cancellation closes connections, cancels request contexts and joins admitted
application handlers before closing native state. Close errors propagate to the
process exit status. Uninterruptible kernel I/O can still delay termination.
The helper uses `GOMAXPROCS=1`, a 64 MiB Go soft memory limit and nice 15;
these are not hard process CPU/RSS quotas. Write tests additionally have 30/45 CPU
second soft/hard limits. Accepted connections are capped at 1–8, work remains
serialized, and content uses the configured read pace. See [resource scope](performance.md).

The helper now also exposes an optional authenticated committed-sequence stream.
It permits `min(4, maxConnections-1)` streams, at most one per client ID, leaving
one accepted connection outside that allowance. Thus the example permits one
notification stream and `maxConnections=1` disables streams. No new configuration
field is required. Frames coalesce actual changes at one per second and idle
streams have no heartbeat. [Protocol and limits](transfer-api.md) describe local
TLS/worker/shutdown tests and subsequent QNAP evidence are recorded below.

## QNAP notifications and socket restrictions, 2026-09-09

The expanded probe now passes three read-only checks (state, initial notification,
two-second quiet stream) and ten write checks, adding committed-change delivery
to the earlier create/diff/download/delete fixture. Notifications occupy a separate
connection while the same client performs transfers. The read-only quiet window
showed zero added encrypted bytes. This is not a sustained idle or packet test.

First run: (raw evidence retained privately; see the publication audit),
using `/tmp/ananas-helper-events-arm64`, `/tmp/ananas-helper-launch-events-arm64`
and `/tmp/ananas-helper-events-test`. After adding device binding plus
`SO_DONTROUTE` checks on outgoing, inherited and accepted sockets, the final
run used the commands below, first without `--write`, then with it:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 scripts/run_helper_test.py \
  --session-dir /tmp/ananas-admin-session-s9XERq8a \
  --helper /tmp/ananas-helper-direct-arm64 \
  --launcher /tmp/ananas-helper-launch-direct-arm64 \
  --client /tmp/ananas-helper-direct-test
PYTHONDONTWRITEBYTECODE=1 python3 scripts/run_helper_test.py --write \
  --session-dir /tmp/ananas-admin-session-s9XERq8a \
  --helper /tmp/ananas-helper-direct-arm64 \
  --launcher /tmp/ananas-helper-launch-direct-arm64 \
  --client /tmp/ananas-helper-direct-test \
  --report docs/evidence/qnap-helper-direct-socket-test-2026-09-09.json
```

The final run passed on QNAP Linux 4.2.8/aarch64 as UID 1000/GID 100, with the
same 45-second lifetime and byte/rate limits as the original test. Its write
fixture took 4.832729521 seconds; the one-byte update still reused 512 KiB in each
direction and encoded to 491 bytes. Lifetime helper CPU was 0.245706 seconds and
peak RSS 11,124,736 bytes (about 10.6 MiB), excluding launcher and SSH. Setup
binaries totaled 16,843,610 bytes. Write traffic was 536,481 encrypted bytes sent,
14,353 received; the separate read-only check counted 2,538/2,633 bytes for its
state and notification protocol traffic. Its quiet window lasted 2.001152511 seconds.

Both new runs exited successfully and cleaned their own children. Independent
exact-path checks confirmed absence and unchanged test-parent (`777/1000/100`)
and production (`777/0/0`) owners/modes. (raw evidence retained privately; see the publication audit)
pins the binaries. No production data, permanent helper or global network policy
was changed. [Egress validation](egress-policy.md) remains open: reading socket
flags and passing a direct connection is not a route-failure packet test.

## Original QNAP transport test, 2026-09-09

The exact commands were run first without `--write`, then with it:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 scripts/run_helper_test.py \
  --session-dir /tmp/ananas-admin-session-s9XERq8a \
  --helper /tmp/ananas-helper-arm64 \
  --launcher /tmp/ananas-helper-launch-arm64 \
  --client /tmp/ananas-helper-test
PYTHONDONTWRITEBYTECODE=1 python3 scripts/run_helper_test.py --write \
  --session-dir /tmp/ananas-admin-session-s9XERq8a \
  --helper /tmp/ananas-helper-arm64 \
  --launcher /tmp/ananas-helper-launch-arm64 \
  --client /tmp/ananas-helper-test \
  --report docs/evidence/qnap-helper-test-2026-09-09.json
```

The session is temporary and must be replaced with a new authenticated visible
setup session if expired. The runner never reads existing NAS credentials. It
creates short-lived test certificates locally, uploads only the test server key
and public certificates, and restricts writes/ownership changes to its new child.

Native preflight and authenticated read-only state succeeded. All eight write
checks passed: fresh namespace, directory/file creation, one-byte diff upload,
verified diff download, explicit file/empty-directory deletion, and contiguous
journal/final tombstone, together with the authenticated state check. A 512 KiB
base was reused in each direction; the update delta was 491 bytes with one literal
byte. The write probe took 4.20377939 seconds at a 2 MiB/s service read rate.

The 45-second helper exited successfully as UID 1000/GID 100. Its lifetime usage
was 0.155733 seconds user CPU, 0.069215 seconds system CPU and 10,100,736 bytes peak
RSS (about 9.6 MiB), excluding the launcher and SSH. Setup binaries totaled
16,833,694 bytes. The write probe counted 534,109 encrypted stream bytes sent and
11,418 received, including the initial file and protocol traffic; these are not
packet-level counters. Read-only state separately counted 2,321 sent/2,292 received.

The runner cleaned up only after successful launcher exit and the helper's stopped
record. Independent exact-path `test ! -e` confirmed its recorded child absent;
`stat -c '%a %u %g %F %n'` confirmed the existing test parent remained `777/1000/100`
and production `Nasdir` remained `777/0/0`. No production files were tested.
(raw evidence retained privately; see the publication audit) pins tested binary hashes.
The later change propagating state-close errors is covered by local builds/tests;
the recorded hashes identify the preceding QNAP-tested build.

This fixture does not establish autosync, out-of-band SMB observation, remote
notifications, aggregate storage quotas, multi-client recovery, disconnect/kill/
power-loss behavior, OS egress enforcement or sustained performance. Those gates,
trust provisioning and deployment remain required before production writes.
