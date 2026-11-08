package awsec2

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"

	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/hive/internal/machine"
	"github.com/qoryai/integrations/contracts"
)

// Neutral example values of the requests.
const (
	testKey  = "dp_0123456789abcdef"
	testPool = "np_0123456789abcdef"
	testID   = "i-0123456789abcdef0"
)

// serve runs command through adapterio.Serve with h, the settings file path and the
// request, and returns the exit status and standard output.
func serve(t *testing.T, h *Handler, command, path, request string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	status := adapterio.Serve(context.Background(), []string{command, "--settings", path}, strings.NewReader(request), &out, h)
	return status, out.String()
}

// wantFailure fails t unless the command exited with status 1 and wrote the error
// document of kind and message, cut to its first 200 characters.
func wantFailure(t *testing.T, status int, out, kind, message string) {
	t.Helper()
	if r := []rune(message); len(r) > adapterio.MaxMessage {
		message = string(r[:adapterio.MaxMessage])
	}
	if status != adapterio.ExitFailure {
		t.Fatalf("exit status %d, want 1; output %s", status, out)
	}
	doc, err := adapterio.DecodeError([]byte(strings.TrimSuffix(out, "\n")))
	if err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if doc.Error.Kind != kind || doc.Error.Message != message {
		t.Errorf("error %s: %s\nwant %s: %s", doc.Error.Kind, doc.Error.Message, kind, message)
	}
}

// makeRequest is a make request for a machine spec of vcpus, memory in MiB, disk in
// GiB, the architecture amd64 and virtualization virt, with the start data data.
func makeRequest(vcpus, mib, disk int, virt bool, data []byte) string {
	return fmt.Sprintf(`{"version": 1, "key": "%s", "pool": "%s", "machine_spec": {"vcpus": %d, "memory_mib": %d, "disk_gib": %d, "arch": "amd64", "virtualization": %t}, "start_data": "%s"}`,
		testKey, testPool, vcpus, mib, disk, virt, base64.StdEncoding.EncodeToString(data))
}

// testData is start data that no error message contains by chance.
var testData = []byte("#!/bin/sh\necho example-start-data\n")

// smallRequest is a make request that every type of catalog for amd64 fits.
func smallRequest(data []byte) string { return makeRequest(1, 1024, 8, false, data) }

// image is the DescribeImages output with the image of validSettings: x86_64, with
// its root device /dev/xvda of 30 GiB.
func image(*ec2.DescribeImagesInput) (*ec2.DescribeImagesOutput, error) {
	return &ec2.DescribeImagesOutput{Images: []types.Image{{
		ImageId:             aws.String("ami-0123456789abcdef0"),
		Architecture:        types.ArchitectureValuesX8664,
		RootDeviceName:      aws.String("/dev/xvda"),
		BlockDeviceMappings: []types.BlockDeviceMapping{{DeviceName: aws.String("/dev/xvda"), Ebs: &types.EbsBlockDevice{VolumeSize: aws.Int32(30)}}},
	}}}, nil
}

// armImage is the DescribeImages output with an arm64 image, with its root device
// /dev/xvda of 30 GiB.
func armImage(in *ec2.DescribeImagesInput) (*ec2.DescribeImagesOutput, error) {
	out, err := image(in)
	out.Images[0].Architecture = types.ArchitectureValuesArm64
	return out, err
}

// testZone is the zone of the subnet of validSettings.
const testZone = "eu-west-1a"

// catalog are the instance types that the fake EC2 knows in the region.
var catalog = map[string]types.InstanceTypeInfo{
	"t3.medium":      typeInfo("t3.medium", 2, 4096, false, "x86_64"),
	"t3a.medium":     typeInfo("t3a.medium", 2, 4096, false, "x86_64"),
	"m7i.large":      typeInfo("m7i.large", 2, 8192, false, "x86_64"),
	"m6i.large":      typeInfo("m6i.large", 2, 8192, false, "x86_64"),
	"c7i.xlarge":     typeInfo("c7i.xlarge", 4, 8192, false, "x86_64"),
	"m8i.large":      typeInfo("m8i.large", 2, 8192, false, "x86_64", types.SupportedAdditionalProcessorFeatureNestedVirtualization),
	"m7i.metal-24xl": typeInfo("m7i.metal-24xl", 96, 393216, true, "x86_64"),
	// z7i.metal-24xl has the size of m7i.metal-24xl and a lower price.
	"z7i.metal-24xl": typeInfo("z7i.metal-24xl", 96, 393216, true, "x86_64"),
	// m8i.metal-48xl is bare metal and lists nested virtualization: it runs virtual
	// machines without CpuOptions.
	"m8i.metal-48xl": typeInfo("m8i.metal-48xl", 192, 786432, true, "x86_64", types.SupportedAdditionalProcessorFeatureNestedVirtualization),
	"m7g.large":      typeInfo("m7g.large", 2, 8192, false, "arm64"),
	// g6.block is sold only as a capacity block, so make never starts it, though it is
	// the cheapest type.
	"g6.block": blockOnly(typeInfo("g6.block", 2, 4096, false, "x86_64")),
}

// testPrices are the on-demand prices in USD per hour that the fake price list has.
var testPrices = map[string]string{
	"t3.medium":      "0.0416000000",
	"t3a.medium":     "0.0376000000",
	"m7i.large":      "0.1008000000",
	"m6i.large":      "0.0960000000",
	"c7i.xlarge":     "0.1785000000",
	"m8i.large":      "0.1058000000",
	"m7i.metal-24xl": "4.8384000000",
	"z7i.metal-24xl": "4.5000000000",
	"m8i.metal-48xl": "10.1600000000",
	"m7g.large":      "0.0816000000",
	"g6.block":       "0.0010000000",
}

// typeInfo returns an EC2 instance type sold on demand.
func typeInfo(name string, vcpus int32, mib int64, bare bool, arch types.ArchitectureType, features ...types.SupportedAdditionalProcessorFeature) types.InstanceTypeInfo {
	return types.InstanceTypeInfo{
		InstanceType:          types.InstanceType(name),
		BareMetal:             aws.Bool(bare),
		VCpuInfo:              &types.VCpuInfo{DefaultVCpus: aws.Int32(vcpus)},
		MemoryInfo:            &types.MemoryInfo{SizeInMiB: aws.Int64(mib)},
		ProcessorInfo:         &types.ProcessorInfo{SupportedArchitectures: []types.ArchitectureType{arch}, SupportedFeatures: features},
		SupportedUsageClasses: []types.UsageClassType{types.UsageClassTypeOnDemand, types.UsageClassTypeSpot},
	}
}

// blockOnly returns t sold only as a capacity block.
func blockOnly(t types.InstanceTypeInfo) types.InstanceTypeInfo {
	t.SupportedUsageClasses = []types.UsageClassType{"capacity-block"}
	return t
}

