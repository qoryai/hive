# Changelog

Every change to Hive, newest first, in the shape of
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- The machine's start script, `qory-machine-start`, prints one line to standard error at
  the end of each stage, with the time in UTC, such as
  `qory-machine-start: 2026-10-30T14:02:13Z Docker started`. The stages are start data
  read, firewall set, Docker started, run code written and qory run started. No line
  contains a value of the start data.
- When the machine adapter cannot make a machine, the provisioner uploads the adapter's
  error as the dispatch's machine log, which instance admins read, and then releases the
  dispatch. A text that contains the run code, the start data, a run key or an access key
  secret is replaced by a fixed text. The release reason stays the fixed text for each
  kind of failure.

### Changed

- `qory-machine.service` sends standard error to the console as well as the journal
  (`StandardError=journal+console`). So the machine log, which is the vendor's console
  text, contains the start script's lines and the standard error of `qory run`.

### Fixed

- `qory-machine.service` sets `SuccessAction=poweroff` and `FailureAction=poweroff` in
  `[Unit]`. In `[Service]`, systemd ignores both, so a machine whose start script was
  killed before its exit handler ran did not power off when the script ended.
