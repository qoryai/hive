# Qory Hive

This repository is part of [Qory](https://github.com/qoryai/qory). Its README lists every part and how to install Qory.

Hive makes and deletes the machines of Qory Apiary's node pools. A node pool is a
group of machines in Qory Apiary, and one provisioner serves it. Hive is
vendor-agnostic: it reaches each hosting vendor only through that vendor's machine
adapter, a separate program that makes, lists, reads and deletes machines at one
vendor. Each machine adapter follows the machine adapter contract
[`contracts/machines/v1`](https://github.com/qoryai/integrations/tree/main/contracts/machines/v1)
of [qoryai/integrations](https://github.com/qoryai/integrations). This repository
contains one machine adapter, the AWS EC2 adapter, `qory-machines-aws-ec2`.

This repository builds two programs:

- `qory-provisioner`, the program of Hive. Its one command, `serve`, claims
  dispatches from Qory Apiary (a dispatch is one piece of work that runs on one new
  machine), makes one machine for each through the machine adapter, and deletes each
  machine once its dispatch ends. It runs until SIGTERM or SIGINT.
  `qory-provisioner serve -h` prints its flags.
- `qory-machines-aws-ec2`, the AWS EC2 adapter. See
  [The AWS EC2 adapter](#the-aws-ec2-adapter).

## Settings of the provisioner

`serve` reads the settings file at `--settings`, by default
`/etc/qory-provisioner/settings.json`. It keeps the access key, the record of the
enrolment, the instance id and the journal of live dispatches in the state folder at
`--state-dir`, by default `/var/lib/qory-provisioner`. The enrolment is the step at the
first start that makes the access key and registers it with Qory Apiary as the key of
one node pool; see
[Enrol the access key](#enrol-the-access-key). The settings file is one JSON object:

```json
{
  "apiary_url": "https://apiary.example.com",
  "gateway_address": "10.0.1.5",
  "adapter": {
    "command": "/usr/local/bin/qory-machines-aws-ec2",
    "settings": "/etc/qory-provisioner/aws-ec2.json"
  },
  "enrolment_code_file": "/run/qory-enrolment/provisioner/code"
}
```

- `apiary_url`: the origin of Qory Apiary, with `https`. For a test, an `http` origin
  on `localhost`, `127.0.0.1` or `[::1]` also works. Required.
- `gateway_address`: the IPv4 or IPv6 address of the gateway on the machines'
  network, with no zone and no port, and not an unspecified, multicast or loopback
  address. The provisioner passes it to each machine that it makes. Required.
- `adapter`: the machine adapter. `command` is the path of the adapter's program, and
  `settings` is the path of the adapter's settings file. Required.
- `enrolment_code_file`: the path of the file with the node pool's enrolment code.
  Optional. See [Enrol the access key](#enrol-the-access-key).

The pool's Machine limit in Qory Apiary is the only limit on how many machines exist
at once: the provisioner makes a machine for each dispatch that it claims.

Write each path as an absolute path of at most 4096 bytes, with no `.` or `..`
element, no double slash, no trailing slash, and no NUL byte. Keep secrets out of
the settings file: when it contains an access key secret, a run key, a run code or an
enrolment code, `serve` exits with status 1, and the error starts with the fix.

## How the provisioner runs the machine adapter

The provisioner makes no call to a hosting vendor. It runs the machine adapter as
`<command> <name> --settings <file>`, where `<command>` is `adapter.command`,
`<name>` is `check`, `make`, `list`, `log` or `delete`, and `<file>` is
`adapter.settings`. It writes each request to the adapter's standard input and reads
the answer from its standard output, in the documents of `contracts/machines/v1`. When
`serve` starts, it runs `check` before it claims a dispatch, and it exits with status 1
when `check` fails.

The answer of `check` contains offers: the machines that the adapter can make, each
with its vCPUs, memory, disk, architecture and whether it runs virtual machines.
`serve` sends these offers in every claim. Each claimed dispatch has a machine spec,
the least machine that its run needs, and `serve` passes the spec unchanged to `make`.
`serve` releases a claimed dispatch that has no machine spec, with the reason "the
machine was not made: the claim has no machine spec, so the provisioner cannot choose
a machine". The answer of `make` contains the machine's type, such as `m7i.large`.
`serve` keeps the type in the journal and sends it in each `dispatched` of the
dispatch. A machine that `serve` finds by its key after a restart has no type, because
the answer of `list` contains none, and its `dispatched` contains no type.

The provisioner copies the adapter's standard error, line by line, to the
provisioner's standard error, which also has the provisioner's log. The provisioner
cuts each line at 4096 bytes. It replaces a line with a fixed text when the line
contains what looks like a run code, or a piece of the start data that the provisioner
passes to a machine. The adapter runs with the provisioner's environment,
without any variable whose name starts with `QORY_`. The provisioner kills the adapter
when `make` runs longer than 2 minutes, or another command longer than 1 minute.

## Run one provisioner per node pool

Run one provisioner per node pool. Every 60 seconds, `serve` lists the node pool's
machines and deletes each machine whose dispatch has no entry in the journal in the
provisioner's state folder, with the log line `found a machine with no journal
entry`. Two provisioners of one node pool therefore delete each other's machines,
also machines that run a dispatch.

When you rotate the key, which means a new provisioner with a new access key for the
node pool, stop the old provisioner before you start the new one. A new provisioner
with a new state folder starts with an empty journal, so it deletes the machines that
the old one made, also those that run a dispatch.

## Enrol the access key

On the first start, while the state folder has no access key, `serve` enrols one with
the node pool's enrolment code (`qecp_…`). Pass the code in one of two ways:

- `enrolment_code_file` in the settings file: the absolute path of the file that Qory
  Apiary writes for a node pool installed with it, such as
  `/run/qory-enrolment/provisioner/code`. The member is optional.
- `QORY_ENROLMENT_CODE` in the environment: a code from the node pool's **Access key**
  tab in Qory Apiary.

Set only one of the two. With both set, `serve` exits with status 1, and the error
starts with "Remove QORY_ENROLMENT_CODE from the environment or enrolment_code_file
from the settings file".

While the file is missing, `serve` logs one line that starts with `the enrolment code
file is missing` and reads the file again every 10 seconds. It reads at most 79 bytes
and exits with status 1 on a file longer than 78 bytes, the longest code and a
newline. It removes one trailing newline. When the code is for another kind of node,
`serve` exits with status 1 before it sends a request.

Before `serve` reads the file, it follows the path one link at a time, and checks the
file, each link on the way, and each folder above them:

1. Root or the user that `serve` runs as, uid 65532 in the image, owns each of them.
2. Other users may not write the file or the folders. A folder that root owns with the
   sticky bit set, such as `/tmp`, passes.
3. A group may write them only when it is gid 0, `wheel`, `admin`, or the owner's
   primary group with the owner's name.

Use a regular file, not a folder or a named pipe, and a path through at most 40 links.
If a check fails, `serve` exits with status 1, and the error starts with the fix and
contains the path. An error about an owner or a write permission also contains the
owner's uid.

When the state folder has an access key, `serve` reads neither the file nor the
variable, and logs one line that the one that is set is not used.

## The AWS EC2 adapter

`qory-machines-aws-ec2` makes, lists and deletes EC2 instances, and reads their
console output as the machine log. The image contains it at
`/usr/local/bin/qory-machines-aws-ec2`. To use it, set `adapter.command` to that path,
and `adapter.settings` to the path of its settings file, `aws-ec2.json`.

### Region and credentials

The adapter reads the region from `AWS_REGION`, or else from the instance metadata of
the EC2 instance that it runs on. `AWS_DEFAULT_REGION` has no effect. The adapter uses
only the credentials of the host's instance role, which it reads from the instance
metadata service, and calls EC2 only at the region's standard endpoint. It reads no
AWS config or credentials file.

Each command fails when the environment sets one of these variables to a value that is
not empty: a variable that selects other credentials, a profile, a config or
credentials file, another metadata address, another endpoint, or another certificate
authority, or that turns off the instance metadata. Examples are `AWS_ACCESS_KEY_ID`,
`AWS_PROFILE`, `AWS_EC2_METADATA_DISABLED` and `AWS_ENDPOINT_URL`. The error starts
with `Unset` and the variable's name.

### Settings

`qory-machines-aws-ec2 check` and `make` read the settings file `aws-ec2.json`, one
JSON object:

```json
{
  "subnet_id": "subnet-0123456789abcdef0",
  "security_group_id": "sg-0123456789abcdef0",
  "image_id": "ami-0123456789abcdef0",
  "tags": {"qory:stack": "example-stack"}
}
```

- `subnet_id` and `security_group_id`: the run subnet and the security group of each
  machine's network interface. Required.
- `image_id`: the image that each machine starts from. Required.
- `tags`: at most 20 entries, each a tag key set to its value. Optional. Each machine
  and its volume get these tags after `qory:managed`, `qory:pool` and `qory:key`. The
  dry run of `check` sends them too.

A tag key has 1 to 128 characters, and a tag value 0 to 256 characters, of letters,
digits, spaces and `_ . : / = + - @`. A key may not start with `aws:`, and may not be
`qory:managed`, `qory:pool` or `qory:key`, in any case. EC2 accepts at most 50 tags on
a resource: the 20 entries and the adapter's 3 tags stay within that limit. When a
member breaks a rule, `check` and `make` fail, and the error starts with the fix. A
file with another member, such as `instance_types`, fails with the error "Remove every
member other than subnet_id, security_group_id, image_id and tags from aws-ec2.json."

### Instance types

The adapter has no list of instance types. `check` and `make` read the architecture
of `image_id` with `ec2:DescribeImages`, the zone of `subnet_id` with
`ec2:DescribeSubnets`, the instance types of the region with
`ec2:DescribeInstanceTypes`, and the types that the zone offers with
`ec2:DescribeInstanceTypeOfferings`. They use only the types that the zone offers,
that are sold on demand, and that run the image's architecture. With an x86_64 image,
a machine spec for arm64 gets no offer and no machine; with an arm64 image, arm64
types work the same way.

The answer of `check` contains the offers that no other offer covers. One offer covers
another when it has at least its vCPUs and memory, the same architecture, and
virtualization when the other has it. Qory Apiary uses offers only to test whether
any offer fits a run's machine type, so a covered offer adds no machine type. Each
offer has a disk of 16384 GiB, the largest gp3 volume, because `make` sizes the root
volume to the machine spec. vCPUs above 1024 and memory above 8388608 MiB are cut to
those values, the most that an offer may have. `check` fails with the kind `defect`
when the zone offers no type for the image's architecture. It sends at most two
`ec2:RunInstances` dry runs: one for the first type that `make` would start, and one
with `CpuOptions` for the first type that needs them for nested virtualization.

`make` starts the first type that fits the machine spec: the type with the fewest
vCPUs, then the least memory, then the lowest on-demand Linux price in the region,
then the first by name. A type with no price comes after the types of its size that
have one. A type fits when it has at least the spec's vCPUs and memory, runs the
spec's architecture, and, when the spec sets `virtualization`, is a bare metal type or
has nested virtualization. When the spec's `disk_gib` is larger than 16384, the
largest gp3 volume, `make` fails with the kind `defect` before any EC2 call. For
virtualization on a type that is not bare metal, `make` turns on nested virtualization
with `CpuOptions`. The root volume is gp3 and encrypted, is deleted with the machine,
and its size is the larger of the spec's `disk_gib` and the size of the root volume of
`image_id`. When EC2 has no capacity for a type in the subnet's zone
(`InsufficientInstanceCapacity`), `make` tries the next type that fits, at most 8
types, and then returns the last error. When no type fits, `make` fails with the kind
`defect`. A key gets one machine: each start sends a client token made of the key and
the type, and before the first start, `make` looks for a machine with the key's tags.
EC2 lists a new machine after a short delay, so a second `make` with the same key
right after the first can start a second machine of another type.

### Prices

The adapter reads the prices with `pricing:GetProducts` of the AWS Price List Query
API, at `https://api.pricing.us-east-1.amazonaws.com`, whatever the region of the
machines: the API runs only in a few regions, and us-east-1 returns the prices of
every region. It reads the hourly on-demand price in USD for the region, Linux,
shared tenancy, no software, and capacity status `Used`, for the product families
`Compute Instance` and `Compute Instance (bare metal)`. One lookup has a time limit of
20 seconds. When the lookup fails, `check` and `make` go on: they log one line that
starts with "Give the instance role pricing:GetProducts", and use the prices of the
cache, or no prices. The cache file records the time of the failed lookup, and
`check` and `make` try no lookup until 1 hour after it.

### Cache

The adapter keeps the instance types, the types of each zone, and the prices in one
JSON file per region, `qory-machines-aws-ec2/<region>.json`, in the folder that Go's
`os.UserCacheDir` returns: `$XDG_CACHE_HOME`, or else `$HOME/.cache`. In the image,
which runs as the user `nonroot`, that is
`/home/nonroot/.cache/qory-machines-aws-ec2/<region>.json`. The folder has mode 0700
and the file 0600.

- Each of the three parts is read again from AWS when it is 24 hours old. The types of
  the region are also read again when the zone offers a type that the file lacks.
- When a read fails, the adapter uses the part in the file and logs one line. A failed
  read of the types or of the zone's types with no part in the file fails the command.
- When `HOME` and `XDG_CACHE_HOME` are both unset, or the folder cannot be written,
  `check` and `make` still work: they log one line, and read the types and prices from
  AWS on every call. Give the adapter a writable home folder, or set
  `XDG_CACHE_HOME` to a writable folder, to keep the file between calls.

### Instance role policy

The AWS EC2 adapter calls EC2 with the credentials of the instance role of the host
that runs the provisioner and the adapter. This IAM policy contains the actions that
the adapter calls. It limits launches to instances tagged `qory:managed=true`, and
limits deletes and console reads to the machines of one node pool. Replace these
example values before you attach it to the instance role:

- `<region>` and `<account-id>`: the region and the account of the machines.
- `subnet-0123456789abcdef0`, `sg-0123456789abcdef0` and `ami-0123456789abcdef0`:
  `subnet_id`, `security_group_id` and `image_id` in `aws-ec2.json`.
- `np_0123456789abcdef`: the node pool's id, the value of each machine's `qory:pool`
  tag.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "LaunchInstance",
      "Effect": "Allow",
      "Action": "ec2:RunInstances",
      "Resource": "arn:aws:ec2:<region>:<account-id>:instance/*",
      "Condition": {
        "StringEquals": {
          "aws:RequestTag/qory:managed": "true",
          "ec2:MetadataHttpTokens": "required"
        },
        "ForAllValues:StringEquals": {
          "aws:TagKeys": ["qory:managed", "qory:pool", "qory:key"]
        },
        "NumericLessThanEquals": {
          "ec2:MetadataHttpPutResponseHopLimit": "1"
        }
      }
    },
    {
      "Sid": "LaunchVolume",
      "Effect": "Allow",
      "Action": "ec2:RunInstances",
      "Resource": "arn:aws:ec2:<region>:<account-id>:volume/*",
      "Condition": {
        "StringEquals": {
          "aws:RequestTag/qory:managed": "true"
        },
        "ForAllValues:StringEquals": {
          "aws:TagKeys": ["qory:managed", "qory:pool", "qory:key"]
        },
        "Bool": {
          "ec2:Encrypted": "true"
        }
      }
    },
    {
      "Sid": "LaunchNetworkInterface",
      "Effect": "Allow",
      "Action": "ec2:RunInstances",
      "Resource": "arn:aws:ec2:<region>:<account-id>:network-interface/*",
      "Condition": {
        "Bool": {
          "ec2:AssociatePublicIpAddress": "false"
        }
      }
    },
    {
      "Sid": "LaunchFromSettings",
      "Effect": "Allow",
      "Action": "ec2:RunInstances",
      "Resource": [
        "arn:aws:ec2:<region>:<account-id>:subnet/subnet-0123456789abcdef0",
        "arn:aws:ec2:<region>:<account-id>:security-group/sg-0123456789abcdef0",
        "arn:aws:ec2:<region>::image/ami-0123456789abcdef0"
      ]
    },
    {
      "Sid": "TagAtLaunch",
      "Effect": "Allow",
      "Action": "ec2:CreateTags",
      "Resource": [
        "arn:aws:ec2:<region>:<account-id>:instance/*",
        "arn:aws:ec2:<region>:<account-id>:volume/*"
      ],
      "Condition": {
        "StringEquals": {
          "ec2:CreateAction": "RunInstances"
        }
      }
    },
    {
      "Sid": "Describe",
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeInstances",
        "ec2:DescribeImages",
        "ec2:DescribeInstanceTypes",
        "ec2:DescribeInstanceTypeOfferings",
        "ec2:DescribeSubnets"
      ],
      "Resource": "*"
    },
    {
      "Sid": "ReadPrices",
      "Effect": "Allow",
      "Action": "pricing:GetProducts",
      "Resource": "*"
    },
    {
      "Sid": "TerminateAndReadConsole",
      "Effect": "Allow",
      "Action": ["ec2:TerminateInstances", "ec2:GetConsoleOutput"],
      "Resource": "arn:aws:ec2:<region>:<account-id>:instance/*",
      "Condition": {
        "StringEquals": {
          "ec2:ResourceTag/qory:managed": "true",
          "ec2:ResourceTag/qory:pool": "np_0123456789abcdef"
        }
      }
    }
  ]
}
```

What each statement allows, and why:

- `LaunchInstance` allows `ec2:RunInstances` to make an instance only with the tag
  `qory:managed` set to `true`, no tag keys other than `qory:managed`, `qory:pool` and
  `qory:key`, IMDSv2 required, and a metadata hop limit of 1. The adapter sends each
  make with these tags and metadata options. If `aws-ec2.json` has `tags`, add each
  key of `tags` to `aws:TagKeys`. Without those keys, `check` fails with
  `UnauthorizedOperation`.
- `LaunchVolume` allows the volumes of a launch only with the same tags, and only
  encrypted. The adapter marks the root volume encrypted. Add the keys of `tags` to
  its `aws:TagKeys` too.
- `LaunchNetworkInterface` allows the network interface of a launch only without a
  public IP address. The adapter sends `AssociatePublicIpAddress` set to false.
- `LaunchFromSettings` allows a launch only into the subnet and the security group of
  `aws-ec2.json`, and only from its image.
- `TagAtLaunch` allows `ec2:CreateTags` only inside `ec2:RunInstances`, which tags the
  instance and its volume as it makes them. The adapter tags nothing after a launch.
- `Describe` allows `ec2:DescribeInstances`, which lists the node pool's machines and
  finds a machine by its key; `ec2:DescribeImages`, which reads the image's root
  device, the size of its root volume and its architecture;
  `ec2:DescribeInstanceTypes`, which reads the vCPUs, memory, architectures,
  virtualization and usage classes of every type in the region;
  `ec2:DescribeSubnets`, which reads the zone of `subnet_id`; and
  `ec2:DescribeInstanceTypeOfferings`, which reads the types that zone offers. None of
  these actions accepts a limit to a resource, so the resource is `*`.
- `ReadPrices` allows `pricing:GetProducts`, which reads the on-demand prices of the
  instance types. The action accepts no limit to a resource, so the resource is `*`.
- `TerminateAndReadConsole` allows `ec2:TerminateInstances` and
  `ec2:GetConsoleOutput` only on instances with the tags `qory:managed` set to `true`
  and `qory:pool` set to the node pool's id. The adapter deletes machines and reads
  their logs with these actions.

`qory-machines-aws-ec2 check` sends `ec2:RunInstances`, at most twice, and
`ec2:DescribeInstances` as dry runs, and a dry run needs the same permissions as the
call. The policy has no condition on `ec2:InstanceType`: if you add one, allow every
type that a machine spec of your machine types may fit, or `check` or `make` fails
with `UnauthorizedOperation`. The dry run of
`ec2:RunInstances` sends the placeholder pool id `np_0000000000000000` as the
`qory:pool` tag. Keep the `RunInstances` statements free of a condition on the value
of `qory:pool`: if they require the node pool's id, `check` fails with
`UnauthorizedOperation`.

The policy contains no `iam:PassRole`, so no machine gets an instance role.

When the instance role lacks an EC2 permission, the adapter's error starts with "Give
the instance role the EC2 rights that the EC2 machine adapter needs" and contains the
action that failed.

The adapter calls these endpoints over HTTPS: `ec2.<region>.amazonaws.com` and
`api.pricing.us-east-1.amazonaws.com`. It also reads the instance metadata service at
`169.254.169.254`, or `[fd00:ec2::254]` with IPv6.

If the account's default EBS encryption key is a customer managed KMS key, also allow
`kms:CreateGrant`, `kms:GenerateDataKeyWithoutPlaintext` and `kms:Decrypt` on that
key. Without them, the launch of the encrypted root volume fails, and the machine
does not start.

```json
{
  "Sid": "UseEbsKey",
  "Effect": "Allow",
  "Action": ["kms:CreateGrant", "kms:GenerateDataKeyWithoutPlaintext", "kms:Decrypt"],
  "Resource": "arn:aws:kms:<region>:<account-id>:key/<key-id>"
}
```

## Build

Use Go 1.27.3, the version in `go.mod` and `mise.toml`.

```
go build -o qory-provisioner ./cmd/qory-provisioner
go build -o qory-machines-aws-ec2 ./cmd/qory-machines-aws-ec2
```

## Run the checks

Run these from the repository's root. CI runs the same checks, with `go test` and
without `-race`.

```
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -race ./...
go run github.com/mgechev/revive@v1.16.0 -config revive.toml -set_exit_status ./...
```

## Image

`Dockerfile` builds the image:

```
docker build --build-arg VERSION=dev -t qory-provisioner .
```

The image runs `qory-provisioner serve` as uid 65532 and gid 65532, with the volume
`/var/lib/qory-provisioner`, 0700. The image also contains the AWS EC2 adapter at
`/usr/local/bin/qory-machines-aws-ec2`, and sets `AWS_EC2_METADATA_V1_DISABLED=true`,
so that the AWS SDK reads the instance metadata only with an IMDSv2 token.

`.github/workflows/prerelease.yml` builds the image for linux/amd64 and linux/arm64
when you push to the branch `prerelease`, or when you run the workflow from the
**Actions** tab once the workflow file is on the default branch. It publishes the
image to `ghcr.io/qoryai/hive` by digest, tags it `sha-` and the commit's first 7
characters, and writes the digest in the run's summary.

## The machine image

Each machine that the provisioner makes boots from a machine image. The image needs two
systemd units. Their files are in `machine/` in this repository, and in
`/usr/share/qory-provisioner/machine/` in the provisioner image.

1. Install the start unit, which runs qory and powers the machine off:
   - `qory-machine.service` as `/etc/systemd/system/qory-machine.service`, mode 0644;
   - `qory-machine-start` as `/usr/local/libexec/qory/qory-machine-start`, mode 0755.
2. Install the start-data unit for the image's vendor. On EC2, it reads the start data
   from the user data:
   - `qory-machine-data-aws-ec2.service` as
     `/etc/systemd/system/qory-machine-data-aws-ec2.service`, mode 0644;
   - `qory-machine-data-aws-ec2` as `/usr/local/libexec/qory/qory-machine-data-aws-ec2`,
     mode 0755.
3. Enable both units:

   ```
   systemctl enable qory-machine.service qory-machine-data-aws-ec2.service
   ```

The start unit reads the start data from `/run/qory-machine-start/start-data`. A
start-data unit, a unit of the instance admin, cloud-init, or a step on a bare host can
write the file. Each writer follows this rule:

- Make the folder `/run/qory-machine-start` as root, mode 0700.
- Write the start data to a temporary file in that folder, mode 0600, then rename the
  file to `start-data`. On failure, leave no file.
- Finish before `qory-machine.service` starts. A systemd unit sets
  `Before=qory-machine.service`.

The start unit fails when the folder path is a link, is not a folder, or is a folder
that another user owns. It fails on a symbolic link, on a file that is not a regular
file, and on a file that root does not own. When the file is missing or empty, it prints
"Install the start-data unit for this machine's vendor.
/run/qory-machine-start/start-data does not exist or is empty." It deletes the file
after the read. A failed start-data unit for EC2 powers the machine off.

Just before `qory run`, the start unit writes the steps of the machine's start to
`/var/lib/qory-machine/steps.jsonl`, owner root, mode 0644, and passes the file to
`qory run` with `--machine-steps-file`. Each line is one JSON object, in the order the
steps ran, with times in UTC to the millisecond:

```
{"name":"start Docker","started":"2026-11-06T00:31:44.120Z","ended":"2026-11-06T00:31:58.904Z"}
```

The steps are `boot`, `read start data`, `set firewall`, `start Docker` and
`write run code`. `boot` runs from the kernel's start to the start of the script. Each
other step runs from the end of the step before it, or from the start of the script
when there is no boot line, to its stage line. The times come from `/proc/uptime`, so a
change of the clock during the start does not reorder the steps. When `/proc/uptime`
cannot be read or has no number, the times come from the clock, and the file has no
`boot` line. The file contains no value of the start data.

The start unit first removes any `steps.jsonl`, so a file from the machine image never
stays. It writes the file only when `/var/lib/qory-machine` is a folder, not a link,
that root owns and that no group or other user can write. When the write fails or is
skipped, the start unit prints one line to the console that starts with the fix, leaves
no `steps.jsonl`, and runs `qory run` without `--machine-steps-file`. When
`/var/lib/qory-machine` is a link, the start unit removes and writes no `steps.jsonl`
in the folder that it links to.

## Licence

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE). *Qory* is a trademark of
8wonders GmbH; see [TRADEMARKS.md](TRADEMARKS.md).