// priceDoc returns a price list document of GetProducts for the instance type name
// with one on-demand hourly price in USD, of the product family "Compute Instance
// (bare metal)" for a name that contains metal, and "Compute Instance" for any other.
func priceDoc(name, usd string) string {
	family := "Compute Instance"
	if strings.Contains(name, "metal") {
		family = "Compute Instance (bare metal)"
	}
	b, err := json.Marshal(map[string]any{
		"product": map[string]any{"productFamily": family, "attributes": map[string]string{"instanceType": name}},
		"terms": map[string]any{"OnDemand": map[string]any{"SKU.TERM": map[string]any{"priceDimensions": map[string]any{
			"SKU.TERM.DIM": map[string]any{"unit": "Hrs", "pricePerUnit": map[string]string{"USD": usd}},
		}}}},
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// world returns a fake EC2 with the image of validSettings, its subnet in testZone,
// the instance types of catalog, of which testZone offers those in offered, or all
// when offered is empty, the prices of testPrices, dry runs that return
// DryRunOperation, no machine for any key, and a RunInstances that returns a pending
// instance of the type it got.
func world(offered ...string) *fakeAPI {
	if len(offered) == 0 {
		offered = slices.Sorted(maps.Keys(catalog))
	}
	return &fakeAPI{
		image: image,
		subnet: func(in *ec2.DescribeSubnetsInput) (*ec2.DescribeSubnetsOutput, error) {
			return &ec2.DescribeSubnetsOutput{Subnets: []types.Subnet{{SubnetId: aws.String(in.SubnetIds[0]), AvailabilityZone: aws.String(testZone)}}}, nil
		},
		itype: func(*ec2.DescribeInstanceTypesInput) (*ec2.DescribeInstanceTypesOutput, error) {
			out := &ec2.DescribeInstanceTypesOutput{}
			for _, name := range slices.Sorted(maps.Keys(catalog)) {
				out.InstanceTypes = append(out.InstanceTypes, catalog[name])
			}
			return out, nil
		},
		offering: func(*ec2.DescribeInstanceTypeOfferingsInput) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
			out := &ec2.DescribeInstanceTypeOfferingsOutput{}
			for _, t := range offered {
				out.InstanceTypeOfferings = append(out.InstanceTypeOfferings, types.InstanceTypeOffering{InstanceType: types.InstanceType(t), Location: aws.String(testZone), LocationType: types.LocationTypeAvailabilityZone})
			}
			return out, nil
		},
		product: func(*pricing.GetProductsInput) (*pricing.GetProductsOutput, error) {
			out := &pricing.GetProductsOutput{}
			for _, name := range slices.Sorted(maps.Keys(testPrices)) {
				out.PriceList = append(out.PriceList, priceDoc(name, testPrices[name]))
			}
			return out, nil
		},
		run: func(in *ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
			if aws.ToBool(in.DryRun) {
				return nil, apiError("DryRunOperation")
			}
			m := launched(testID, "pending", testKey)
			m.InstanceType = in.InstanceType
			return &ec2.RunInstancesOutput{Instances: []types.Instance{m}}, nil
		},
		describe: func(in *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
			if aws.ToBool(in.DryRun) {
				return nil, apiError("DryRunOperation")
			}
			return &ec2.DescribeInstancesOutput{}, nil
		},
	}
}

// launched returns an instance with id, state, the qory:key tag key and a launch time
// in UTC+2 with half a second.
func launched(id, state, key string) types.Instance {
	return types.Instance{
		InstanceId: aws.String(id),
		State:      &types.InstanceState{Name: types.InstanceStateName(state)},
		LaunchTime: aws.Time(time.Date(2026, 10, 22, 12, 0, 0, 500_000_000, time.FixedZone("UTC+2", 2*3600))),
		Tags: []types.Tag{
			{Key: aws.String("qory:managed"), Value: aws.String("true")},
			{Key: aws.String("qory:pool"), Value: aws.String(testPool)},
			{Key: aws.String("qory:key"), Value: aws.String(key)},
		},
	}
}

// TestCheck pins check: one DescribeImages for the image, one DescribeSubnets for the
// subnet, one DescribeInstanceTypes for the region's types, one
// DescribeInstanceTypeOfferings for the subnet's zone and one price lookup, then two
// RunInstances dry runs, with placeholder tags in the shapes of a pool id and a key:
// one for the first type in make's order, and one with CpuOptions for the first type
// that needs them. Then one DescribeInstances dry run. It makes no machine, and its
// answer has the one shape that covers every other.
func TestCheck(t *testing.T) {
	f := world()
	h, log, _ := testHandler(f)
	path := writeSettings(t, validSettings)
	status, out := serve(t, h, "check", path, `{"version": 1}`)
	const want = `{"offers":[{"vcpus":192,"memory_mib":786432,"disk_gib":16384,"arch":"amd64","virtualization":true}]}` + "\n"
	if status != adapterio.ExitOK || out != want {
		t.Fatalf("exit status %d, output\n%s\nwant\n%s", status, out, want)
	}
	if log.Len() != 0 {
		t.Errorf("log lines %s", log)
	}
	if len(f.images) != 1 || !reflect.DeepEqual(f.images[0].ImageIds, []string{"ami-0123456789abcdef0"}) {
		t.Errorf("DescribeImages calls %+v", f.images)
	}
	if len(f.subnets) != 1 || !reflect.DeepEqual(f.subnets[0].SubnetIds, []string{"subnet-0123456789abcdef0"}) {
		t.Errorf("DescribeSubnets calls %+v", f.subnets)
	}
	if len(f.types) != 1 || !reflect.DeepEqual(f.types[0], &ec2.DescribeInstanceTypesInput{MaxResults: aws.Int32(100)}) {
		t.Errorf("DescribeInstanceTypes calls %+v", f.types)
	}
	wantOfferings := &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: types.LocationTypeAvailabilityZone,
		Filters:      []types.Filter{{Name: aws.String("location"), Values: []string{testZone}}},
		MaxResults:   aws.Int32(1000),
	}
	if len(f.offerings) != 1 || !reflect.DeepEqual(f.offerings[0], wantOfferings) {
		t.Errorf("DescribeInstanceTypeOfferings calls %+v", f.offerings)
	}
	if len(f.products) != 1 || aws.ToString(f.products[0].Filters[0].Value) != "eu-west-1" {
		t.Errorf("GetProducts calls %+v", f.products)
	}
	var got []string
	for _, in := range f.runs {
		if !aws.ToBool(in.DryRun) {
			t.Error("check sent RunInstances without DryRun")
		}
		if in.ClientToken != nil {
			t.Errorf("the dry run has the client token %q", aws.ToString(in.ClientToken))
		}
		got = append(got, string(in.InstanceType))
		tags := in.TagSpecifications[0].Tags
		if aws.ToString(tags[1].Value) != "np_0000000000000000" || aws.ToString(tags[2].Value) != "dp_0000000000000000" {
			t.Errorf("tags %+v", tags)
		}
		ebs := in.BlockDeviceMappings[0].Ebs
		if aws.ToString(in.BlockDeviceMappings[0].DeviceName) != "/dev/xvda" || aws.ToInt32(ebs.VolumeSize) != 30 || in.UserData != nil {
			t.Errorf("input %+v", in)
		}
		if nested := in.CpuOptions != nil; nested != (in.InstanceType == "m8i.large") {
			t.Errorf("%s: CpuOptions %+v", in.InstanceType, in.CpuOptions)
		}
	}
	if want := []string{"t3a.medium", "m8i.large"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dry runs for %v, want %v", got, want)
	}
	if want := []*ec2.DescribeInstancesInput{{DryRun: aws.Bool(true)}}; !reflect.DeepEqual(f.describes, want) {
		t.Errorf("DescribeInstances calls %+v, want one dry run", f.describes)
	}
	if len(f.kills) != 0 || len(f.consoles) != 0 {
		t.Errorf("%d TerminateInstances and %d GetConsoleOutput calls, want none", len(f.kills), len(f.consoles))
	}

	f.describe = func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return nil, apiError("UnauthorizedOperation")
	}
	status, out = serve(t, h, "check", path, `{"version": 1}`)
	wantFailure(t, status, out, contracts.FailureDefect, "Give the instance role the EC2 rights that the EC2 machine adapter needs: DescribeInstances dry run failed with UnauthorizedOperation: api error UnauthorizedOperation: test message for UnauthorizedOperation")

	f.run = func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
		return nil, apiError("UnauthorizedOperation")
	}
	status, out = serve(t, h, "check", path, `{"version": 1}`)
	wantFailure(t, status, out, contracts.FailureDefect, "Give the instance role the EC2 rights that the EC2 machine adapter needs: RunInstances dry run failed with UnauthorizedOperation: api error UnauthorizedOperation: test message for UnauthorizedOperation")
}

// TestCheckDryRuns pins that check sends at most 2 dry runs also for a zone of many
// types, one when no type needs CpuOptions, and the error of the second dry run.
func TestCheckDryRuns(t *testing.T) {
	var names []string
	for i := range 20 {
		name := fmt.Sprintf("c7i.n%02d", i)
		catalog[name] = typeInfo(name, 2, 4096, false, "x86_64", types.SupportedAdditionalProcessorFeatureNestedVirtualization)
		names = append(names, name)
	}
	t.Cleanup(func() {
		for _, name := range names {
			delete(catalog, name)
		}
	})
	f := world()
	h, _, _ := testHandler(f)
	if status, out := serve(t, h, "check", writeSettings(t, validSettings), `{"version": 1}`); status != adapterio.ExitOK {
		t.Fatalf("exit status %d, output %q", status, out)
	}
	if len(f.runs) != 2 {
		t.Errorf("%d dry runs, want 2", len(f.runs))
	}

	f = world("t3.medium", "m7i.large", "m8i.metal-48xl")
	h, _, _ = testHandler(f)
	if status, out := serve(t, h, "check", writeSettings(t, validSettings), `{"version": 1}`); status != adapterio.ExitOK {
		t.Fatalf("exit status %d, output %q", status, out)
	}
	if len(f.runs) != 1 || f.runs[0].InstanceType != "t3.medium" || f.runs[0].CpuOptions != nil {
		t.Errorf("dry runs %+v, want one for t3.medium without CpuOptions", f.runs)
	}

	f = world()
	f.run = func(in *ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
		if in.CpuOptions != nil {
			return nil, apiError("InvalidParameterCombination")
		}
		return nil, apiError("DryRunOperation")
	}
	h, _, _ = testHandler(f)
	status, out := serve(t, h, "check", writeSettings(t, validSettings), `{"version": 1}`)
	wantFailure(t, status, out, contracts.FailureDefect, "Look up EC2 error code InvalidParameterCombination, then check aws-ec2.json and the instance role: RunInstances dry run failed: api error InvalidParameterCombination: test message for InvalidParameterCombination")
}

