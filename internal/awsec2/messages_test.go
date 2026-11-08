package awsec2

import (
	"context"
	"io/fs"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
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
// fits in adapterio.MaxMessage: a settings path of 60 characters, which comes last, a
// long region and zone, and the largest values of a machine spec. Package machine
// tests its messages in messages_test.go.
func TestMessagesFit(t *testing.T) {
	const ok = `"subnet_id": "subnet-0123456789abcdef0", "security_group_id": "sg-0123456789abcdef0", "image_id": "ami-0123456789abcdef0"`
	for _, doc := range []string{
		``,
		`[]`,
		`{"subnet_id": "subnet-01234567",}`,
		`{"subnet_id": "subnet-01234567", "subnet_id": "subnet-01234567"}`,
		`{"other": 1}`,
		`{}`,
		`{"subnet_id": "x", "security_group_id": "x", "image_id": "x"}`,
		`{"subnet_id": "subnet-0123456789abcdef0", "security_group_id": "x", "image_id": "x"}`,
		`{"subnet_id": "subnet-0123456789abcdef0", "security_group_id": "sg-0123456789abcdef0", "image_id": "x"}`,
		`{` + ok + `, "instance_types": ["t3.medium"]}`,
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

	// The defects of check and make, with a long region and zone.
	region := func(f *fakeAPI) *Handler {
		h, _, _ := testHandler(f)
		h.client = func(context.Context) (*machine.Client, error) {
			return machine.NewWithPrices(f, f, "ap-southeast-4"), nil
		}
		return h
	}
	longZone := func(f *fakeAPI) *fakeAPI {
		f.subnet = func(in *ec2.DescribeSubnetsInput) (*ec2.DescribeSubnetsOutput, error) {
			return &ec2.DescribeSubnetsOutput{Subnets: []types.Subnet{{SubnetId: aws.String(in.SubnetIds[0]), AvailabilityZone: aws.String("ap-southeast-4-wl1-mel-wlz-1")}}}, nil
		}
		offering := f.offering
		f.offering = func(in *ec2.DescribeInstanceTypeOfferingsInput) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
			out, err := offering(in)
			for i := range out.InstanceTypeOfferings {
				out.InstanceTypeOfferings[i].Location = aws.String("ap-southeast-4-wl1-mel-wlz-1")
			}
			return out, err
		}
		return f
	}
	_, err = region(longZone(world("m7g.large"))).Check(context.Background(), writeSettings(t, validSettings), contracts.MachinesCheck{Version: 1})
	if err == nil || !strings.Contains(err.Error(), "ap-southeast-4-wl1-mel-wlz-1") {
		t.Fatalf("check: error %v, want one with the zone", err)
	}
	wantFits(t, err.Error())
	spec := contracts.MachineSpec{VCPUs: 1024, MemoryMiB: 8388608, DiskGiB: 16384, Arch: "arm64", Virtualization: true}
	_, err = region(longZone(world())).Make(context.Background(), writeSettings(t, validSettings), contracts.MachinesMake{Version: 1, Key: testKey, Pool: testPool, MachineSpec: spec}, []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "ap-southeast-4-wl1-mel-wlz-1") {
		t.Fatalf("make with a spec that nothing fits: error %v", err)
	}
	wantFits(t, err.Error())

	// The defect of a disk above the largest gp3 volume, with the largest value.
	spec.DiskGiB = 65536
	_, err = region(world()).Make(context.Background(), writeSettings(t, validSettings), contracts.MachinesMake{Version: 1, Key: testKey, Pool: testPool, MachineSpec: spec}, []byte("x"))
	if err == nil || !strings.HasPrefix(err.Error(), "Report this defect") {
		t.Fatalf("make with a disk larger than a gp3 volume: error %v", err)
	}
	wantFits(t, err.Error())
}
