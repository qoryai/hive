// Package provisioner has the tests of the files at the repository's root.
package provisioner

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// stage is one stage of the Dockerfile: its FROM line and the instructions after it,
// each a line that is not blank and not a comment.
type stage struct {
	from  string
	lines []string
}

// stages reads the Dockerfile's stages, in order.
func stages(t *testing.T) []stage {
	t.Helper()
	b, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	var all []stage
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "FROM "):
			all = append(all, stage{from: line})
		case len(all) > 0:
			all[len(all)-1].lines = append(all[len(all)-1].lines, line)
		}
	}
	return all
}

// TestDockerfileBuildsTheProvisioner pins the Dockerfile's two stages. The build stage
// runs on the platform of the machine that builds and cross-compiles the program for
// the image's platform, so one build makes the images of linux/amd64 and linux/arm64.
// The image, the last stage, is on the pinned distroless base, runs as 65532:65532,
// keeps its state directory in a volume, turns off IMDSv1 for an AWS SDK, and runs
// qory-provisioner serve.
func TestDockerfileBuildsTheProvisioner(t *testing.T) {
	all := stages(t)
	if len(all) != 2 {
		t.Fatalf("the Dockerfile has %d stages, want 2", len(all))
	}
	build, image := all[0], all[1]
	if !strings.HasPrefix(build.from, "FROM --platform=$BUILDPLATFORM golang:1.27.1-trixie@sha256:") || !strings.HasSuffix(build.from, " AS build") {
		t.Errorf("the first stage is %q, want the build stage on $BUILDPLATFORM", build.from)
	}
	const base = "FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3 AS provisioner"
	if image.from != base {
		t.Errorf("the image is %q, want %q", image.from, base)
	}
	for _, c := range []struct {
		name string
		s    stage
		want []string
	}{
		{"build stage", build, []string{
			"ARG VERSION=dev",
			"ARG TARGETOS",
			"ARG TARGETARCH",
			`RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \`,
			"-o /out/qory-provisioner ./cmd/qory-provisioner",
			"RUN mkdir -p /out/var/lib/qory-provisioner && chmod 0700 /out/var/lib/qory-provisioner",
		}},
		{"image", image, []string{
			"COPY --from=build /out/qory-provisioner /qory-provisioner",
			"COPY --from=build --chown=65532:65532 /out/var/lib/ /var/lib/",
			`VOLUME ["/var/lib/qory-provisioner"]`,
			"ENV AWS_EC2_METADATA_V1_DISABLED=true",
			"USER 65532:65532",
			`ENTRYPOINT ["/qory-provisioner"]`,
			`CMD ["serve"]`,
		}},
	} {
		for _, line := range c.want {
			if !slices.Contains(c.s.lines, line) {
				t.Errorf("the %s has no %s", c.name, line)
			}
		}
	}
	var builds int
	for _, line := range build.lines {
		if strings.Contains(line, "go build") {
			builds++
		}
	}
	if builds != 1 {
		t.Errorf("the build stage has %d builds, want 1", builds)
	}
	if got := image.lines[len(image.lines)-1]; got != `CMD ["serve"]` {
		t.Errorf("the image's last instruction is %q, want CMD [\"serve\"]", got)
	}
}
