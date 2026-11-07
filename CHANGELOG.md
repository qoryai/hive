# Changelog

The changes to Hive that no release contains yet, in the shape of
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- The provisioner sends the machine adapter's offers in every claim, and passes the
  claim's `machine_spec` unchanged to `make`. The offers come from the answer of
  `check`, and the provisioner claims nothing before a `check` that succeeds. The
  answer of `make` contains `machine_type`, which the provisioner keeps in the journal
  and sends in each `dispatched` of the dispatch, also in the repeats of each watch
  and after a restart. A machine found by its key after a restart has no type, because
  the answer of `list` contains none. The provisioner releases a claim with no
  `machine_spec`, with the reason "the machine was not made: the claim has no machine
  spec, so the provisioner cannot choose a machine".
- The AWS EC2 adapter chooses the instance type by the machine spec. The answer of
  `check` contains one offer per type in `instance_types`, in that order, each with a
  disk of 16384 GiB. `make` starts the smallest type that fits: by vCPUs, then memory,
  then the order of `instance_types`. For a spec with `virtualization` on a type that
  is not bare metal, it turns on nested virtualization with `CpuOptions`. The root
  volume is gp3, encrypted and deleted with the machine, and its size is the larger of
  `disk_gib` and the size of the image's root volume. When EC2 has no capacity for a
  type in the subnet's zone, `make` tries the next type that fits, and when every type
  fails that way, it returns the last error. The answer of `make` contains the type it
  started.
- The machine's start script writes the steps of the machine's start to
  `/var/lib/qory-machine/steps.jsonl` just before it runs `qory run`: one JSON object
  per line with the step's name and its start and end in UTC to the millisecond, for
  boot, read start data, set firewall, start Docker and write run code. The file
  contains no value of the start data. The script passes the file to `qory run` with
  `--machine-steps-file`. A failed write does not stop the script, and `qory run` then
  starts without the flag.
- The start-data unit for EC2: `qory-machine-data-aws-ec2.service` and its script
  `qory-machine-data-aws-ec2`. It runs before `qory-machine.service`, reads the EC2 user
  data with IMDSv2, and writes it to `/run/qory-machine-start/start-data`. It fails
  when the folder path is a link, is not a folder, or is a folder that another user
  owns, and when a path exists at `start-data` before the write. When it fails, it
  powers the machine off. The provisioner image contains both files in
  `/usr/share/qory-provisioner/machine/`.
- The provisioner tells Qory Apiary how each machine's removal went, with the signed
  `POST /v1/dispatches/<id>/machine-end`. After each delete it sends `deleting` or
  `gone`, and `delete_failed` when the delete fails. It sends `deleting` or
  `delete_failed` once per change, and `gone` once the machine adapter reports that the
  machine no longer exists. After a signed 400, 403 or 404, the provisioner sends no
  more `machine-end` for that dispatch, and its deletes go on.
- The provisioner keeps checking a machine after its delete. Every 60 seconds it sends
  the delete again, also after a failed delete, until the machine adapter returns gone.
- The journal of live dispatches has two more phases: `deleting`, for a machine whose
  delete has not returned gone, and `ended`, for a machine that is gone while the post
  of `gone` is pending.
- The licence: `LICENSE`, the Apache License, Version 2.0; `NOTICE`; and
  `TRADEMARKS.md`, with the rules for using the name Qory.
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

- `aws-ec2.json` has `instance_types`, a list of 1 to 64 EC2 instance types, each
  listed once, in place of `sizes`. Before, `sizes` set each size name of a make
  request, such as `standard`, to an instance type, and the provisioner made the size
  `standard` for a claim with no machine type.
- `check` of the AWS EC2 adapter reads the instance types, the subnet's zone and the
  types that zone offers, and fails with the kind `defect` when a type does not run the
  image's architecture or the zone does not offer it. The error contains the type and
  the region. It sends one `RunInstances` dry run per type, at most 8 at once.
- The error of `InsufficientInstanceCapacity` starts with "Wait, or add a type that
  fits to instance_types in aws-ec2.json". Before, it started with "Wait, or map the
  size to another instance type in aws-ec2.json".
