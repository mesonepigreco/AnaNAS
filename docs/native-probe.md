# Explicit native NAS capability probe

This is a setup test, not the application daemon or a background sync transport.
It writes only when `-write` is passed, and only beneath a uniquely created child
of a pre-existing canonical directory named `nas-sync-capability-test`. It rejects
network/FUSE mounts and symlink traversal. Never run it against the production
sync root, existing user files, a share root or a GVFS path.

The probe generates a 512 KiB base and a one-byte prefix insertion, reconstructs
the delta into native staging, creates a parent directory, creates/replaces/deletes a disposable visible file,
verifies retained bytes, checks competing journal-open exclusion, reopens and
recovers prepared work, removes the empty parent, and advances a contiguous
client cursor over five committed batches. It removes its
own unique fixture directory afterward, including on ordinary reported failures.
It does not list or hash the supplied parent directory's existing contents.

## Build and scoped execution

Build for the prepared QNAP's aarch64 runtime:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  GOCACHE=/tmp/nas-sync-go-cache GOMODCACHE=/tmp/nas-sync-go-mod-cache \
  go build -trimpath -ldflags='-s -w' -o /tmp/ananas-native-probe-arm64 \
  ./cmd/ananas-native-probe
```

After the user signs into the already-authorized temporary SSH session in a
visible terminal, use its private session directory. The runner never requests,
reads or forwards a password; it requires the existing authenticated socket,
checks the direct `enp1s0` route and forbids new proxy/password connections.

```sh
# Read-only SSH/directory preflight; no upload or NAS file creation.
python3 scripts/run_native_probe.py --session-dir SESSION_DIR \
  --binary /tmp/ananas-native-probe-arm64

# Explicit setup upload and scoped test. Runs native read-only mode first.
python3 scripts/run_native_probe.py --write --session-dir SESSION_DIR \
  --binary /tmp/ananas-native-probe-arm64

# Verified dedicated account, foreground execution with UID/GID checks.
python3 scripts/run_native_probe.py --write --as-sync-user \
  --session-dir SESSION_DIR --binary /tmp/ananas-native-probe-arm64
```

The runner inspects exactly `/share/nas-sync-test/nas-sync-capability-test`,
resolves its canonical native location and checks that it remains under the
dedicated test share. Write mode creates a new private `ananas-probe-tool.XXXXXX`
child, uploads only the selected owned ARM64 ELF executable (maximum 16 MiB),
checks its SHA-256 digest, runs metadata-only mode, then the disposable write
mode. It removes the exact uploaded executable and its empty private directory.
No permanent helper, user account, service, automount or firewall change occurs.

`--as-sync-user` verifies the known account UID 1000/GID 100, changes ownership
only of the new `ananas-probe-tool.XXXXXX` directory and uploaded executable, and
uses QNAP BusyBox `start-stop-daemon -S -c nas-sync-test:everyone` without its
background flag. The executable checks real/effective UID/GID and supplementary
groups before accessing the test path. The existing test parent is never chowned;
an explicit path validator and tests reject parent/traversal/other-child inputs.
The command now reports process CPU time and Linux peak RSS through `getrusage`.

An expired local SSH observation does not prove remote completion. On timeout,
the runner reports and retains its exact upload directory for investigation;
verify the remote process before cleanup or another run. A killed probe can
leave its unique fixture directory. Never sweep the shared parent indiscriminately.

## Evidence and limits

PC-local tests, ARM64 compilation and **native QNAP execution passed**. On
2026-09-09 all 11 checks passed first through the admin setup session (4.96 seconds),
then as the dedicated non-admin account (4.724918849 seconds). The non-admin run
reported UID 1000, GID 100, supplementary group 100, 0.269685 seconds user CPU,
0.140705 seconds system CPU and 8,175,616 bytes peak RSS. It reused 524,288 bytes
for one literal byte, with a 491-byte delta and 344-byte signature. The fixture
reported cleanup complete; the exact uploaded tool child was independently
confirmed absent. Production/test-parent ownership and modes were unchanged.
See (raw evidence retained privately; see the publication audit).

The controlled interruption is after the publisher has returned success and
before the journal commits. This exercises recovery ordering, but is not an
actual process kill, disconnect, power loss, disk-full or cross-client fencing
test. Separate local publication tests kill a process at a rename boundary;
equivalent NAS tests remain required. A competing database open in this probe
uses a second handle in the same process, not an independent NAS client.

All fixture bytes originate on the machine running the native executable. The
reported delta/signature lengths are serialization sizes, not network or SMB
packet counters. Initial executable upload is setup traffic, reported separately.
Read [performance boundaries](performance.md) for pacing and process limits.

Automatic synchronization remains disabled. Passing this probe cannot close the
remaining egress, transport authentication, conflict resolution, directory scheduling,
storage quota, multi-client, recovery or sustained resource gates. Close the
temporary admin SSH session and disable NAS SSH after setup and validation;
administrator SSH is not the automatic application's runtime identity.
