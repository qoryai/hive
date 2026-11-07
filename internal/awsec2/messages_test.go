package awsec2

import (
	"context"
	"io/fs"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/hive/internal/machine"
	"github.com/qoryai/integrations/contracts"
)

// wantFits fails t unless message has at most adapterio.MaxMessage characters.
func wantFits(t *testing.T, message string) {
	t.Helper()
	if n := utf8.RuneCountInString(message); n > adapterio.MaxMessage {
		t.Errorf("%d characters, more than %d: %s", n, adapterio.MaxMessage, message)
	}
}

// TestMessagesFit checks that each fixed message of this package, with typical values,
// fits in adapterio.MaxMessage: a settings path of 60 characters, which comes last, an
// instance type of 40 characters, and the largest values of a machine spec. Package
// machine tests its messages in messages_test.go.
func TestMessagesFit(t *testing.T) {
	const ok = `"subnet_id": "subnet-0123456789abcdef0", "security_group_id": "sg-0123456789abcdef0", "image_id": "ami-0123456789abcdef0"`
	for _, doc := range []string{
		``,
		`[]`,
		`{"subnet_id": "subnet-01234567",}`,
		`{"subnet_id": "subnet-01234567", "subnet_id": "subnet-01234567"}`,
		`{"other": 1}`,
		`{}`,
		`{"subnet_id": "x", "security_group_id": "x", "image_id": "x", "instance_types": []}`,
		`{"subnet_id": "subnet-0123456789abcdef0", "security_group_id": "x", "image_id": "x", "instance_types": []}`,
		`{"subnet_id": "subnet-0123456789abcdef0", "security_group_id": "sg-0123456789abcdef0", "image_id": "x", "instance_types": []}`,
		`{` + ok + `, "instance_types": []}`,
		`{` + ok + `, "instance_types": ["t3.medium", "t3.medium"]}`,
		withTags(`[]`),
		withTags(`{"!": ""}`),
		withTags(`{"aws:a": ""}`),
		withTags(`{"qory:key": ""}`),
	} {
		_, err := ParseSettings([]byte(doc))
		if err == nil {
			t.Fatalf("%q: no error", doc)
		}
		wantFits(t, err.Error())
	}

	_, err := ReadSettings(writeSettings(t, strings.Repeat(" ", maxFile+1)))
	wantFits(t, err.Error())

	path := "/etc/qory/provisioner/adapters/machine/settings/aws-ec2.json"
	if len(path) != 60 {
		t.Fatalf("the path has %d characters, want 60", len(path))
	}
	for _, e := range []struct {
		op  string
		err error
	}{
		{"opening", &fs.PathError{Op: "open", Path: path, Err: syscall.ENOENT}},
		{"opening", &fs.PathError{Op: "open", Path: path, Err: syscall.EACCES}},
		{"reading", &fs.PathError{Op: "read", Path: path, Err: syscall.EISDIR}},
	} {
		m := fileError(e.err, e.op).Error()
		if !strings.HasSuffix(m, ": "+path) {
			t.Errorf("the message does not end with the path: %s", m)
		}
		wantFits(t, m)
	}

	// The defects of check and make, with a type of 40 characters and a long region.
	long := "c7i-" + strings.Repeat("x", 21) + ".metal-48xlarge"
	defer delete(catalog, long)
	region := func(f *fakeAPI) *Handler {
		h, _, _ := testHandler(f)
		h.client = func(context.Context) (*machine.Client, error) { return machine.NewInRegion(f, "ap-southeast-4"), nil }
		return h
	}
	for _, tc := range []struct {
		arch       types.ArchitectureType
		vcpus      int32
		notOffered []string
	}{
		{"arm64", 2, nil},
		{"x86_64", 2, []string{long}},
		{"x86_64", 2048, nil},
	} {
		catalog[long] = typeInfo(long, tc.vcpus, 8388608, true, tc.arch)
		_, err = region(world(tc.notOffered...)).Check(context.Background(), writeSettings(t, withTypes(long)), contracts.MachinesCheck{Version: 1})
		if err == nil || !strings.Contains(err.Error(), long) || !strings.Contains(err.Error(), "ap-southeast-4") {
			t.Fatalf("check: error %v, want one with the type and the region", err)
		}
		wantFits(t, err.Error())
	}
	spec := contracts.MachineSpec{VCPUs: 1024, MemoryMiB: 8388608, DiskGiB: 65536, Arch: "arm64", Virtualization: true}
	_, err = region(world()).Make(context.Background(), writeSettings(t, validSettings), contracts.MachinesMake{Version: 1, Key: testKey, Pool: testPool, MachineSpec: spec}, []byte("x"))
	if err == nil {
		t.Fatal("make with a spec that nothing fits: no error")
	}
	wantFits(t, err.Error())
}