- A line of the journal of live dispatches has a fifth field, the machine type, or `-`
  for none: "dispatch_id phase machine_id max_lifetime_minutes machine_type". `serve`
  reads a line of four fields, which an earlier version wrote, as an entry with no
  machine type.
- The start script, `qory-machine-start`, reads the start data from the file
  `/run/qory-machine-start/start-data`, which a start-data unit for the machine's vendor
  writes, and deletes the file after the read. The script fails when the folder path is
  a link, is not a folder, or is a folder that another user owns. It fails on a symbolic
  link, on a file that is not a regular file, and on a file that root does not own, and
  treats an empty file as missing. The header of `qory-machine.service` and the README
  section "The machine image" describe how to write the file. Before, the script read
  `machine: aws-ec2` from `/home/qory/.config/qory/forager.yaml` and fetched the EC2
  user data itself, and the image needed curl and sed for the script. The lines about
  `forager.yaml` are gone. The two lines about the token and user data requests moved to
  the EC2 unit with the same words. In two lines, "The user data" now reads "The start
  data".
- A dispatch keeps its journal entry, and its place in `max_machines`, until the machine
  adapter returns gone. Before, the entry left when the adapter accepted the delete. So
  with `max_machines` of 1, the next run waits at least one more watch of 60 seconds,
  and a delete that keeps failing holds its place while the provisioner retries it.
- Each `machine-end` post has one try. After the first failed post in a watch, that
  watch sends no more `machine-end`, and its deletes and `dispatched` go on. So a Qory
  Apiary with no `machine-end` route costs one failed post per watch.
- When the post of `gone` fails or waits for the next watch, the machine's entry stays
  in the phase `ended`, which does not count toward `max_machines`. Each watch sends
  `gone` again. After 24 hours the provisioner logs an error and removes the entry.
- A post of `deleting` or `delete_failed` that keeps failing stops 24 hours after its
  first failure. The provisioner logs an error, and a later state goes out as usual.
- A machine that is deleting, or that is still not listed 5 minutes after its make, gets
  a delete, and `gone` is posted for it. Before, the provisioner removed its entry with
  no delete.
- A listed machine with no journal entry and a key in the shape of a dispatch id gets a
  journal entry, and `machine-end` as any other machine does.
- The README describes Hive as vendor-agnostic: Hive reaches each hosting vendor through
  that vendor's machine adapter, by `contracts/machines/v1` of qoryai/integrations, and
  the AWS EC2 adapter is the one machine adapter in this repository. It adds the
  settings of the provisioner, how the provisioner runs the machine adapter, and the
  region and credentials of the AWS EC2 adapter. It moves the settings and the instance
  role policy of the AWS EC2 adapter into one section about that adapter. Before, the
  README was titled `qory-provisioner` and described the EC2 adapter beside the
  provisioner.
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

- In `aws-ec2.json`, replace `sizes` with `instance_types`, a list of the instance
  types that `sizes` had. Without it, `check` fails, and the error starts with the fix.
- Add `ec2:DescribeInstanceTypes`, `ec2:DescribeInstanceTypeOfferings` and
  `ec2:DescribeSubnets` to the `Describe` statement of the instance role policy, as the
  README section "Instance role policy" lists. Without them, `check` fails with an
  error that starts with "Give the instance role the EC2 rights that the EC2 machine
  adapter needs".
- Run this version with a Qory Apiary that sends `machine_spec` in each claim. The
  provisioner releases each claim without one.
- In a machine image for EC2, install and enable `qory-machine-data-aws-ec2.service`
  and its script, as the README section "The machine image" lists. Without the unit,
  the start script prints "Install the start-data unit for this machine's vendor.
  /run/qory-machine-start/start-data does not exist or is empty." and powers the
  machine off.
- Build the machine image with a qory that writes its status and error lines to the
  descriptor in `QORY_STATUS_FD`. With an earlier qory, the console and the machine log
  contain no line of qory run, and qory run's errors are only in the machine's journal,
  which is deleted with the machine.
