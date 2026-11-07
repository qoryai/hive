package machine

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/qoryai/integrations/contracts"
)

// typeInfo returns an EC2 instance type with vcpus, memory in MiB, the architectures
// and the processor features.
func typeInfo(name string, vcpus int32, mib int64, bare bool, archs []types.ArchitectureType, features ...types.SupportedAdditionalProcessorFeature) types.InstanceTypeInfo {
	return types.InstanceTypeInfo{
		InstanceType:  types.InstanceType(name),
		BareMetal:     aws.Bool(bare),
		VCpuInfo:      &types.VCpuInfo{DefaultVCpus: aws.Int32(vcpus)},
		MemoryInfo:    &types.MemoryInfo{SizeInMiB: aws.Int64(mib)},
		ProcessorInfo: &types.ProcessorInfo{SupportedArchitectures: archs, SupportedFeatures: features},
	}
}

// TestInstanceTypes pins the DescribeInstanceTypes input, the paging, what each type
// contains, and the errors for a type that EC2 does not return or does not know.
func TestInstanceTypes(t *testing.T) {
	pages := map[string]*ec2.DescribeInstanceTypesOutput{
		"": {
			InstanceTypes: []types.InstanceTypeInfo{typeInfo("t3.medium", 2, 4096, false, []types.ArchitectureType{"i386", "x86_64"})},
			NextToken:     aws.String("page-2"),
		},
		"page-2": {InstanceTypes: []types.InstanceTypeInfo{
			typeInfo("m8i.large", 2, 8192, false, []types.ArchitectureType{"x86_64"}, "amd-sev-snp", "nested-virtualization"),
			typeInfo("m7g.metal", 64, 262144, true, []types.ArchitectureType{"arm64", "arm64_mac"}),
		}},
	}
	f := &fakeAPI{itype: func(in *ec2.DescribeInstanceTypesInput) (*ec2.DescribeInstanceTypesOutput, error) {
		return pages[aws.ToString(in.NextToken)], nil
	}}
	c := NewInRegion(f, "eu-west-1")
	got, err := c.InstanceTypes(context.Background(), []string{"t3.medium", "m8i.large", "m7g.metal"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]InstanceType{
		"t3.medium": {Name: "t3.medium", VCPUs: 2, MemoryMiB: 4096, Archs: []string{ArchAMD64}},
		"m8i.large": {Name: "m8i.large", VCPUs: 2, MemoryMiB: 8192, Archs: []string{ArchAMD64}, NestedVirtualization: true},
		"m7g.metal": {Name: "m7g.metal", VCPUs: 64, MemoryMiB: 262144, Archs: []string{ArchARM64}, BareMetal: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("types %+v, want %+v", got, want)
	}
	if len(f.types) != 2 || !reflect.DeepEqual(f.types[0].InstanceTypes, []types.InstanceType{"t3.medium", "m8i.large", "m7g.metal"}) {
		t.Errorf("%d calls, first input %+v", len(f.types), f.types[0])
	}
	if !got["m8i.large"].Virtualization() || !got["m7g.metal"].Virtualization() || got["t3.medium"].Virtualization() {
		t.Error("Virtualization is not true for exactly the nested and the bare metal type")
	}

	_, err = c.InstanceTypes(context.Background(), []string{"t3.medium", "c7i.large"})
	const wantMissing = "Remove c7i.large from instance_types in aws-ec2.json: DescribeInstanceTypes returned no such type in eu-west-1"
	if err == nil || err.Error() != wantMissing || kindOf(err) != contracts.FailureDefect {
		t.Errorf("a missing type: error %v, want %q of kind defect", err, wantMissing)
	}

	f.itype = func(*ec2.DescribeInstanceTypesInput) (*ec2.DescribeInstanceTypesOutput, error) {
		return nil, apiError("InvalidInstanceType")
	}
	_, err = c.InstanceTypes(context.Background(), []string{"t3.huge"})
	const wantInvalid = "Set instance_types in aws-ec2.json to EC2 instance types that exist in eu-west-1: DescribeInstanceTypes failed: api error InvalidInstanceType: test message for InvalidInstanceType"
	if err == nil || err.Error() != wantInvalid || kindOf(err) != contracts.FailureDefect {
		t.Errorf("an unknown type: error %v, want %q of kind defect", err, wantInvalid)
	}

	f.itype = func(*ec2.DescribeInstanceTypesInput) (*ec2.DescribeInstanceTypesOutput, error) {
		return nil, apiError("UnauthorizedOperation")
	}
	_, err = c.InstanceTypes(context.Background(), []string{"t3.medium"})
	const wantDenied = "Give the instance role the EC2 rights that the EC2 machine adapter needs: DescribeInstanceTypes failed with UnauthorizedOperation: api error UnauthorizedOperation: test message for UnauthorizedOperation"
	if err == nil || err.Error() != wantDenied {
		t.Errorf("denied: error %v, want %q", err, wantDenied)
	}
}

// TestSubnetZone pins the DescribeSubnets input, the zone, and the errors for a
// subnet that does not exist and an answer with no zone.
func TestSubnetZone(t *testing.T) {
	const id = "subnet-0123456789abcdef0"
	var reply *ec2.DescribeSubnetsOutput
	var replyErr error
	f := &fakeAPI{subnet: func(*ec2.DescribeSubnetsInput) (*ec2.DescribeSubnetsOutput, error) { return reply, replyErr }}
	c := NewInRegion(f, "eu-west-1")

	reply = &ec2.DescribeSubnetsOutput{Subnets: []types.Subnet{{SubnetId: aws.String(id), AvailabilityZone: aws.String("eu-west-1a")}}}
	zone, err := c.SubnetZone(context.Background(), id)
	if err != nil || zone != "eu-west-1a" {
		t.Errorf("zone %q, error %v", zone, err)
	}
	if want := (&ec2.DescribeSubnetsInput{SubnetIds: []string{id}}); !reflect.DeepEqual(f.subnets[0], want) {
		t.Errorf("input %+v, want %+v", f.subnets[0], want)
	}

	for _, tc := range []struct {
		reply *ec2.DescribeSubnetsOutput
		err   error
		want  string
	}{
		{nil, apiError("InvalidSubnetID.NotFound"), "Set subnet_id in aws-ec2.json to a subnet that exists in eu-west-1: DescribeSubnets returned no subnet subnet-0123456789abcdef0: api error InvalidSubnetID.NotFound: test message for InvalidSubnetID.NotFound"},
		{&ec2.DescribeSubnetsOutput{}, nil, "Set subnet_id in aws-ec2.json to a subnet that exists in eu-west-1: DescribeSubnets returned no zone for subnet subnet-0123456789abcdef0"},
		{nil, apiError("UnauthorizedOperation"), "Give the instance role the EC2 rights that the EC2 machine adapter needs: DescribeSubnets failed with UnauthorizedOperation: api error UnauthorizedOperation: test message for UnauthorizedOperation"},
	} {
		reply, replyErr = tc.reply, tc.err
		_, err := c.SubnetZone(context.Background(), id)
		if err == nil || err.Error() != tc.want || kindOf(err) != contracts.FailureDefect {
			t.Errorf("error %v, want %q of kind defect", err, tc.want)
		}
	}
}

// TestOffered pins the DescribeInstanceTypeOfferings input, the paging, and that only
// the offerings in the zone count.
func TestOffered(t *testing.T) {
	offering := func(name, zone string) types.InstanceTypeOffering {
		return types.InstanceTypeOffering{InstanceType: types.InstanceType(name), Location: aws.String(zone), LocationType: types.LocationTypeAvailabilityZone}
	}
	pages := map[string]*ec2.DescribeInstanceTypeOfferingsOutput{
		"":       {InstanceTypeOfferings: []types.InstanceTypeOffering{offering("t3.medium", "eu-west-1a")}, NextToken: aws.String("page-2")},
		"page-2": {InstanceTypeOfferings: []types.InstanceTypeOffering{offering("m7i.large", "eu-west-1a"), offering("m7g.large", "eu-west-1b")}},
	}
	f := &fakeAPI{offering: func(in *ec2.DescribeInstanceTypeOfferingsInput) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
		return pages[aws.ToString(in.NextToken)], nil
	}}
	got, err := New(f).Offered(context.Background(), "eu-west-1a", []string{"t3.medium", "m7i.large", "m7g.large"})
	if err != nil || !reflect.DeepEqual(got, map[string]bool{"t3.medium": true, "m7i.large": true}) {
		t.Errorf("offered %v, error %v", got, err)
	}
	want := &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: types.LocationTypeAvailabilityZone,
		Filters: []types.Filter{
			{Name: aws.String("location"), Values: []string{"eu-west-1a"}},
			{Name: aws.String("instance-type"), Values: []string{"t3.medium", "m7i.large", "m7g.large"}},
		},
	}
	if len(f.offerings) != 2 || !reflect.DeepEqual(f.offerings[0], want) {
		t.Errorf("%d calls, first input %+v, want %+v", len(f.offerings), f.offerings[0], want)
	}

	f.offering = func(*ec2.DescribeInstanceTypeOfferingsInput) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
		return nil, apiError("UnauthorizedOperation")
	}
	_, err = New(f).Offered(context.Background(), "eu-west-1a", []string{"t3.medium"})
	if kindOf(err) != contracts.FailureDefect {
		t.Errorf("denied: error %v, want kind defect", err)
	}
}