// TestCheckNoType pins check's defect, before any dry run, when the subnet's zone
// offers no on-demand instance type that runs the image's architecture.
func TestCheckNoType(t *testing.T) {
	for _, tc := range []struct {
		name    string
		offered []string
		image   func(*ec2.DescribeImagesInput) (*ec2.DescribeImagesOutput, error)
		arch    string
	}{
		{"only arm64 types for an x86_64 image", []string{"m7g.large"}, image, "amd64"},
		{"only a capacity block", []string{"g6.block"}, image, "amd64"},
		{"no arm64 type for an arm64 image", []string{"t3.medium", "m7i.large"}, armImage, "arm64"},
	} {
		f := world(tc.offered...)
		f.image = tc.image
		h, _, _ := testHandler(f)
		status, out := serve(t, h, "check", writeSettings(t, validSettings), `{"version": 1}`)
		wantFailure(t, status, out, contracts.FailureDefect, "Set subnet_id in aws-ec2.json to a subnet in another zone, or image_id to an image of another architecture: zone eu-west-1a offers no on-demand instance type for "+tc.arch)
		if len(f.runs) != 0 {
			t.Errorf("%s: %d RunInstances calls, want none", tc.name, len(f.runs))
		}
	}
}

// TestArchitecture pins that check offers and make starts only types of the image's
// architecture, read from DescribeImages. With an x86_64 image, an arm64 spec gets no
// offer and no type, and make fails with the defect of a spec that nothing fits. With
// an arm64 image, the same code offers and starts arm64 types.
func TestArchitecture(t *testing.T) {
	f := world()
	h, _, _ := testHandler(f)
	path := writeSettings(t, validSettings)
	status, out := serve(t, h, "check", path, `{"version": 1}`)
	if status != adapterio.ExitOK || strings.Contains(out, "arm64") {
		t.Errorf("x86_64 image: exit status %d, output %s, want no arm64 offer", status, out)
	}
	status, out = serve(t, h, "make", path, strings.Replace(makeRequest(2, 4096, 8, false, testData), "amd64", "arm64", 1))
	wantFailure(t, status, out, contracts.FailureDefect, "Run check again, or report this defect in the provisioner: zone eu-west-1a offers no type for image_id that fits 2 vCPUs, 4096 MiB, 8 GiB, arm64")
	if len(makes(f)) != 0 {
		t.Errorf("RunInstances calls %+v, want none", makes(f))
	}

	f = world()
	f.image = armImage
	h, _, _ = testHandler(f)
	status, out = serve(t, h, "check", path, `{"version": 1}`)
	const want = `{"offers":[{"vcpus":2,"memory_mib":8192,"disk_gib":16384,"arch":"arm64","virtualization":false}]}` + "\n"
	if status != adapterio.ExitOK || out != want {
		t.Errorf("arm64 image: exit status %d, output\n%s\nwant\n%s", status, out, want)
	}
	status, out = serve(t, h, "make", path, strings.Replace(makeRequest(2, 4096, 8, false, testData), "amd64", "arm64", 1))
	if status != adapterio.ExitOK || out != `{"id":"`+testID+`","machine_type":"m7g.large"}`+"\n" {
		t.Errorf("arm64 image: exit status %d, output %q", status, out)
	}
	status, out = serve(t, h, "make", path, makeRequest(2, 4096, 8, false, testData))
	wantFailure(t, status, out, contracts.FailureDefect, "Run check again, or report this defect in the provisioner: zone eu-west-1a offers no type for image_id that fits 2 vCPUs, 4096 MiB, 8 GiB, amd64")
}

// TestOffers pins the offers of check: only the shapes that no other shape covers,
// each shape once, the values above the bounds of an offer cut to those bounds, a type
// below them left out, the order by vCPUs, then memory, with virtualization first,
// and at most 64 offers, those with the most vCPUs, then memory.
func TestOffers(t *testing.T) {
	img := machine.Image{Arch: machine.ArchAMD64}
	it := func(name string, vcpus, mib int, nested bool) machine.InstanceType {
		return machine.InstanceType{Name: name, VCPUs: vcpus, MemoryMiB: mib, Archs: []string{machine.ArchAMD64}, NestedVirtualization: nested, OnDemand: true}
	}
	spec := func(vcpus, mib int, virt bool) contracts.MachineSpec {
		return contracts.MachineSpec{VCPUs: vcpus, MemoryMiB: mib, DiskGiB: machine.MaxRootGiB, Arch: machine.ArchAMD64, Virtualization: virt}
	}
	ts := []machine.InstanceType{
		it("a.large", 64, 131072, false),
		it("b.large", 32, 262144, false),
		it("c.large", 16, 65536, true),
		it("d.large", 16, 65536, false),
		it("e.large", 8, 32768, false),
		it("e2.large", 8, 32768, false),
		it("tiny.large", 1, 256, false),
		it("huge.large", 2000, 9000000, false),
	}
	got := offers(ts, img)
	if want := []contracts.MachineSpec{spec(1024, 8388608, false), spec(16, 65536, true)}; !reflect.DeepEqual(got, want) {
		t.Errorf("offers %+v, want %+v", got, want)
	}

	got = offers(ts[:6], img)
	want := []contracts.MachineSpec{spec(64, 131072, false), spec(32, 262144, false), spec(16, 65536, true)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("offers %+v, want %+v", got, want)
	}

	if got := offers([]machine.InstanceType{it("tiny.large", 1, 256, false)}, img); len(got) != 0 {
		t.Errorf("offers %+v, want none", got)
	}

	ts = nil
	for i := 1; i <= 70; i++ {
		ts = append(ts, it(fmt.Sprintf("s%02d.large", i), i, (100-i)*1024, false))
	}
	got = offers(ts, img)
	if len(got) != 64 || got[0] != spec(70, 30*1024, false) || got[63] != spec(7, 93*1024, false) {
		t.Errorf("%d offers, first %+v, last %+v, want 64 from 70 vCPUs down to 7", len(got), got[0], got[len(got)-1])
	}
}

