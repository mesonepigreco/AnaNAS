# Synchronization reliability audit — 2026-09-13

## Scope and immediate failure

This audit reviewed local selection and comparison, preparation and abort,
upload dispatch and completion, remote replay, native NAS observation,
connection/session recovery, resource bounds and control-panel reporting.
The live error was `invalid sync path`: one Linux file had a backslash name,
which the synchronization protocol does not support. Selection called a strict
path-state API before isolating unsupported names, stopping the entire pass.

The broader finding was that a source-specific failure could discard the current
batch selection and end the whole pass. Fixing one file exposed the next failure.
The changes below isolate failures where no remote operation has been sent;
they do not discard uncertain committed work to make the status appear healthy.

## Changes and checks

| Failure class | Handling after the audit | Evidence |
| --- | --- | --- |
| Unsupported name, type or oversized source | Shared validation before strict path APIs; leave pending with reason and continue through later pages | Index tests and real TLS worker test with backslash name, symlink, oversized file and healthy neighbors; pagination regression with an unsupported name as the page cursor |
| Source permission or missing-leaf/parent error | Record a generation-bound file issue; continue unrelated sources | TLS unreadable-source test; root-unavailable boundary test |
| Source changes during comparison | Refresh only that source's accessible metadata, defer it for the pass, then retry with bounded backoff | TLS race injection verifies the newer bytes eventually arrive |
| Indexed files grow between selection and preparation | Reselect an aggregate batch that no longer fits; isolate a source exceeding its individual limit | Regression verifies reselection without creating an oversized preparation |
| Source changes after a snapshot is created | Abort only untransmitted, provenance-owned candidates; release their reservation before another batch | New interrupted-snapshot isolation test plus existing killed-process preparation test |
| Local comparison conflict or unsupported type transition | Keep the path pending for reconciliation; do not select a conflict winner or prevent other eligible local files from uploading | Per-path issue lifecycle and existing conflict/comparison tests |
| Temporary NAS HTTP service error | Retry 408/429/500/502/503/504 with bounded backoff | Status-code table test and TLS worker outage injection |
| Authentication/protocol rejection | Do not blindly retry 400/401/403/404/409/413/422 | Status-code table test |
| Lost upload reply or restart after commit | Query the durable outcome before opening/resending spools; preserve the outbox | Existing TLS dispatch recovery test with missing local payload |
| Restart after wire reclamation | Complete from the saved receipt and immutable snapshots | Existing reclaim-before-finalization regression |
| LAN loss, pause or stream failure | Cancel active I/O, retain work and re-establish the session within LAN policy | Existing full TLS worker lifecycle and network join/retry tests |
| Deletion already observed on both sides | Verify retained history and durable absence; preserve recreations | Existing publisher restart/recreation tests and inbox-progress test |
| Cache exhaustion, storage I/O failure, identity mismatch or unavailable/replaced root | Stop affected transactional work with an explicit error; do not quarantine it as an ordinary bad file | Budget, corruption, identity and new global-failure boundary tests |

File issues survive a daemon restart. A new observed generation or downloaded
base invalidates the old issue. Confirm sync/Confirm all clears retryable-by-user
file issues and queues another attempt, but cannot make an unsupported name or
type transferable. Successful completion clears the issue and pending request.
Transient issues are released at the start of the next bounded retry pass.

When the eligible queue drains with isolated failures remaining, the status
reports partial completion rather than presenting it as a stalled global worker.
Pending files continues to show the unresolved paths and reasons. Unsupported
names are not renamed, excluded from accounting, or falsely acknowledged.

## Validation commands

All Go commands use `GOCACHE=/tmp/nas-sync-go-cache` and
`GOMODCACHE=/tmp/nas-sync-go-mod-cache`:

```sh
go test ./...
go test -race ./...
go vet ./...
go build ./...
go test ./internal/transferapi -run 'TestTLSWorkerIsolates|TestOnlyTemporary' -count=1 -v
git diff --check
```

The five TLS fault-injection scenarios passed, including automatic recovery
after a temporary service failure, an observed file changing mid-comparison,
replacement by a symlink and manual retry after restoring file permissions.
Full unit/race suites, vet and builds passed. Production verification is recorded
in [live-trial.md](live-trial.md).

## Remaining boundaries

This is not a proof that synchronization can never stop. A prepared remote
publication still serializes journal work: corruption or a conflict at that point
requires recovery/reconciliation, not skipping its sequence. Remote conflict
choices, general directory-tree deletion, never-observed remote-head reconciliation,
automatic history reclamation and broad power-loss/NAS reboot acceptance remain
open. A missing parent is not treated as proof that all descendants were deleted.
ENOSPC/EIO, exhausted retention budgets and identity/policy failures cannot safely
be hidden by continuing to publish dependent changes.

The audit tests use real local filesystem operations and TLS sockets, plus named
fault injections. They do not claim exhaustive hardware fault testing or completed
multi-PC conflict acceptance. Wider filename/encoding compatibility and long
soak tests remain useful follow-up acceptance work.