// TestFindByKey pins the tag filters, the not-found case, the choice of a machine that
// is not terminated, and the instance type of the machine found.
func TestFindByKey(t *testing.T) {
	var reply []types.Instance
	f := &fakeAPI{describe: func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: reply}}}, nil
	}}
	c := New(f)
	const key, pool = "dp_0123456789abcdef", "np_0123456789abcdef"
	if _, found, err := c.FindByKey(context.Background(), pool, key); err != nil || found {
		t.Fatalf("no machine: found %v, error %v", found, err)
	}
	wantIn := &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{Name: aws.String("tag:qory:managed"), Values: []string{"true"}},
			{Name: aws.String("tag:qory:pool"), Values: []string{pool}},
			{Name: aws.String("tag:qory:key"), Values: []string{key}},
		},
		MaxResults: aws.Int32(1000),
	}
	if !reflect.DeepEqual(f.describes[0], wantIn) {
		t.Errorf("input %+v, want %+v", f.describes[0], wantIn)
	}

	running := instance("i-0bbbbbbbbbbbbbbb0", StateRunning, key)
	running.InstanceType = "m7i.large"
	reply = []types.Instance{instance("i-0aaaaaaaaaaaaaaa0", StateTerminated, key), running}
	m, found, err := c.FindByKey(context.Background(), pool, key)
	if err != nil || !found || m.ID != "i-0bbbbbbbbbbbbbbb0" || m.InstanceType != "m7i.large" {
		t.Errorf("machine %+v, found %v, error %v", m, found, err)
	}

	f.describe = func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return nil, apiError("RequestLimitExceeded")
	}
	if _, _, err := c.FindByKey(context.Background(), pool, key); kindOf(err) != contracts.FailureRetry {
		t.Errorf("error %v, want kind retry", err)
	}
}