// TestCheckStopsEarly pins that check makes no EC2 call when the settings or the
// credentials fail, and writes their errors, and that it stops after a failed read of
// the image, the subnet or the instance types.
func TestCheckStopsEarly(t *testing.T) {
	f := &fakeAPI{}
	h, _, built := testHandler(f)
	status, out := serve(t, h, "check", writeSettings(t, `{}`), `{"version": 1}`)
	wantFailure(t, status, out, contracts.FailureDefect, "Add the missing members to aws-ec2.json: subnet_id, security_group_id, image_id.")
	if *built != 0 || f.calls() != 0 {
		t.Errorf("%d clients built and %d EC2 calls, want none", *built, f.calls())
	}

	h.client = func(context.Context) (*machine.Client, error) {
		return nil, &adapterio.Failure{Kind: contracts.FailureDefect, Message: "Set AWS_REGION: the environment has no region, and the instance metadata returned none"}
	}
	status, out = serve(t, h, "check", writeSettings(t, validSettings), `{"version": 1}`)
	wantFailure(t, status, out, contracts.FailureDefect, "Set AWS_REGION: the environment has no region, and the instance metadata returned none")
	if f.calls() != 0 {
		t.Errorf("%d EC2 calls, want none", f.calls())
	}

	f = world()
	f.image = func(*ec2.DescribeImagesInput) (*ec2.DescribeImagesOutput, error) {
		return nil, apiError("InvalidAMIID.NotFound")
	}
	h, _, _ = testHandler(f)
	status, out = serve(t, h, "check", writeSettings(t, validSettings), `{"version": 1}`)
	wantFailure(t, status, out, contracts.FailureDefect, "Set image_id in aws-ec2.json to an image that exists in eu-west-1: DescribeImages returned no image ami-0123456789abcdef0: api error InvalidAMIID.NotFound: test message for InvalidAMIID.NotFound")
	if len(f.runs) != 0 || len(f.subnets) != 0 {
		t.Errorf("%d RunInstances and %d DescribeSubnets calls, want none", len(f.runs), len(f.subnets))
	}

	f = world()
	f.subnet = func(*ec2.DescribeSubnetsInput) (*ec2.DescribeSubnetsOutput, error) {
		return nil, apiError("UnauthorizedOperation")
	}
	h, _, _ = testHandler(f)
	status, out = serve(t, h, "check", writeSettings(t, validSettings), `{"version": 1}`)
	wantFailure(t, status, out, contracts.FailureDefect, "Give the instance role the EC2 rights that the EC2 machine adapter needs: DescribeSubnets failed with UnauthorizedOperation: api error UnauthorizedOperation: test message for UnauthorizedOperation")
	if len(f.runs) != 0 || len(f.types) != 0 {
		t.Errorf("%d RunInstances and %d DescribeInstanceTypes calls, want none", len(f.runs), len(f.types))
	}

	f = world()
	f.itype = func(*ec2.DescribeInstanceTypesInput) (*ec2.DescribeInstanceTypesOutput, error) {
		return nil, apiError("UnauthorizedOperation")
	}
	h, _, _ = testHandler(f)
	status, out = serve(t, h, "check", writeSettings(t, validSettings), `{"version": 1}`)
	wantFailure(t, status, out, contracts.FailureDefect, "Give the instance role the EC2 rights that the EC2 machine adapter needs: DescribeInstanceTypes failed with UnauthorizedOperation: api error UnauthorizedOperation: test message for UnauthorizedOperation")
	if len(f.runs) != 0 || len(f.offerings) != 0 {
		t.Errorf("%d RunInstances and %d DescribeInstanceTypeOfferings calls, want none", len(f.runs), len(f.offerings))
	}
}

// makes are the RunInstances calls of f that are not dry runs.
func makes(f *fakeAPI) []*ec2.RunInstancesInput {
	var out []*ec2.RunInstancesInput
	for _, in := range f.runs {
		if !aws.ToBool(in.DryRun) {
			out = append(out, in)
		}
	}
	return out
}

// TestMake pins make: the smallest type that fits, the start data as base64 user
// data, the client token of the key and the type, the key as the qory:key tag, the
// pool as the qory:pool tag, an encrypted gp3 root volume of the image's 30 GiB for a
// smaller disk, no CpuOptions, a lookup of the key before the launch, and the id and
// the type of the instance in the answer.
func TestMake(t *testing.T) {
	f := world("t3.medium", "m7i.large")
	h, _, _ := testHandler(f)
	path := writeSettings(t, validSettings)
	data := []byte("#!/bin/sh\necho start\n")
	for range 2 {
		status, out := serve(t, h, "make", path, smallRequest(data))
		if status != adapterio.ExitOK || out != `{"id":"`+testID+`","machine_type":"t3.medium"}`+"\n" {
			t.Fatalf("exit status %d, output %q", status, out)
		}
	}
	if len(f.runs) != 2 || !reflect.DeepEqual(f.runs[0], f.runs[1]) {
		t.Fatalf("RunInstances calls %d, or the second input differs", len(f.runs))
	}
	in := f.runs[0]
	if aws.ToString(in.ClientToken) != testKey+".t3.medium" || in.DryRun != nil || in.InstanceType != "t3.medium" || in.CpuOptions != nil {
		t.Errorf("client token %q, dry run %v, instance type %q, CpuOptions %+v", aws.ToString(in.ClientToken), in.DryRun, in.InstanceType, in.CpuOptions)
	}
	if got, err := base64.StdEncoding.DecodeString(aws.ToString(in.UserData)); err != nil || !bytes.Equal(got, data) {
		t.Errorf("user data %q, error %v", got, err)
	}
	wantRoot := []types.BlockDeviceMapping{{
		DeviceName: aws.String("/dev/xvda"),
		Ebs:        &types.EbsBlockDevice{Encrypted: aws.Bool(true), DeleteOnTermination: aws.Bool(true), VolumeSize: aws.Int32(30), VolumeType: types.VolumeTypeGp3},
	}}
	if !reflect.DeepEqual(in.BlockDeviceMappings, wantRoot) {
		t.Errorf("block device mappings %+v, want %+v", in.BlockDeviceMappings, wantRoot)
	}
	wantLookup := []types.Filter{
		{Name: aws.String("tag:qory:managed"), Values: []string{"true"}},
		{Name: aws.String("tag:qory:pool"), Values: []string{testPool}},
		{Name: aws.String("tag:qory:key"), Values: []string{testKey}},
	}
	if len(f.describes) != 2 || !reflect.DeepEqual(f.describes[0].Filters, wantLookup) {
		t.Errorf("DescribeInstances calls %+v, want one lookup by key per make", f.describes)
	}
	wantTags := []types.Tag{
		{Key: aws.String("qory:managed"), Value: aws.String("true")},
		{Key: aws.String("qory:pool"), Value: aws.String(testPool)},
		{Key: aws.String("qory:key"), Value: aws.String(testKey)},
	}
	wantTagSpecs(t, in, wantTags)
	ni := in.NetworkInterfaces
	if len(ni) != 1 || aws.ToBool(ni[0].AssociatePublicIpAddress) || ni[0].AssociatePublicIpAddress == nil ||
		aws.ToString(ni[0].SubnetId) != "subnet-0123456789abcdef0" || !reflect.DeepEqual(ni[0].Groups, []string{"sg-0123456789abcdef0"}) ||
		in.SubnetId != nil || in.SecurityGroupIds != nil {
		t.Errorf("network interfaces %+v, subnet %v, groups %v", ni, in.SubnetId, in.SecurityGroupIds)
	}
	mo := in.MetadataOptions
	if mo.HttpTokens != types.HttpTokensStateRequired || mo.HttpEndpoint != types.InstanceMetadataEndpointStateEnabled || aws.ToInt32(mo.HttpPutResponseHopLimit) != 1 {
		t.Errorf("metadata options %+v", mo)
	}
	if in.InstanceInitiatedShutdownBehavior != types.ShutdownBehaviorStop {
		t.Errorf("shutdown %q", in.InstanceInitiatedShutdownBehavior)
	}
	// The second make reads the types, the zone's types and the prices from the cache.
	if len(f.subnets) != 2 || len(f.types) != 1 || len(f.offerings) != 1 || len(f.products) != 1 {
		t.Errorf("%d DescribeSubnets, %d DescribeInstanceTypes, %d DescribeInstanceTypeOfferings and %d GetProducts calls, want 2, 1, 1 and 1", len(f.subnets), len(f.types), len(f.offerings), len(f.products))
	}
}

