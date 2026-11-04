# Changelog

The changes to Hive that no release contains yet, in the shape of
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- The licence: `LICENSE`, the Apache License, Version 2.0; `NOTICE`; and
  `TRADEMARKS.md`, which states what you may do with the name Qory.
- The machine's start script, `qory-machine-start`, prints one line to standard error at
  each stage, with the time in UTC, such as
  `qory-machine-start: 2026-10-30T14:02:13Z Docker started`. It prints start data read,
  firewall set, Docker started and run code written as each of those stages ends, and
  qory run started just before it runs `qory run`. No line contains a value of the start
  data. A line that cannot be written does not stop the script.
- When the machine adapter cannot make a machine, the provisioner releases the dispatch.
  When the release returns 204 or 409 lease_expired, the provisioner then uploads the
  adapter's error as the dispatch's machine log, which instance admins read. After any
  other answer, such as 409 machine_exists, 410 gone or a failed request, a machine may
  exist, so the provisioner uploads no error and keeps the machine log for that
  machine's console text. A text that contains the run code, the start data, a run key
  or an access key secret is replaced by a fixed text. The release reason stays the
  fixed text for each kind of failure.

### Changed

- The README describes Hive as vendor-agnostic: Hive reaches each hosting vendor through
  that vendor's machine adapter, by `contracts/machines/v1` of qoryai/integrations, and
  the AWS EC2 adapter is the one machine adapter today. It adds the settings of the
  provisioner and how the provisioner runs the machine adapter, and it moves the region,
  credentials, settings and instance role policy of the AWS EC2 adapter into one section
  about that adapter. Before, the README was titled `qory-provisioner` and described the
  EC2 adapter beside the provisioner.
- `qory-machine.service` sends standard error to the console as well as the journal
  (`StandardError=journal+console`). The start script sets `QORY_STATUS_FD=3` for qory
  run, with descriptor 3 on that standard error, and sends qory run's stdout and stderr
  to the journal only. So the machine log, which is the vendor's console text, contains
  the start script's lines and qory run's status and error lines, and the output of the
  agent, the setup and the prebuild stays in the journal. Before, the start script's
  lines and all of qory run's output reached the journal only.
- The line that the start script prints when `qory run` exits with an error points to
  the machine log. Before, it read "Read the lines of qory run in the journal of
  qory-machine.service for the cause", and that journal is gone once the machine is
  deleted. After, it reads "Read the status lines of qory run above this line in the
  machine log for the cause".
- When `docker.service` fails to start, the start script prints the last 20 lines of
  its journal from this boot before its failure line, so the machine log contains
  them. In those lines, the engine proxy password is replaced by `[hidden]`, and a line
  that contains `qak_`, `qrk_` or `qecr_` is left out. Before, the failure line read
  "Read the journal of docker.service for the cause", and that journal is gone once the
  machine is deleted. After, it reads "Read the lines of docker.service above this line
  in the machine log for the cause".

### Fixed

- `qory-machine.service` sets `SuccessAction=poweroff` and `FailureAction=poweroff` in
  `[Unit]`. In `[Service]`, systemd ignores both, so a machine whose start script was
  killed before its exit handler ran did not power off when the script ended.

### Upgrading

- Build the machine image with a qory that writes its status and error lines to the
  descriptor in `QORY_STATUS_FD`. With an earlier qory, the console and the machine log
  contain no line of qory run, and qory run's errors are only in the machine's journal,
  which is deleted with the machine.
