# Authenticated client progress

The native transfer API now accepts `POST /v1/ack`. It advances only the logical
client selected by the verified TLS certificate allowlist, with both client and
server writes settings required. The namespace header, LAN gate, serialized
operation slot and 45-second deadline apply. Production writes remain disabled.

The framed request contains `namespace`, `policy`, `expected`, `through` and client
`exclusions`, limited to 16 KiB plus the four-byte frame prefix. Unknown fields,
query parameters and trailing data are refused. The policy must match this
namespace and both current exclusion configurations. The response is at most 1 KiB
and must echo the exact namespace, policy and through cursor before PC confirmation.

One request advances **1–32 contiguous committed batches**. The journal verifies
every referenced committed sequence, then atomically saves policy binding and
cursor. Gaps, future/uncommitted sequences, changed policy and unexpected cursors
fail without partial advancement. At most 64 logical clients can acquire bindings;
first registration examines at most 64 small policy records. Existing clients use
exact metadata lookups. No content or visible tree is read. Nonzero legacy unbound
cursors require reconciliation, and the older single-step internal method cannot
bypass an established policy binding. Identical retries perform no database write.

The PC persists three distinct cursors:

- `Received`: batches durably queued in the inbox;
- `Completed`: batches with durable applied/exclusion/conflict bookkeeping;
- `Acknowledged`: the completed prefix confirmed by the NAS.

The invariant is `Acknowledged <= Completed <= Received`. Existing indexes read
the new field as zero. Page receipt never acknowledges completion. The worker sends
one bounded cumulative request after completing a page and handles pending
confirmation before fetching more history. Older accumulated progress is caught
up in ranges of at most 32. Local work retains its turn between full pages.
An error leaves the old confirmed cursor for the next external wake/restart to
retry. A caught-up worker sends nothing; acknowledgements do not emit content
change hints. There is no acknowledgement poll, heartbeat or internal retry timer.

An acknowledgement is an authenticated **claim of durable bookkeeping**, not proof
that all visible bytes are identical. Exclusions or deferred conflicts differ
from applied content. The NAS cannot inspect the PC's disk; this endpoint also
does not prove prior delivery of the claimed page. No garbage collection uses the
cursor yet. Safe history retirement must pin current bases, in-flight/conflict
versions and account for **every configured participant**, including clients that
have never acknowledged anything. Client removal/reset and policy changes need
explicit reconciliation; no clock-based expiry or automatic retirement exists.

## Local evidence, 2026-09-09

With temporary Go caches, `go test -race ./...` passes. Journal tests cover a full
32-batch range, gaps, restart/idempotence without another bbolt write, policy
changes, legacy cursor refusal and the 64-client cap. Index tests refuse
received-only/future acknowledgement and retain pending confirmation across
reopen. TLS tests bind progress to the authenticated client, reject a forged
client field and refuse mutations in read-only mode.

The worker test simulates losing the reply after remote acceptance, reopens the
index and retries the same range; this is not packet-loss evidence. The real local
TLS/inotify worker completes uploads, downloads and deletion and verifies that
the NAS cursor matches durable local completion. Its existing 150 ms idle check
after settling still adds no encrypted bytes or metadata reads. These are local
functional/short-idle tests, not sustained CPU/RSS, QNAP or power-loss evidence.
Recorded QNAP runs predate this endpoint; deployment and historical-version
reclamation remain unfinished.
