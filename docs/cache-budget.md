# Native receiver and PC cache budgets

The native helper now requires explicit `maxCacheBytes` and `maxCacheEntries`.
The example and disposable runner use **32 MiB and 128 entries**. There are no
defaults. Accepted configuration ranges are 192 KiB–8 TiB and 1–1,000,000 entries.
These are limits on conservative logical reservations and retained history entry
count, not a filesystem quota or a claim that 32 MiB supports a production share.
The PC worker now requires an explicitly configured replica budget too. Its local
TLS/inotify fixture uses 256 MiB and 128 entries; these are test settings, not new
defaults or production configuration. The installed observer remains unchanged.

The journal persists the limits and counters under its existing exclusive
lifetime lock. Initialization requires an unused journal and a private native
state directory containing only `journal.db`. PC mode additionally permits the
observer's `index.db`, checking its exact type/mode/owner/link count. Bootstrap
inspects at most two names for receivers or three for replicas in the pinned
directory, never a visible root or CIFS/GVFS path. Existing unaccounted payload,
history or intake is refused. The replica's remote namespace is durably bound.
Reopening an established budget accepts identical limits or a monotonic capacity
increase while preserving exact usage. It refuses decreases and mode/namespace changes;
opening the journal without helper
configuration does not disable its persisted enforcement.

Before a new intake or prepared publication, one bbolt transaction reserves:

- 128 KiB per operation;
- 64 KiB per entry, including directories, tombstones and empty files;
- twice each target file's size, for candidate and publication copy;
- the expected old base size, because its displaced visible inode may be retained.

These allowances are deliberately conservative. They are charged in the same
transaction that establishes ownership, before receiving/reconstructing content
or invoking the publisher. Validation/refusal rolls back the whole transaction.
The receiver still reads the bounded proposal first; a client or network buffer
may already have sent some body bytes. This is not zero network traffic on refusal.

Identical retries use the same reservation. Interrupted intake, prepared
publication, process restart and successful commit all retain their charge.
Budget exhaustion returns HTTP **507** before the delta decoder/stager runs, and
does not claim an intake or prevent a subsequent operation that fits. A fixed
entry limit prevents tiny/zero-byte operations from growing history indefinitely.
There is no idle timer, payload hash, directory sweep or history scan for accounting;
each reservation uses at most 128 exact base lookups and one bounded proposal hash.

The receiver-only mode refuses replica origin binding/adoption. PC mode shares one
counter between downloaded publications and local upload preparations. Uploads
reserve the same fixed operation/entry allowances plus snapshot size and the
encoded wire bound. For a target of `N` bytes, that bound is
`N + 45 × min(N, delta.MaxOperations) + delta.HeaderBytes + 33`: each frame advances
at least one target byte. Tiny files therefore do not reserve the global maximum
frame allowance. The encoder's spool writer enforces this bound. No full-file
fallback is added.

PC reservation occurs after the index durably records preparation and before any
snapshot or wire write. A crash before reservation leaves a preparation with no
content; a crash afterward leaves both reservation and recovery provenance.
Reservation identity includes operation, client, path, expected base, candidate
ID/type/size, purpose and charged amount. Snapshot creation and authenticated
adoption separately verify content digests. Future charge-rule changes require
matching/migrating persisted tokens rather than crediting a different amount.

The automatic worker refuses construction without a matching replica budget and
checks the outbox reservation before dispatch. An old directory-only outbox can
exist without payload files, so bootstrap alone is not accepted as accounting
proof. An unaccounted outbox is retained for reconciliation without sending it.
Adoption also requires a matching upload reservation; completion retains snapshot
and metadata charges but now reclaims its encoded wire allowance as described below.
Download owns a durable intake before its first content request,
reserves candidate/publication capacity there and reuses the charge on retry.
Owned partial files are discarded before verifying/reusing retained candidates.
An unfinished download intake blocks unrelated publication until resolved.

Generic explicit test journals may still omit a budget; the assembled helper and
automatic PC worker require one. These internal APIs do not enable the installed
observer's reserved `limits.cacheBytes` field or production transfer wiring yet.

Explicit abort of an untransmitted PC preparation now removes/fsyncs only its
owned snapshot/wire names and verifies absence before releasing the charge. Abort
cannot credit committed/prepared history or an outbox. Missing reservations allow
only the no-artifact case; unaccounted content is preserved. Retry after charge
release but before the index forgets preparation is idempotent. Cleanup failure
retains provenance and its charge. This is not committed-history reclamation.

