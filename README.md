# Hive

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
enrolment, and the journal of live dispatches in the state folder at `--state-dir`, by
default `/var/lib/qory-provisioner`. The enrolment is the step at the first start that
makes the access key and registers it with Qory Apiary as the key of one node pool; see
[Enrol the access key](#enrol-the-access-key). The settings file is one JSON object:

```json
{
  "apiary_url": "https://apiary.example.com",
  "gateway_address": "10.0.1.5",
  "max_machines": 2,
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
- `max_machines`: the most machines that exist at once, 1 to 100. Optional, 1 when
  you leave it out.
- `adapter`: the machine adapter. `command` is the path of the adapter's program, and
  `settings` is the path of the adapter's settings file. Required.
- `enrolment_code_file`: the path of the file with the node pool's enrolment code.
  Optional. See [Enrol the access key](#enrol-the-access-key).

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

The provisioner copies the adapter's standard error, line by line, to its standard
error, where it writes its log. It cuts a line at 4096 bytes, and it replaces a line
that contains what looks like a run code, or a piece of the start data that it passes
to a machine, with a fixed text. The adapter runs with the provisioner's environment,
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
and stops on a file longer than 78 bytes, the longest code and a newline. It removes
one trailing newline. A code for another kind of node stops `serve` before it sends a
request.

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
AWS config or credentials file. When the environment sets, to a value that is not
empty, a variable that selects other credentials, a profile or a config or credentials
file, another metadata address, another endpoint or another certificate authority, or
that turns off the instance metadata, such as `AWS_ACCESS_KEY_ID`, `AWS_PROFILE`,
`AWS_EC2_METADATA_DISABLED` or `AWS_ENDPOINT_URL`, each command fails, and the error
starts with `Unset` and the variable's name.

### Settings

`qory-machines-aws-ec2 check` and `make` read the settings file `aws-ec2.json`, one
JSON object:

```json
{
  "subnet_id": "subnet-0123456789abcdef0",
  "security_group_id": "sg-0123456789abcdef0",
  "image_id": "ami-0123456789abcdef0",
  "sizes": {"standard": "t3.medium", "large": "m7g.large"},
  "tags": {"qory:stack": "example-stack"}
}
```

- `subnet_id` and `security_group_id`: the run subnet and the security group of each
  machine's network interface. Required.
- `image_id`: the image that each machine starts from. Required.
- `sizes`: 1 to 20 entries, each a size name of a make request set to an EC2 instance
  type. Required.
- `tags`: at most 20 entries, each a tag key set to its value. Optional. Each machine
  and its volume get these tags after `qory:managed`, `qory:pool` and `qory:key`. The
  dry run of `check` sends them too.

A tag key has 1 to 128 characters, and a tag value 0 to 256 characters, of letters,
digits, spaces and `_ . : / = + - @`. A key may not start with `aws:`, and may not be
`qory:managed`, `qory:pool` or `qory:key`, in any case. EC2 accepts at most 50 tags on
a resource: the 20 entries and the adapter's 3 tags stay within that limit. When a
member breaks a rule, `check` and `make` fail, and the error starts with the fix.

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
      "Action": ["ec2:DescribeInstances", "ec2:DescribeImages"],
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
  finds a machine by its key, and `ec2:DescribeImages`, which reads the image's root
  device name. Neither action accepts a limit to a resource, so the resource is `*`.
- `TerminateAndReadConsole` allows `ec2:TerminateInstances` and
  `ec2:GetConsoleOutput` only on instances with the tags `qory:managed` set to `true`
  and `qory:pool` set to the node pool's id. The adapter deletes machines and reads
  their logs with these actions.

`qory-machines-aws-ec2 check` sends `ec2:RunInstances` and `ec2:DescribeInstances`
as dry runs, and a dry run needs the same permissions as the call. The dry run of
`ec2:RunInstances` sends the placeholder pool id `np_0000000000000000` as the
`qory:pool` tag. Keep the `RunInstances` statements free of a condition on the value
of `qory:pool`: if they require the node pool's id, `check` fails with
`UnauthorizedOperation`.

The policy contains no `iam:PassRole`, so no machine gets an instance role.

When the instance role lacks a permission, the adapter's error starts with "Give the
instance role the EC2 rights that the EC2 machine adapter needs" and contains the
action that failed.

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

## Licence

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE). *Qory* is a trademark of
8wonders GmbH; see [TRADEMARKS.md](TRADEMARKS.md).
