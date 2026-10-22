# qory-provisioner

This repository builds two programs:

- `qory-provisioner`, a program with one command, `serve`.
- `qory-machines-aws-ec2`, the EC2 machine adapter. The provisioner runs it as
  `qory-machines-aws-ec2 <command> --settings FILE` to make, list, read and delete EC2
  instances, as `contracts/machines/v1` of `github.com/qoryai/integrations` states the
  protocol. FILE is the adapter's settings file, `aws-ec2.json`.

`serve` is not built in this commit: it prints one error line on standard error and
exits with status 2.

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