// TestMakeChoosesType pins the type that make starts: the first that fits, by vCPUs,
// then memory, then price, a type without a price after the types with one, then
// name. A type that does not run the image's architecture, and a type that is not sold
// on demand, are left out.
func TestMakeChoosesType(t *testing.T) {
	for _, tc := range []struct {
		name    string
		offered []string
		prices  map[string]string
		request string
		want    string
	}{
		{"the fewest vCPUs", []string{"c7i.xlarge", "m7i.large"}, nil, makeRequest(2, 4096, 8, false, nil), "m7i.large"},
		{"the least memory", []string{"m7i.large", "t3.medium"}, nil, makeRequest(2, 4096, 8, false, nil), "t3.medium"},
		{"the lower price", []string{"t3.medium", "t3a.medium"}, nil, makeRequest(2, 4096, 8, false, nil), "t3a.medium"},
		{"the lower price, other values", []string{"m7i.large", "m6i.large"}, nil, makeRequest(2, 8192, 8, false, nil), "m6i.large"},
		{"a priced type before one without a price", []string{"t3.medium", "t3a.medium"}, map[string]string{"t3.medium": "0.0416000000"}, makeRequest(2, 4096, 8, false, nil), "t3.medium"},
		{"the name without prices", []string{"t3a.medium", "t3.medium"}, map[string]string{}, makeRequest(2, 4096, 8, false, nil), "t3.medium"},
		{"more memory than the smaller type", []string{"t3.medium", "c7i.xlarge", "m7i.large"}, nil, makeRequest(1, 6000, 8, false, nil), "m7i.large"},
		{"more vCPUs than the smaller types", []string{"t3.medium", "m7i.large", "c7i.xlarge"}, nil, makeRequest(3, 1024, 8, false, nil), "c7i.xlarge"},
		{"another architecture left out", []string{"m7g.large", "m7i.large"}, nil, makeRequest(2, 8192, 8, false, nil), "m7i.large"},
		{"a capacity block left out", []string{"g6.block", "t3.medium"}, nil, makeRequest(2, 4096, 8, false, nil), "t3.medium"},
		{"virtualization", []string{"m7i.large", "m8i.large"}, nil, makeRequest(2, 8192, 8, true, nil), "m8i.large"},
		{"virtualization on bare metal", []string{"m7i.large", "m7i.metal-24xl"}, nil, makeRequest(2, 8192, 8, true, nil), "m7i.metal-24xl"},
		{"the lower price of bare metal", []string{"m7i.metal-24xl", "z7i.metal-24xl"}, nil, makeRequest(96, 393216, 8, true, nil), "z7i.metal-24xl"},
		{"every type of the zone", nil, nil, makeRequest(2, 4096, 8, false, nil), "t3a.medium"},
	} {
		f := world(tc.offered...)
		if tc.prices != nil {
			f.product = func(*pricing.GetProductsInput) (*pricing.GetProductsOutput, error) {
				out := &pricing.GetProductsOutput{}
				for name, usd := range tc.prices {
					out.PriceList = append(out.PriceList, priceDoc(name, usd))
				}
				return out, nil
			}
		}
		h, _, _ := testHandler(f)
		status, out := serve(t, h, "make", writeSettings(t, validSettings), strings.Replace(tc.request, `"start_data": ""`, `"start_data": "eA=="`, 1))
		if status != adapterio.ExitOK || out != `{"id":"`+testID+`","machine_type":"`+tc.want+`"}`+"\n" {
			t.Errorf("%s: exit status %d, output %q, want %s", tc.name, status, out, tc.want)
			continue
		}
		if got := makes(f); len(got) != 1 || string(got[0].InstanceType) != tc.want {
			t.Errorf("%s: RunInstances calls %+v", tc.name, got)
		}
	}
}

// TestMakePriceFailure pins that a failed price lookup does not stop make: it writes
// one log line, and make orders the types of one size by name.
func TestMakePriceFailure(t *testing.T) {
	f := world("t3a.medium", "t3.medium")
	f.product = func(*pricing.GetProductsInput) (*pricing.GetProductsOutput, error) {
		return nil, apiError("AccessDeniedException")
	}
	h, log, _ := testHandler(f)
	status, out := serve(t, h, "make", writeSettings(t, validSettings), makeRequest(2, 4096, 8, false, testData))
	if status != adapterio.ExitOK || out != `{"id":"`+testID+`","machine_type":"t3.medium"}`+"\n" {
		t.Fatalf("exit status %d, output %q", status, out)
	}
	if strings.Count(log.String(), "\n") != 1 || !strings.Contains(log.String(), msgNoPrices) || !strings.Contains(log.String(), "AccessDeniedException") {
		t.Errorf("log\n%s\nwant one line: %s", log, msgNoPrices)
	}
}

// TestMakeNothingFits pins that make fails with the kind defect, and makes no machine,
// when no type that the zone offers fits the spec.
func TestMakeNothingFits(t *testing.T) {
	const prefix = "Run check again, or report this defect in the provisioner: zone eu-west-1a offers no type for image_id that fits "
	for _, tc := range []struct {
		request, want string
	}{
		{makeRequest(200, 1024, 8, false, testData), prefix + "200 vCPUs, 1024 MiB, 8 GiB, amd64"},
		{makeRequest(2, 1048576, 8, false, testData), prefix + "2 vCPUs, 1048576 MiB, 8 GiB, amd64"},
		{makeRequest(200, 1024, 8, true, testData), prefix + "200 vCPUs, 1024 MiB, 8 GiB, amd64, with virtualization"},
		{strings.Replace(makeRequest(2, 1024, 8, false, testData), "amd64", "arm64", 1), prefix + "2 vCPUs, 1024 MiB, 8 GiB, arm64"},
	} {
		f := world()
		h, _, _ := testHandler(f)
		status, out := serve(t, h, "make", writeSettings(t, validSettings), tc.request)
		wantFailure(t, status, out, contracts.FailureDefect, tc.want)
		if len(f.runs) != 0 || len(f.describes) != 0 {
			t.Errorf("%d RunInstances and %d DescribeInstances calls, want none", len(f.runs), len(f.describes))
		}
	}
}

// TestMakeNestedVirtualization pins that make turns nested virtualization on only for
// a spec that sets it, on a type that has it, and not on a bare metal type.
func TestMakeNestedVirtualization(t *testing.T) {
	want := &types.CpuOptionsRequest{NestedVirtualization: types.NestedVirtualizationSpecificationEnabled}
	for _, tc := range []struct {
		offered string
		virt    bool
		want    *types.CpuOptionsRequest
	}{
		{"m8i.large", true, want},
		{"m8i.large", false, nil},
		{"m7i.metal-24xl", true, nil},
		{"m8i.metal-48xl", true, nil},
	} {
		f := world(tc.offered)
		h, _, _ := testHandler(f)
		if status, out := serve(t, h, "make", writeSettings(t, validSettings), makeRequest(2, 1024, 8, tc.virt, testData)); status != adapterio.ExitOK {
			t.Fatalf("exit status %d, output %q", status, out)
		}
		if got := makes(f)[0].CpuOptions; !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s, virtualization %v: CpuOptions %+v, want %+v", tc.offered, tc.virt, got, tc.want)
		}
	}
}

// TestMakeRootVolume pins the root volume: the larger of the spec's disk and the
// image's root volume, encrypted, gp3 and deleted with the machine.
func TestMakeRootVolume(t *testing.T) {
	for disk, want := range map[int]int32{1: 30, 30: 30, 31: 31, 16384: 16384} {
		f := world()
		h, _, _ := testHandler(f)
		if status, out := serve(t, h, "make", writeSettings(t, validSettings), makeRequest(1, 1024, disk, false, testData)); status != adapterio.ExitOK {
			t.Fatalf("disk %d: exit status %d, output %q", disk, status, out)
		}
		ebs := makes(f)[0].BlockDeviceMappings[0].Ebs
		if aws.ToInt32(ebs.VolumeSize) != want || ebs.VolumeType != types.VolumeTypeGp3 || !aws.ToBool(ebs.Encrypted) || !aws.ToBool(ebs.DeleteOnTermination) {
			t.Errorf("disk %d: root volume %+v, want %d GiB, gp3, encrypted, deleted on termination", disk, ebs, want)
		}
	}
}

// TestMakeDiskAboveGP3 pins that make fails with the kind defect, before any EC2 call,
// for a spec whose disk is larger than the largest gp3 volume, 16384 GiB.
func TestMakeDiskAboveGP3(t *testing.T) {
	for _, disk := range []int{16385, 65536} {
		f := world()
		h, _, built := testHandler(f)
		status, out := serve(t, h, "make", writeSettings(t, validSettings), makeRequest(1, 1024, disk, false, testData))
		wantFailure(t, status, out, contracts.FailureDefect, fmt.Sprintf("Report this defect in the provisioner: the machine spec has disk_gib %d GiB, and a gp3 volume has at most 16384 GiB", disk))
		if *built != 0 || f.calls() != 0 {
			t.Errorf("disk %d: %d clients built and %d EC2 calls, want none", disk, *built, f.calls())
		}
	}
}

// capacityFor returns a RunInstances of f that fails with InsufficientInstanceCapacity
// for the types in full, and otherwise acts as world's.
func capacityFor(f *fakeAPI, full ...string) {
	run := f.run
	f.run = func(in *ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
		if !aws.ToBool(in.DryRun) && slices.Contains(full, string(in.InstanceType)) {
			return nil, apiError("InsufficientInstanceCapacity")
		}
		return run(in)
	}
}