Completed PC uploads now reclaim the separate encoded wire spool after verified
local adoption of the authenticated remote commit, while the durable receipt/outbox
still exists. The journal checks the exact committed operation and versions, removes
only owned `.wire.partial`/`.wire.ready` names, flushes the private directory once
for the bounded batch, then atomically credits the wire allowance and marks its
reservation as snapshot-only. It never removes `.ready` content, changes live files,
reduces the history entry count or makes a NAS request. No payload hash, directory
listing, age timer or background cleanup loop is added.

An interrupted removal or flush retains the full charge. Restart retries missing
names safely. Once credit is recorded, an identical retry verifies absence and
does not credit twice; a newly created object at a released name is preserved and
reported. Cleanup refuses unowned hard links, symlinks, directories and unexpected
ownership/modes. Two links are allowed only for the spool's own partial/ready pair.
Sending requires an active wire reservation; snapshot-only reservations permit
completion solely with a saved receipt and matching local committed operation.
The receipt/outbox is cleared only after cleanup and index base finalization.

The credit is the conservative reserved wire bound, not a measurement of physical
bytes freed. Snapshots, fixed metadata allowances and history entries remain
charged. This does not reclaim previous file versions or NAS publication artifacts.

Authenticated bounded client acknowledgements now pass local worker/TLS tests
([protocol](acknowledgements.md)). Safe history retention still needs their validated
integration with all configured participants,
in-flight and conflict pins, and durable cleanup before crediting freed space.
Consequently repeated successful edits eventually stop at the limit, even when older payload
could theoretically be removed. Abandoned NAS intake resolution is also pending.
Do not reset the journal or delete retained files to bypass exhaustion.

Logical bytes exclude physical bbolt page growth, filesystem metadata/allocation,
visible-root capacity and other processes' disk usage. In particular, an external
process with an open writable displaced inode could grow it beyond its recorded
size. There is no global free-space reservation, enforced filesystem quota or
power-loss guarantee. Physical disk limits and safe history reclamation
remain open gates.

## Production capacity correction — 2026-09-10

The live 3.13 GB `SampleDocuments` import contains 9,828 regular files and 810 directories.
The old production profile's 4,096-entry limit was therefore structurally too low
even for the first version of every path; it stopped after five large files at about
39 MB. The deployed PC and NAS configurations now use 1 TiB and 1,000,000 entries,
and both binaries support increasing the persisted budget without resetting history.
Those application ceilings exceed the PC's current physical free space and support
about 100 times the observed SampleDocuments path count. This removes the artificial
4,096-entry/1-GiB stop; it does not pretend that finite disks are unlimited or
complete the still-required safe history reclamation work.

## Local verification on 2026-09-09

With Go 1.27.1 and caches `/tmp/nas-sync-go-cache` and
`/tmp/nas-sync-go-mod-cache`, these commands passed on the PC (host loopback
access was required for TLS tests):

```sh
go test ./internal/transferapi ./internal/helper ./internal/journal ./internal/stage ./internal/replica ./internal/delta
go test -race ./...
```

Tests cover refusal before callbacks, transaction rollback, interrupted intake
and reopen, process kill while a partial file exists, recovery without a second
charge, committed retries, retained deletion bases, the empty-file entry cap,
conflicting heads, refusal of unaccounted state and refusal of replica bypass.
The TLS refusal test deliberately omits its declared delta: it receives 507 before
an EOF decoder error, leaves no candidate/visible file and then publishes a smaller
fitting upload. The full local native helper test performs create, 512 KiB upload,
one-byte update, diff download and deletions; its five commits retain exactly
**4,128,771 logical reserved bytes and five entries**. These are accounting values,
not measured physical disk usage.

PC tests additionally cover upload reservation/refusal before snapshot creation,
reopen and shared download/upload capacity, identity/purpose mismatches, refusal
of unreserved adoption/dispatch, safe abort credit after process kill, cleanup
failure without credit, download refusal before network and retry accounting.
The real local inotify/TLS worker completes bidirectional operations with its
budget enabled and retains snapshot/history charges after completion. It now
checks that completed spools are absent and retained snapshots remain available
for subsequent diffs. Wire-bound tests exercise
empty, literal and mixed copy/literal encodings from zero bytes to 512 KiB, plus
size-limit arithmetic without allocating an 8 GiB fixture. A process-kill test
stops after wire unlink/credit but before index finalization, then completes through
the worker with no network implementation, preserving newer local edits and not
crediting twice. Partial batch cleanup with an unowned hard link retains the full
charge across reopen and resumes after that test-owned obstruction is removed.

The recorded QNAP helper/socket evidence predates this budget change. This version
has not yet been tested for exhaustion/restart on QNAP. Production automatic writes
remain disabled while reclamation and other acceptance gates
are incomplete.
