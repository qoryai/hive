# qory-provisioner

This repository builds two programs:

- `qory-provisioner`, a program with one command, `serve`.
- `qory-machines-aws-ec2`, the EC2 machine adapter. The provisioner runs it as
  `qory-machines-aws-ec2 <command> --settings FILE` to make, list, read and delete EC2
  instances, as `contracts/machines/v1` of `github.com/qoryai/integrations` states the
  protocol. FILE is the adapter's settings file, `aws-ec2.json`.

`serve` claims dispatches from Qory Apiary, makes one machine for each through the
machine adapter, and deletes each machine once its dispatch ends. It runs until SIGTERM.
`qory-provisioner serve -h` prints its flags.

## Run one provisioner per node pool

Run one provisioner per node pool. Every 60 seconds, `serve` lists the node pool's
machines and deletes each machine whose dispatch has no entry in the journal in the
provisioner's state folder, with the log line `found a machine with no journal
entry`. Two provisioners of one node pool therefore delete each other's machines,
also machines that run a dispatch.

For a key rotation, a new provisioner with a new access key for the node pool, stop
the old provisioner before you start the new one.

## Instance role policy

The provisioner makes no AWS call. The EC2 machine adapter calls EC2 with the
credentials of the instance role of the host that runs both programs. This IAM policy
contains the actions that the adapter calls, and limits them to the machines of one
node pool. Replace these example values before you attach it to the instance role:

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
  make with these tags and metadata options.
- `LaunchVolume` allows the volumes of a launch only with the same tags, and only
  encrypted. The adapter marks the root volume encrypted.
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

Use Go 1.27.1, the version in `go.mod` and `mise.toml`.

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
`/var/lib/qory-provisioner`, 0700. It sets `AWS_EC2_METADATA_V1_DISABLED=true`. The
image also contains the EC2 machine adapter at `/usr/local/bin/qory-machines-aws-ec2`.

`.github/workflows/prerelease.yml` builds the image for linux/amd64 and linux/arm64
when you push to the branch `prerelease`, or when you run the workflow from the
**Actions** tab once the workflow file is on the default branch. It publishes the
image to `ghcr.io/qoryai/hive` by digest, tags it `sha-` and the commit's first 7
characters, and writes the digest in the run's summary.