// TestMakeCapacityFallback pins that make tries the next type that fits, in the same
// order, when EC2 has no capacity for a type, with a log line, and returns the type it
// started. A type that does not fit is never tried.
func TestMakeCapacityFallback(t *testing.T) {
	f := world()
	capacityFor(f, "t3a.medium", "t3.medium")
	h, log, _ := testHandler(f)
	status, out := serve(t, h, "make", writeSettings(t, validSettings), makeRequest(2, 4096, 8, false, testData))
	if status != adapterio.ExitOK || out != `{"id":"`+testID+`","machine_type":"m6i.large"}`+"\n" {
		t.Fatalf("exit status %d, output %q", status, out)
	}
	var tried, tokens []string
	for _, in := range makes(f) {
		tried = append(tried, string(in.InstanceType))
		tokens = append(tokens, aws.ToString(in.ClientToken))
	}
	if want := []string{"t3a.medium", "t3.medium", "m6i.large"}; !reflect.DeepEqual(tried, want) {
		t.Errorf("tried %v, want %v", tried, want)
	}
	if want := []string{testKey + ".t3a.medium", testKey + ".t3.medium", testKey + ".m6i.large"}; !reflect.DeepEqual(tokens, want) {
		t.Errorf("client tokens %v, want %v", tokens, want)
	}
	if n := strings.Count(log.String(), "EC2 has no capacity for the instance type"); n != 2 || !strings.Contains(log.String(), "instance_type=t3.medium next=m6i.large") {
		t.Errorf("%d capacity log lines: %s", n, log)
	}
}

// TestMakeCapacityBound pins that make tries at most 8 types, maxTries, and then
// returns the capacity failure of the eighth.
func TestMakeCapacityBound(t *testing.T) {
	var names []string
	for i := range 12 {
		name := fmt.Sprintf("c7i.n%02d", i)
		catalog[name] = typeInfo(name, 2, 4096, false, "x86_64")
		names = append(names, name)
	}
	t.Cleanup(func() {
		for _, name := range names {
			delete(catalog, name)
		}
	})
	f := world(names...)
	capacityFor(f, names...)
	h, log, _ := testHandler(f)
	status, out := serve(t, h, "make", writeSettings(t, validSettings), makeRequest(2, 4096, 8, false, testData))
	wantFailure(t, status, out, contracts.FailureLimit, "Wait, or set subnet_id in aws-ec2.json to a subnet in another zone: RunInstances failed with InsufficientInstanceCapacity in the subnet's zone: api error InsufficientInstanceCapacity: test message for InsufficientInstanceCapacity")
	var tried []string
	for _, in := range makes(f) {
		tried = append(tried, string(in.InstanceType))
	}
	if !reflect.DeepEqual(tried, names[:maxTries]) {
		t.Errorf("tried %v, want %v", tried, names[:maxTries])
	}
	if n := strings.Count(log.String(), "EC2 has no capacity for the instance type"); n != maxTries-1 {
		t.Errorf("%d capacity log lines, want %d", n, maxTries-1)
	}
}

// TestMakeCapacityAllFail pins that make returns the failure of the last type, with
// its kind, when EC2 has no capacity for any type that fits.
func TestMakeCapacityAllFail(t *testing.T) {
	f := world("m7i.large", "t3.medium", "m7g.large")
	capacityFor(f, "t3.medium", "m7i.large")
	h, _, _ := testHandler(f)
	status, out := serve(t, h, "make", writeSettings(t, validSettings), smallRequest(testData))
	wantFailure(t, status, out, contracts.FailureLimit, "Wait, or set subnet_id in aws-ec2.json to a subnet in another zone: RunInstances failed with InsufficientInstanceCapacity in the subnet's zone: api error InsufficientInstanceCapacity: test message for InsufficientInstanceCapacity")
	if got := makes(f); len(got) != 2 || got[1].InstanceType != "m7i.large" {
		t.Errorf("RunInstances calls %+v, want t3.medium, then m7i.large", got)
	}

	f = world("t3.medium", "m7i.large", "c7i.xlarge")
	f.run = func(in *ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
		if in.InstanceType == "t3.medium" {
			return nil, apiError("InsufficientInstanceCapacity")
		}
		return nil, apiError("VcpuLimitExceeded")
	}
	h, _, _ = testHandler(f)
	status, out = serve(t, h, "make", writeSettings(t, validSettings), smallRequest(testData))
	wantFailure(t, status, out, contracts.FailureLimit, "Wait until machines end, or raise the account's EC2 instance or vCPU quota: RunInstances failed with VcpuLimitExceeded: api error VcpuLimitExceeded: test message for VcpuLimitExceeded")
	if got := makes(f); len(got) != 2 {
		t.Errorf("%d RunInstances calls, want 2: another failure than capacity stops the tries", len(got))
	}
}

// TestMakeOneMachinePerKey pins that a second make with the same key makes no second
// machine after a capacity fallback: make finds the first make's machine by its
// qory:key tag and returns its id and type, also when the type that the first make
// tried first has capacity again.
func TestMakeOneMachinePerKey(t *testing.T) {
	f := world("t3.medium", "m7i.large")
	capacityFor(f, "t3.medium")
	var made []types.Instance
	run := f.run
	f.run = func(in *ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
		out, err := run(in)
		if err == nil && !aws.ToBool(in.DryRun) {
			made = append(made, out.Instances[0])
		}
		return out, err
	}
	f.describe = func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: made}}}, nil
	}
	h, _, _ := testHandler(f)
	path := writeSettings(t, validSettings)
	status, out := serve(t, h, "make", path, smallRequest(testData))
	if status != adapterio.ExitOK || out != `{"id":"`+testID+`","machine_type":"m7i.large"}`+"\n" {
		t.Fatalf("first make: exit status %d, output %q", status, out)
	}

	f.run = func(in *ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
		t.Errorf("the second make sent RunInstances for %s", in.InstanceType)
		return nil, apiError("InternalError")
	}
	status, out = serve(t, h, "make", path, smallRequest(testData))
	if status != adapterio.ExitOK || out != `{"id":"`+testID+`","machine_type":"m7i.large"}`+"\n" {
		t.Fatalf("second make: exit status %d, output %q", status, out)
	}
	if len(made) != 1 {
		t.Errorf("%d machines made, want 1", len(made))
	}

	f.describe = func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return nil, apiError("RequestLimitExceeded")
	}
	status, out = serve(t, h, "make", path, smallRequest(testData))
	if status != adapterio.ExitFailure || !strings.Contains(out, `"kind":"retry"`) {
		t.Errorf("a failed lookup: exit status %d, output %q, want kind retry", status, out)
	}
}

// wantTagSpecs fails t unless in tags the instance and the volume, in that order,
// each with tags.
func wantTagSpecs(t *testing.T, in *ec2.RunInstancesInput, tags []types.Tag) {
	t.Helper()
	want := []types.TagSpecification{
		{ResourceType: types.ResourceTypeInstance, Tags: tags},
		{ResourceType: types.ResourceTypeVolume, Tags: tags},
	}
	if !reflect.DeepEqual(in.TagSpecifications, want) {
		t.Errorf("tag specifications\n%+v\nwant\n%+v", in.TagSpecifications, want)
	}
}

// TestTags pins that make and check's dry run send the tags of the settings after
// qory:managed, qory:pool and qory:key, sorted by key, on the instance and the
// volume.
func TestTags(t *testing.T) {
	f := world()
	h, _, _ := testHandler(f)
	path := writeSettings(t, withTags(`{"qory:stack": "example-stack", "env": "test"}`))
	if status, out := serve(t, h, "make", path, smallRequest(testData)); status != adapterio.ExitOK {
		t.Fatalf("make: exit status %d, output %q", status, out)
	}
	if status, out := serve(t, h, "check", path, `{"version": 1}`); status != adapterio.ExitOK {
		t.Fatalf("check: exit status %d, output %q", status, out)
	}
	if len(f.runs) != 3 {
		t.Fatalf("%d RunInstances calls, want make's and 2 dry runs", len(f.runs))
	}
	for i, in := range f.runs {
		pool, key := testPool, testKey
		if i > 0 {
			pool, key = "np_0000000000000000", "dp_0000000000000000"
		}
		wantTagSpecs(t, in, []types.Tag{
			{Key: aws.String("qory:managed"), Value: aws.String("true")},
			{Key: aws.String("qory:pool"), Value: aws.String(pool)},
			{Key: aws.String("qory:key"), Value: aws.String(key)},
			{Key: aws.String("env"), Value: aws.String("test")},
			{Key: aws.String("qory:stack"), Value: aws.String("example-stack")},
		})
	}
}

