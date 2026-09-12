# Publication and privacy

Before publishing the native-sync/control-panel/setup changes, the unpushed commit
was sanitized:

- Raw production evidence, screenshots and directory listings were excluded.
- Personal account names, NAS names, LAN addresses, interfaces, native volume names
  and sample document-tree names were replaced with fictitious examples.
- Detailed deployment histories were replaced with public capability/acceptance
  summaries. Unredacted material was retained outside the repository for private use.
- Credential/key files, Python bytecode and raw evidence are ignored.
- Fixed-machine legacy tools require explicit opt-in before running; their example
  values must be configured and reviewed. The generic setup wizard remains available.

`example-user`, `example-nas`, `10.23.42.0/24`, `enp1s0`, `EXAMPLE_VOLUME` and
`SampleDocuments` are illustrative substitutions, not a description of a deployment.
Private IPv4 ranges are used in tests because the LAN guard intentionally rejects
public documentation networks for automatic synchronization.

The audit checks tracked text and file types for identifying values, credential
material and unexpected binary/runtime files. It is not a formal security audit
or a guarantee that the application has no vulnerabilities. Privileged setup and
NAS writes still require reviewing the selected targets and keeping backups.

The publication cleanup amends only the new, unpushed commit. It does not rewrite
previously published repository history or change an installed application's
configuration, services, credentials or data.
