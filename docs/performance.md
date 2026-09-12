# Resource behavior and measurement scope

Private deployment measurements and raw machine logs are not distributed. This
page records implementation bounds and what still needs measurement on a target
system; it does not claim sustained throughput or universal idle-resource results.

## Observation and transport

- Local metadata observation is event-driven, with bounded coalescing, paced scans
  and a configurable watch limit. Startup reconciles metadata after downtime.
- Native NAS observation has its own pacing and private index. Transfer admission,
  file/batch sizes, retained publications and connections are bounded.
- The task CPU governor can temporarily raise a scan/transfer task's allowance,
  then reduces it over time and returns to its idle limit. Service files declare
  separate process memory and scheduling limits.
- LAN policy uses interface, address, route and mount evidence. Healthy idle does
  not require recursive NAS polling; failure paths have bounded retries.

## Desktop and setup

- The indicator consumes a coalesced local event stream and publishes cached menu
  properties. Unchanged metadata does not cause repeated menu redraws.
- Native control panels run separately from the indicator and reuse their windows.
  Their periodic status refresh is active only while visible.
- Folder sizes aggregate index metadata. NAS capacity uses a bounded command on a
  verified mount and a short cache, not recursive content enumeration.
- Traffic accounting uses in-memory counters with periodic active checkpoints and
  clean-shutdown flushing. An abrupt crash may lose the last unflushed interval.
- Explicit setup discovery checks at most 1,024 addresses with sixteen workers;
  QTS product responses are bounded and no credentials are sent during discovery.
- Setup browsing requests one directory at a time and displays at most 1,000
  immediate entries. It does not read file contents or recursively inventory NAS
  data. Configured-root shortcuts come from existing helper metadata.
- Mount units exit after successful mounting. Failed boot mounts retry after a
  delay and recheck direct-LAN eligibility before contacting a share.

## Reproduce measurements

Use the unit/race tests for package invariants, and review the measurement scripts
before running them on a configured environment. Use synthetic data where possible.
Record CPU time, peak/current memory, idle duration, transport bytes, data volume,
hardware/software environment and failure conditions separately. Short functional
tests do not establish sustained-load or reboot acceptance.

Raw captures, screenshots and status records belong in ignored private evidence
storage. Publish only reviewed, non-identifying summaries. Historical fixed-profile
measurement tools require explicit adaptation and opt-in before execution.
