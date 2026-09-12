# Experimental delta and staging primitives

Implemented in `internal/delta` and `internal/stage`; not connected to the
observer or any NAS transport. Automatic writes remain disabled. The receiver
must execute next to its immutable base: on the NAS for uploads, on the PC for
downloads. Calling the generic receiver with a PC-side CIFS base would read
unchanged payload across the network and is not an acceptable integration.

The separate local `internal/manifest` encoding now distinguishes a directory
from an empty file. Its existing version-1 32-byte header uses byte 8 values
0 (file), 1 (tombstone) and 2 (directory); 3 and unknown values are rejected.
Directory/tombstone manifests have zero size and zero blocks, followed by the
64-byte version ID. Old file/tombstone encodings remain unchanged. Older readers
reject the new directory flag instead of interpreting it as file content.
This metadata type extension does not alter the file delta stream format.

`stage.ReceiveVerified` additionally accepts a bounded sequential content
producer for transports that verify completion after emitting decoded bytes.
The writer enforces exact approved size, hashes written bytes and latches output
errors even if the producer ignores them. A producer error, including a failed
final HTTP trailer, prevents ready-name creation. It uses the same exclusive
partial/flush/link lifecycle as delta receipt. This adds hashing CPU but no
second full-file read. Late storage-flush errors still require recovery of any
ready name that may already exist; readiness is never inferred from its suffix.

For native delta uploads, `stage.ReceiveDeltaVerified` checks the approved target
size in the header before creating output and compares the decoder's final
size/digest before sealing. It uses the decoder's whole digest, adding no second
hash of reconstructed bytes. The transfer handler runs it under durable journal
intake ownership; exact retries can verify/reuse owned candidates and discard
abandoned partials. This is separate from adopting unknown files or aggregate
storage quotas. See [intake protocol](journal-protocol.md#upload-intake-ownership).

For local source capture, `stage.Capture` returns the digest it computes while
writing an exact-size candidate. Its producer must verify source provenance;
it does not compare against an externally supplied digest. The content snapshot
builder checks native source fingerprints/path identity before returning success.
Encoded upload spools use separate `ID.wire.partial` and `ID.wire.ready` names,
the same exclusive creation/flush lifecycle, and explicit byte limits. They
cannot replace a content candidate. Their size/digest is retained in the upload
outbox and checked before use after restart. No automatic orphan cleanup or
aggregate quota enforcement is implied by these file-level primitives.

All integers are unsigned big-endian on the wire, validated before allocation.
Digests are 32-byte BLAKE3. Block sizes are powers of two from 1 KiB to 4 MiB;
both files are limited to 131,072 blocks (8 GiB at the default 64 KiB). Higher
block sizes raise that size bound and working-buffer cost; the scheduler must
also enforce its configured per-file and aggregate spool limits.

## Signature version 1

The 56-byte header contains magic `NSSG0001`, block size (4 bytes), block count
(4), base size (8), and whole-base digest (32). Each fixed-block entry contains
a rolling weak checksum (4) and strong digest (32). Length/count checks happen
before allocating entries. Maximum serialized size is 4,718,648 bytes.

The weak checksum packs the modulo-65,536 sum of bytes and weighted sum, with
the first byte weighted by window length. Rolling updates are constant work.
Only a matching strong digest authorizes a copy. Full base blocks can match
any target offset; a partial final base block can match the target suffix.

Building a signature reads the base once. A caller must verify stable identity
and generation before/after, then retain or otherwise ensure an immutable base.
An unchanged base's cached signature need not be rebuilt or retransmitted.

## Delta stream version 1

The 60-byte header contains magic `NSDL0001`, block size (4 bytes), base size
(8), target size (8), and expected base digest (32). Frames follow:

| Tag | Fields after the tag byte |
|---|---|
| 1, literal | length (4), digest (32), payload (length) |
| 2, copy | base offset (8), length (4), digest (32) |
| 0, end | whole-target digest (32), then EOF |

Each data frame contains at most one block. Copy offsets align to base blocks;
a short copy must end at the base's end. A frame cannot exceed remaining target
length. At most 393,217 data frames are accepted. Every reconstructed frame is
verified before writing to staging; exact total size, whole digest and EOF are
required for success. The expected base identity comes from the caller's
authenticated, versioned protocol, not an untrusted stream's claim alone.

Encoding uses two block buffers, a 64 KiB input buffer and bounded maps over the
base signatures. It scans target content once and checks cancellation between
I/O/frames and during scanning. Weak-checksum matches trigger at most
`4 * ceil(targetSize/blockSize) + 1024` strong window checks before failing with
`ErrVerificationBudget`. It never silently restarts as a full-file transfer.
Entirely new content naturally needs literal payload for all its bytes.

Callers own read/transfer pacing and deadlines; context checks cannot interrupt
a blocked generic reader/writer. Codec statistics report literal/reused content
and operations, not total wire bytes or durable acknowledgement.

## Private staging lifecycle

`stage.Open` pins an existing canonical native-filesystem directory owned by
the effective user with mode 0700. It opens each path component without
following symlinks using `openat`, rather than requiring the PC's `openat2` API.
The integrating configuration must verify the directory is outside both roots.
The store rejects CIFS, SMB, NFS and FUSE. It does not enumerate directories.

Each operation has a 64-character lowercase hex ID. `Receive` exclusively
creates `ID.partial`, reconstructs/validates into it, makes it read-only,
flushes and closes it, then exclusively links `ID.ready`, removes its partial
name and flushes the directory. Existing ready objects are never overwritten.
Failures return no publication permission; late flush failure can leave a ready
object that must be checked against the durable transaction during recovery.

A killed process can leave a partial or, at the link/unlink boundary, two names
for the same object. `OpenReady` refuses the partial and a multiply-linked ready
object. `DiscardPartial` removes only a known operation's partial name, after
the future coordinator has proved no receiver still owns it. There is no timed
lock stealing or automatic sweep. Ready objects still need digest/length
verification against the transaction after restart. File read-only mode does
not make an owner-controlled store tamper-proof.

This package creates a staged object, **not publication to the synchronized
path**. A separate coordinator and native file publisher now have local
integration tests; see [publication](publication.md). Full generation/base
comparison in the scheduler, retention, aggregate quota and conflict resolution
remain required integration work.
Local process-kill and recovery tests do not prove power-loss durability or
the NAS filesystem's flush behavior. No native NAS execution has been validated.