// TestMakeTerminated pins make for a key whose machine is terminated: EC2 returns
// IdempotentInstanceTerminated, and make returns the id that DescribeInstances returns
// for the client token, or the error when EC2 no longer returns the machine.
func TestMakeTerminated(t *testing.T) {
	var found []types.Instance
	f := world("t3.medium", "m7i.large")
	f.run = func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
		return nil, apiError("IdempotentInstanceTerminated")
	}
	f.describe = func(in *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		if aws.ToString(in.Filters[0].Name) != "client-token" {
			return &ec2.DescribeInstancesOutput{}, nil
		}
		return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: found}}}, nil
	}
	h, _, _ := testHandler(f)
	path := writeSettings(t, validSettings)
	found = []types.Instance{launched(testID, "terminated", testKey)}
	status, out := serve(t, h, "make", path, smallRequest(testData))
	if status != adapterio.ExitOK || out != `{"id":"`+testID+`","machine_type":"t3.medium"}`+"\n" {
		t.Fatalf("exit status %d, output %q", status, out)
	}
	if want := []types.Filter{{Name: aws.String("client-token"), Values: []string{testKey + ".t3.medium"}}}; !reflect.DeepEqual(f.describes[1].Filters, want) {
		t.Errorf("filters %+v", f.describes[1].Filters)
	}

	found = nil
	status, out = serve(t, h, "make", path, smallRequest(testData))
	wantFailure(t, status, out, contracts.FailureOther, "No action needed: the machine made for this key is terminated, and EC2 makes no second machine for the key: api error IdempotentInstanceTerminated: test message for IdempotentInstanceTerminated")
}

// TestMakeErrorKinds pins the kind of each class of RunInstances error in the error
// document, and that no message contains the start data, its base64 or the run code
// in it.
func TestMakeErrorKinds(t *testing.T) {
	const runCode = "qecr_runcodemarker0c9d"
	data := []byte(`{"version": 1, "run_code": "` + runCode + `"}`)
	encoded := base64.StdEncoding.EncodeToString(data)
	cases := []struct {
		err  error
		kind string
	}{
		{apiError("InstanceLimitExceeded"), contracts.FailureLimit},
		{apiError("VcpuLimitExceeded"), contracts.FailureLimit},
		{apiError("InsufficientFreeAddressesInSubnet"), contracts.FailureLimit},
		{&retry.MaxAttemptsError{Attempt: 3, Err: apiError("InsufficientInstanceCapacity")}, contracts.FailureLimit},
		{apiError("RequestLimitExceeded"), contracts.FailureRetry},
		{&retry.MaxAttemptsError{Attempt: 3, Err: apiError("InternalError")}, contracts.FailureRetry},
		{context.DeadlineExceeded, contracts.FailureRetry},
		{apiError("UnauthorizedOperation"), contracts.FailureDefect},
		{apiError("InvalidParameterValue"), contracts.FailureDefect},
		{apiError("IdempotentParameterMismatch"), contracts.FailureDefect},
		{apiError("AuthFailure"), contracts.FailureOther},
		{errors.New("unexpected end of the answer"), contracts.FailureOther},
	}
	for _, tc := range cases {
		f := world()
		f.run = func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) { return nil, tc.err }
		h, log, _ := testHandler(f)
		status, out := serve(t, h, "make", writeSettings(t, validSettings), smallRequest(data))
		if status != adapterio.ExitFailure {
			t.Fatalf("%v: exit status %d", tc.err, status)
		}
		doc, err := adapterio.DecodeError([]byte(strings.TrimSuffix(out, "\n")))
		if err != nil || doc.Error.Kind != tc.kind {
			t.Errorf("%v: document %+v, error %v, want kind %s", tc.err, doc, err, tc.kind)
		}
		for _, s := range []string{out, log.String()} {
			if strings.Contains(s, runCode) || strings.Contains(s, encoded) {
				t.Errorf("%v: the output contains the start data: %s", tc.err, s)
			}
		}
	}
}

// TestList pins list: the tag and state filters, the state of each EC2 state,
// terminated left out, the key from the qory:key tag, the start time in UTC, and an
// instance whose qory:key tag is not a key left out with a log line.
func TestList(t *testing.T) {
	f := &fakeAPI{describe: func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{
			launched("i-0000000000000000a", "pending", "dp_000000000000000a"),
			launched("i-0000000000000000b", "running", "dp_000000000000000b"),
			launched("i-0000000000000000c", "stopping", "dp_000000000000000c"),
			launched("i-0000000000000000d", "stopped", "dp_000000000000000d"),
			launched("i-0000000000000000e", "shutting-down", "dp_000000000000000e"),
			launched("i-0000000000000000f", "terminated", "dp_000000000000000f"),
			launched("i-0000000000000001a", "running", "run-41"),
		}}}}, nil
	}}
	h, log, _ := testHandler(f)
	status, out := serve(t, h, "list", "/nonexistent/aws-ec2.json", `{"version": 1, "pool": "`+testPool+`"}`)
	if status != adapterio.ExitOK {
		t.Fatalf("exit status %d, output %s", status, out)
	}
	const started = `"started":"2026-10-22T10:00:00Z"`
	want := `{"machines":[` +
		`{"id":"i-0000000000000000a","key":"dp_000000000000000a","state":"starting",` + started + `},` +
		`{"id":"i-0000000000000000b","key":"dp_000000000000000b","state":"running",` + started + `},` +
		`{"id":"i-0000000000000000c","key":"dp_000000000000000c","state":"running",` + started + `},` +
		`{"id":"i-0000000000000000d","key":"dp_000000000000000d","state":"stopped",` + started + `},` +
		`{"id":"i-0000000000000000e","key":"dp_000000000000000e","state":"deleting",` + started + `}]}` + "\n"
	if out != want {
		t.Errorf("output\n%s\nwant\n%s", out, want)
	}
	wantFilters := []types.Filter{
		{Name: aws.String("tag:qory:managed"), Values: []string{"true"}},
		{Name: aws.String("tag:qory:pool"), Values: []string{testPool}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped", "shutting-down"}},
	}
	if !reflect.DeepEqual(f.describes[0].Filters, wantFilters) {
		t.Errorf("filters %+v", f.describes[0].Filters)
	}
	const line = `level=WARN msg="Set the instance's qory:key tag to the key of its make request, or remove its qory:managed tag: list leaves out an instance whose qory:key tag is not a key" instance=i-0000000000000001a`
	if !strings.Contains(log.String(), line) || strings.Count(log.String(), "\n") != 1 {
		t.Errorf("log\n%s\nwant the line\n%s", log, line)
	}

	f.describe = func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return &ec2.DescribeInstancesOutput{}, nil
	}
	if status, out := serve(t, h, "list", "/nonexistent/aws-ec2.json", `{"version": 1, "pool": "`+testPool+`"}`); status != adapterio.ExitOK || out != `{"machines":[]}`+"\n" {
		t.Errorf("no machine: exit status %d, output %q", status, out)
	}

	f.describe = func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return nil, apiError("UnauthorizedOperation")
	}
	status, out = serve(t, h, "list", "/nonexistent/aws-ec2.json", `{"version": 1, "pool": "`+testPool+`"}`)
	wantFailure(t, status, out, contracts.FailureDefect, "Give the instance role the EC2 rights that the EC2 machine adapter needs: DescribeInstances failed with UnauthorizedOperation: api error UnauthorizedOperation: test message for UnauthorizedOperation")
}

// TestListNoLaunchTime pins that an instance that EC2 returns with no launch time is
// left out of the list answer, with one log line, so the provisioner does not take
// its start as 0001-01-01.
func TestListNoLaunchTime(t *testing.T) {
	noTime := launched("i-0000000000000002a", "running", "dp_000000000000002a")
	noTime.LaunchTime = nil
	f := &fakeAPI{describe: func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{
			noTime,
			launched("i-0000000000000002b", "running", "dp_000000000000002b"),
		}}}}, nil
	}}
	h, log, _ := testHandler(f)
	status, out := serve(t, h, "list", "/nonexistent/aws-ec2.json", `{"version": 1, "pool": "`+testPool+`"}`)
	want := `{"machines":[{"id":"i-0000000000000002b","key":"dp_000000000000002b","state":"running","started":"2026-10-22T10:00:00Z"}]}` + "\n"
	if status != adapterio.ExitOK || out != want {
		t.Errorf("exit status %d, output\n%s\nwant\n%s", status, out, want)
	}
	const line = `level=WARN msg="Check the instance in EC2: list leaves out an instance that DescribeInstances returned with no launch time" instance=i-0000000000000002a`
	if !strings.Contains(log.String(), line) || strings.Count(log.String(), "\n") != 1 {
		t.Errorf("log\n%s\nwant the line\n%s", log, line)
	}
}

