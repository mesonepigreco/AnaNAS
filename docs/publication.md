# Native file publication: implemented scope

`internal/publish` implements `journal.Publisher` for regular files on a native
Linux filesystem, plus explicit directory creates and empty-directory deletes.
It is connected to delta reconstruction, staging and journal
commit in local integration tests. It is not connected to the running daemon
or a NAS transport. Automatic writes remain disabled.

## Files and recovery

The sync root and private state root must be disjoint canonical directories on
the same native mount. Paths open with `openat` and `O_NOFOLLOW`; selected parent
directories must already exist. Descriptor mount IDs are read from local
`/proc/self/fdinfo` to reject child/bind mounts. Failure to obtain that evidence
fails closed. Roots/parents are revalidated before mutation. This does not make
arbitrary concurrent directory renames atomic with the subsequent mutation;
managed publication still requires the coordinator and a validated access model.

Each prepared put has retained `VERSION.ready` data. The publisher verifies and
copies it with a bounded buffer into `VERSION.copying`, flushes it, and renames
that private file exclusively to `VERSION.publish`. A killed incomplete copy
can be removed by its exact ID under coordinator ownership, because `.copying`
is never a visible or displaced file. `.publish` is never truncated on retry.

Before visible publication, a flushed `VERSION.started` marker records that
the namespace operation may have begun. New files use `RENAME_NOREPLACE` so a
racing create cannot be overwritten. Existing files use `RENAME_EXCHANGE`;
the incoming copy becomes visible and the displaced inode remains at the
private `.publish` name. Both directories are flushed. The displaced bytes
must still match the expected base before reporting success.

Deletes are explicit tombstones. The visible inode moves exclusively to
`VERSION.displaced`, is verified against its expected base and remains retained.
Missing visible data alone is not enough to infer successful deletion: recovery
needs the retained expected object, unless the acknowledged base was already
absent/tombstoned.

Recovery recognizes verified incoming visible content and checks any displaced
candidate again. It also verifies retained immutable content before returning
success. An external deletion after an initial create does not silently trigger
recreation: a started operation with a consumed publication copy is uncertain
and stops for resolution. The journal commits only after the publisher succeeds.

## Conflicts and incomplete work

An unexpected visible base fails before mutation. If a writer races the atomic
exchange, its displaced inode is retained and a conflict is reported, including
on subsequent retries. The incoming file may already be visible; the journal
remains prepared and blocks further commits. No automatic rollback, timeout
abort or conflict-winner choice is implemented. Resolution must expose all
retained candidates and durably reconcile the prepared record. A noncooperating
application holding an old descriptor can continue writing that retained inode;
this is not a transaction-consistent snapshot or general CAS against SMB tools.

Directory scheduling and adoption of pre-existing directories, metadata policy,
case collision handling, aggregate staging quota, retention/pruning, scheduler
generation checks, conflict UI, NAS transport and enforced egress are unfinished.
All are integration/acceptance work, not implied by these primitives passing.
Linux `renameat2` flag support, mount-ID availability and durability must be
validated on the QNAP. An ARM64 cross-build alone proves none of those runtime
properties. Multi-file batches have recoverable ordering, not atomic visibility.

## Local evidence

`go test -race ./internal/publish ./internal/journal ./internal/stage` tests real
creates/replacements/deletes and retained content; kills a subprocess after an
actual namespace exchange, reopens the journal and recovers; resumes a partially
published two-file batch; preserves racing writer/create/delete outcomes; and
rejects symlinks, missing/excluded parents and tampered immutable content.

`TestDeltaThroughDurablePublication` inserts one byte at the start of a 512 KiB
file, sends one literal byte and receiver-local block references through the
delta stream (under 1 KiB), reconstructs into staging, publishes, verifies bytes,
retains the base and advances the client's journal cursor. All fixtures are PC
temporary files; no NAS or packet-level measurement is involved. Power loss,
disk-full/flush failures and every cross-protocol recovery boundary remain open.

## Directory publication and its limits

A directory version has a distinct journal flag and no file payload or digest.
Its parent must be committed first. The publisher creates a private, empty
`VERSION.mkdir` directory with mode 0700 and durably saves a 16-byte native
device/inode receipt. Receipt writes use `.directory-writing` followed by an
exclusive rename to `.directory`; an interrupted metadata write cannot become
a completed receipt. The empty directory is moved into place using
`RENAME_NOREPLACE` and directory entries are flushed.

Recovery compares a visible directory's native identity with the receipt.
It rejects a moved/replaced directory with a different identity, and does not
recreate an externally removed directory after its prepared name was consumed.
This receipt is not a general external-writer fencing mechanism: inode reuse
and a concurrent namespace change between checking identity and mutation remain
limits. Directory metadata/ACL preservation is not implemented. No xattr or
state file is placed inside a visible synchronized directory.

Explicit directory tombstones use `unlinkat(..., AT_REMOVEDIR)`. The kernel
atomically refuses nonempty directories, including excluded children or children
created just before removal. No visible child listing, content read, recursive
remove or subtree relocation is used to delete a directory. An operation-scoped
started marker supports recovery after successful removal before journal commit.
Children must be deleted through their own verified transactions before parents.

Untracked same-name directories and file/directory type transitions are refused
for explicit reconciliation; a type transition requires deletion of the prior
type first. Adopting observed existing directories and scheduling these ordered
operations are still integration work. These primitives alone do not make empty
folders synchronize automatically.

Local tests cover nested directory/file publication and bottom-up removal,
interrupted create/delete and metadata preparation, same-name replacement,
exclusions, type-transition refusal and nonempty/racing-child protection.
The native probe now exercises one created parent directory and its final empty
removal. This scoped probe now passes on the QNAP, including as the dedicated
non-admin account, with native create/exchange/delete and prepared recovery.
(raw evidence retained privately; see the publication audit) distinguish
that controlled fixture from pending hardware durability, real process-kill,
disconnect and independent-client acceptance. No production publisher is deployed.