// TestNoCapacity pins that NoCapacity matches InsufficientInstanceCapacity alone, also
// wrapped.
func TestNoCapacity(t *testing.T) {
	if !NoCapacity(fmt.Errorf("x: %w", apiError("InsufficientInstanceCapacity"))) {
		t.Error("NoCapacity does not match InsufficientInstanceCapacity")
	}
	for _, err := range []error{nil, apiError("InstanceLimitExceeded"), apiError("VcpuLimitExceeded"), apiError("InsufficientFreeAddressesInSubnet")} {
		if NoCapacity(err) {
			t.Errorf("NoCapacity matches %v", err)
		}
	}
}

// TestCreateNestedVirtualization pins that Create sends CpuOptions with nested
// virtualization enabled for a spec that sets it, and no CpuOptions otherwise, in
// Create and in CheckRights.
func TestCreateNestedVirtualization(t *testing.T) {
	f := &fakeAPI{run: func(in *ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
		if aws.ToBool(in.DryRun) {
			return nil, apiError("DryRunOperation")
		}
		return &ec2.RunInstancesOutput{Instances: []types.Instance{instance("i-0123456789abcdef0", StatePending, "x")}}, nil
	}}
	c := New(f)
	spec := testSpec()
	spec.InstanceType = "m8i.large"
	if _, err := c.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	spec.NestedVirtualization = true
	if _, err := c.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckRights(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	want := &types.CpuOptionsRequest{NestedVirtualization: types.NestedVirtualizationSpecificationEnabled}
	if f.runs[0].CpuOptions != nil || !reflect.DeepEqual(f.runs[1].CpuOptions, want) || !reflect.DeepEqual(f.runs[2].CpuOptions, want) {
		t.Errorf("CpuOptions %+v, %+v and %+v, want none, then nested virtualization twice", f.runs[0].CpuOptions, f.runs[1].CpuOptions, f.runs[2].CpuOptions)
	}
}