// TestLog pins log: GetConsoleOutput with Latest for the id, and the decoded text.
func TestLog(t *testing.T) {
	const text = "boot\r\nqory-machine-exit status=0\r\n"
	f := &fakeAPI{console: func(*ec2.GetConsoleOutputInput) (*ec2.GetConsoleOutputOutput, error) {
		return &ec2.GetConsoleOutputOutput{Output: aws.String(base64.StdEncoding.EncodeToString([]byte(text)))}, nil
	}}
	h, _, _ := testHandler(f)
	status, out := serve(t, h, "log", "/nonexistent/aws-ec2.json", `{"version": 1, "id": "`+testID+`"}`)
	a, err := adapterio.DecodeLogAnswer([]byte(strings.TrimSuffix(out, "\n")))
	if status != adapterio.ExitOK || err != nil || a.Log != text {
		t.Fatalf("exit status %d, output %q, error %v", status, out, err)
	}
	if want := (&ec2.GetConsoleOutputInput{InstanceId: aws.String(testID), Latest: aws.Bool(true)}); !reflect.DeepEqual(f.consoles[0], want) {
		t.Errorf("input %+v", f.consoles[0])
	}

	f.console = func(*ec2.GetConsoleOutputInput) (*ec2.GetConsoleOutputOutput, error) {
		return nil, apiError("InvalidInstanceID.NotFound")
	}
	status, out = serve(t, h, "log", "/nonexistent/aws-ec2.json", `{"version": 1, "id": "`+testID+`"}`)
	wantFailure(t, status, out, contracts.FailureOther, "Check that the instance exists in this region: GetConsoleOutput failed with InvalidInstanceID.NotFound, which EC2 can return right after RunInstances: api error InvalidInstanceID.NotFound: test message for InvalidInstanceID.NotFound")
}

// TestLogNotNitro pins that log returns the console text of an instance that is not
// on the Nitro system: GetConsoleOutput with Latest returns UnsupportedOperation, and
// log reads the output again without Latest.
func TestLogNotNitro(t *testing.T) {
	const text = "boot on a Xen instance\n"
	f := &fakeAPI{console: func(in *ec2.GetConsoleOutputInput) (*ec2.GetConsoleOutputOutput, error) {
		if aws.ToBool(in.Latest) {
			return nil, apiError("UnsupportedOperation")
		}
		return &ec2.GetConsoleOutputOutput{Output: aws.String(base64.StdEncoding.EncodeToString([]byte(text)))}, nil
	}}
	h, _, _ := testHandler(f)
	status, out := serve(t, h, "log", "/nonexistent/aws-ec2.json", `{"version": 1, "id": "`+testID+`"}`)
	a, err := adapterio.DecodeLogAnswer([]byte(strings.TrimSuffix(out, "\n")))
	if status != adapterio.ExitOK || err != nil || a.Log != text {
		t.Fatalf("exit status %d, output %q, error %v", status, out, err)
	}
	if len(f.consoles) != 2 || f.consoles[1].Latest != nil {
		t.Errorf("inputs %+v, want a second call without Latest", f.consoles)
	}
}

// TestLogCut pins that log keeps the end of a long console text that fits a log
// answer: U+0000 and each run of invalid UTF-8 become U+FFFD, the text is at most
// 768 KiB, and the answer, with its line feed, at most 1 MiB.
func TestLogCut(t *testing.T) {
	for i, c := range []struct {
		console, end string
		size         int
	}{
		{strings.Repeat("a\xff", adapterio.MaxLog/2), "a\uFFFD", adapterio.MaxLog},
		{strings.Repeat("boot\x00\x1b[0m\n", adapterio.MaxLog/4) + "end", "boot\uFFFD\x1b[0m\nend", 0},
	} {
		f := &fakeAPI{console: func(*ec2.GetConsoleOutputInput) (*ec2.GetConsoleOutputOutput, error) {
			return &ec2.GetConsoleOutputOutput{Output: aws.String(base64.StdEncoding.EncodeToString([]byte(c.console)))}, nil
		}}
		h, _, _ := testHandler(f)
		status, out := serve(t, h, "log", "/nonexistent/aws-ec2.json", `{"version": 1, "id": "`+testID+`"}`)
		a, err := adapterio.DecodeLogAnswer([]byte(strings.TrimSuffix(out, "\n")))
		if status != adapterio.ExitOK || err != nil || len(out) > contracts.MaxMachinesAnswer || len(a.Log) > adapterio.MaxLog || !strings.HasSuffix(a.Log, c.end) || strings.Contains(a.Log, "\x00") {
			t.Errorf("case %d: exit status %d, %d bytes of output, %d bytes of log, error %v", i, status, len(out), len(a.Log), err)
		}
		if c.size != 0 && len(a.Log) != c.size {
			t.Errorf("case %d: %d bytes of log, want %d", i, len(a.Log), c.size)
		}
		if c.size == 0 && len(out) < contracts.MaxMachinesAnswer-6 {
			t.Errorf("case %d: %d bytes of output, want the end that fits 1 MiB", i, len(out))
		}
	}
}

// TestDelete pins delete: TerminateInstances for the id; gone for an unknown id and a
// terminated instance, deleting for any other, and the kind of a failed call.
func TestDelete(t *testing.T) {
	var reply *ec2.TerminateInstancesOutput
	var replyErr error
	f := &fakeAPI{terminate: func(*ec2.TerminateInstancesInput) (*ec2.TerminateInstancesOutput, error) { return reply, replyErr }}
	h, _, _ := testHandler(f)
	change := func(previous, current types.InstanceStateName) *ec2.TerminateInstancesOutput {
		return &ec2.TerminateInstancesOutput{TerminatingInstances: []types.InstanceStateChange{{
			InstanceId:    aws.String(testID),
			PreviousState: &types.InstanceState{Name: previous},
			CurrentState:  &types.InstanceState{Name: current},
		}}}
	}
	cases := []struct {
		reply *ec2.TerminateInstancesOutput
		err   error
		state string
	}{
		{change(types.InstanceStateNameRunning, types.InstanceStateNameShuttingDown), nil, "deleting"},
		{change(types.InstanceStateNameStopped, types.InstanceStateNameShuttingDown), nil, "deleting"},
		{change(types.InstanceStateNameShuttingDown, types.InstanceStateNameShuttingDown), nil, "deleting"},
		{change(types.InstanceStateNameTerminated, types.InstanceStateNameTerminated), nil, "gone"},
		{&ec2.TerminateInstancesOutput{}, nil, "deleting"},
		{nil, apiError("InvalidInstanceID.NotFound"), "gone"},
	}
	for i, tc := range cases {
		reply, replyErr = tc.reply, tc.err
		status, out := serve(t, h, "delete", "/nonexistent/aws-ec2.json", `{"version": 1, "id": "`+testID+`"}`)
		if want := `{"state":"` + tc.state + `"}` + "\n"; status != adapterio.ExitOK || out != want {
			t.Errorf("case %d: exit status %d, output %q, want %q", i, status, out, want)
		}
	}
	if want := (&ec2.TerminateInstancesInput{InstanceIds: []string{testID}}); !reflect.DeepEqual(f.kills[0], want) {
		t.Errorf("input %+v", f.kills[0])
	}

	reply, replyErr = nil, apiError("RequestLimitExceeded")
	status, out := serve(t, h, "delete", "/nonexistent/aws-ec2.json", `{"version": 1, "id": "`+testID+`"}`)
	wantFailure(t, status, out, contracts.FailureRetry, "If this repeats, check EC2's service health and the account's request rate in this region: TerminateInstances failed with an error the SDK retries: api error RequestLimitExceeded: test message for RequestLimitExceeded")
}

// TestFailure pins the conversion of each kind of error to the error document.
func TestFailure(t *testing.T) {
	done := &adapterio.Failure{Kind: contracts.FailureLimit, Message: "m"}
	if got := failure(done); got != done {
		t.Errorf("a *adapterio.Failure: %#v", got)
	}
	var e *adapterio.Failure
	if !errors.As(failure(errors.New("plain")), &e) || e.Kind != contracts.FailureOther || e.Message != "plain" {
		t.Errorf("a plain error: %#v", e)
	}
}
