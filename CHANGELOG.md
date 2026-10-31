# Changelog

The changes to Hive that no release contains yet, in the shape of
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- The provisioner tells Qory Apiary how each machine's removal went, with the signed
  `POST /v1/dispatches/<id>/machine-end`. After each delete it sends `deleting` or
  `gone`, and `delete_failed` when the delete fails. It sends `deleting` or
  `delete_failed` once per change, and `gone` once the machine adapter reports that the
  machine no longer exists.
- The provisioner keeps checking a machine after its delete. Every 60 seconds it sends
  the delete again, also after a failed delete, until the machine adapter returns gone.
- The journal of live dispatches has two more phases: `deleting`, for a machine whose
  delete has not returned gone, and `ended`, for a machine that is gone while the post
  of `gone` is pending.
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

- A dispatch keeps its journal entry, and its place in `max_machines`, until the machine
  adapter returns gone. Before, the entry left when the adapter accepted the delete. So
  with `max_machines` of 1, the next run waits at least one more watch of 60 seconds,
  and a delete that keeps failing holds its place while the provisioner retries it.
- When the post of `gone` fails, the machine's entry stays in the phase `ended`, which
  does not count toward `max_machines`. Each watch sends `gone` again with one try, and
  stops at the first failure of that watch. After 24 hours the provisioner logs an error
  and removes the entry, and Qory Apiary then shows no removal of that machine.
- A machine that is deleting, or that is still not listed 5 minutes after its make, gets
  a delete, and `gone` is posted for it. Before, the provisioner removed its entry with
  no delete.
- A listed machine with no journal entry and a key in the shape of a dispatch id gets a
  journal entry, and `machine-end` as any other machine does.
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
