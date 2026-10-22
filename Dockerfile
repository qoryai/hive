# The container image of qory-provisioner, built from cmd/qory-provisioner. Its command
# is qory-provisioner serve. The image also contains the EC2 machine adapter,
# /usr/local/bin/qory-machines-aws-ec2, built from cmd/qory-machines-aws-ec2.
#
# Build from the repository's root:
#
#   docker build --build-arg VERSION=dev -t qory-provisioner .
#
# Build both platforms at once with docker buildx build --platform
# linux/amd64,linux/arm64. The build stage runs on the building machine's platform, and
# Go cross-compiles the programs, so nothing runs under emulation.
#
# .github/workflows/prerelease.yml publishes the image for both platforms, tags it only
# sha-<the commit's first 7 characters>, and prints the digest in the run's summary.
#
# The programs run as distroless's nonroot user, uid 65532 and gid 65532. The state
# directory /var/lib/qory-provisioner belongs to that user and is 0700.
#
# The base images are pinned by version tag and by digest; distroless publishes no
# version tags, so its tag names the Debian release and the user. Pin each base image by
# its image index digest, not by one platform's image digest: the image is built for
# linux/amd64 and linux/arm64 from these pins.

# The build stage runs on the platform of the machine that builds, BUILDPLATFORM, which
# docker buildx sets, and cross-compiles both programs for the platform of the image,
# TARGETOS and TARGETARCH below.
FROM --platform=$BUILDPLATFORM golang:1.27.1-trixie@sha256:8f58fd67ea075142d947a60e0caa4317746a55118d312f027793d382c7741734 AS build

WORKDIR /src
# go.sum exists only when go.mod requires a module; the pattern matches it when it
# exists.
COPY go.mod go.sum* ./
RUN go mod download
COPY . .

# The build sets the program's version to this value: sha-<the commit's first 7
# characters> for a pre-release image, and dev when none is given.
ARG VERSION=dev
# The platform of the image, which docker buildx sets from --platform, or to the platform
# of the machine that builds when no --platform is given.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/qory-provisioner ./cmd/qory-provisioner
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w" \
      -o /out/qory-machines-aws-ec2 ./cmd/qory-machines-aws-ec2

# The state directory, 0700; it is given to the nonroot user below. /out/var/lib/ is
# copied whole, so the directory keeps its mode.
RUN mkdir -p /out/var/lib/qory-provisioner && chmod 0700 /out/var/lib/qory-provisioner

# The image, the last stage, so the default target.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3 AS provisioner

COPY --from=build /out/qory-provisioner /qory-provisioner
COPY --from=build /out/qory-machines-aws-ec2 /usr/local/bin/qory-machines-aws-ec2

# The start unit of each machine that the provisioner makes. The provisioner does not run
# these files; a machine image installs them as /etc/systemd/system/qory-machine.service
# and /usr/local/libexec/qory/qory-machine-start.
COPY --chmod=0644 machine/qory-machine.service /usr/share/qory-provisioner/machine/qory-machine.service
COPY --chmod=0755 machine/qory-machine-start /usr/share/qory-provisioner/machine/qory-machine-start

# The state directory. A named volume mounted here starts as a copy of this directory,
# with its owner and mode.
COPY --from=build --chown=65532:65532 /out/var/lib/ /var/lib/
VOLUME ["/var/lib/qory-provisioner"]

# With this variable set, an AWS SDK reads the instance metadata only with an IMDSv2
# token, and makes no IMDSv1 request when the token request fails.
ENV AWS_EC2_METADATA_V1_DISABLED=true

USER 65532:65532
ENTRYPOINT ["/qory-provisioner"]
CMD ["serve"]
