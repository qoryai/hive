package machine

import (
	"context"
	"fmt"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// codeSubnetNotFound is the EC2 error code of a subnet that does not exist.
const codeSubnetNotFound = "InvalidSubnetID.NotFound"

// InstanceType is what make and check read of one EC2 instance type. The adapter keeps
// it in its cache file as JSON, with these field names.
type InstanceType struct {
	// Name is the instance type, such as m7i.large.
	Name string `json:"name"`
	// VCPUs is the number of virtual CPUs.
	VCPUs int `json:"vcpus"`
	// MemoryMiB is the memory in MiB.
	MemoryMiB int `json:"memory_mib"`
	// Archs are the architectures the type runs, as the provisioner contract writes
	// them: [ArchAMD64] and [ArchARM64]. An EC2 architecture that the contract does not
	// have is left out.
	Archs []string `json:"archs"`
	// NestedVirtualization is true when the type's processor lists the feature
	// nested-virtualization, which RunInstances turns on with CpuOptions.
	NestedVirtualization bool `json:"nested_virtualization"`
	// BareMetal is true for a bare metal type, whose machine runs virtual machines
	// without CpuOptions.
	BareMetal bool `json:"bare_metal"`
	// OnDemand is true when the type lists the usage class on-demand. A type without
	// it is sold only in another way, such as a capacity block, and RunInstances
	// without a reservation fails for it.
	OnDemand bool `json:"on_demand"`
}

// Virtualization reports whether a machine of the type can run virtual machines
// inside it: the type is bare metal or has nested virtualization.
func (t InstanceType) Virtualization() bool {
	return t.BareMetal || t.NestedVirtualization
}

// NestedCPUOptions reports whether a machine of the type needs CpuOptions to run
// virtual machines: the type has nested virtualization and is not bare metal. A bare
// metal type runs them without CpuOptions, also when it lists the feature.
func (t InstanceType) NestedCPUOptions() bool {
	return t.NestedVirtualization && !t.BareMetal
}

// AllInstanceTypes returns every instance type of the region, with paginated
// DescribeInstanceTypes calls of 100 types each and no filter.
func (c *Client) AllInstanceTypes(ctx context.Context) ([]InstanceType, error) {
	var out []InstanceType
	p := ec2.NewDescribeInstanceTypesPaginator(c.api, &ec2.DescribeInstanceTypesInput{MaxResults: aws.Int32(100)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, wrap("DescribeInstanceTypes", err)
		}
		for _, info := range page.InstanceTypes {
			out = append(out, instanceTypeOf(info))
		}
	}
	return out, nil
}

// instanceTypeOf converts one EC2 instance type.
func instanceTypeOf(info types.InstanceTypeInfo) InstanceType {
	t := InstanceType{Name: string(info.InstanceType), BareMetal: aws.ToBool(info.BareMetal)}
	if info.VCpuInfo != nil {
		t.VCPUs = int(aws.ToInt32(info.VCpuInfo.DefaultVCpus))
	}
	if info.MemoryInfo != nil {
		t.MemoryMiB = int(aws.ToInt64(info.MemoryInfo.SizeInMiB))
	}
	if info.ProcessorInfo != nil {
		for _, a := range info.ProcessorInfo.SupportedArchitectures {
			if arch, ok := archOf(string(a)); ok && !slices.Contains(t.Archs, arch) {
				t.Archs = append(t.Archs, arch)
			}
		}
		t.NestedVirtualization = slices.Contains(info.ProcessorInfo.SupportedFeatures, types.SupportedAdditionalProcessorFeatureNestedVirtualization)
	}
	t.OnDemand = slices.Contains(info.SupportedUsageClasses, types.UsageClassTypeOnDemand)
	return t
}

// SubnetZone returns the availability zone of subnet subnetID, such as eu-west-1a,
// with one DescribeSubnets call for that subnet id.
func (c *Client) SubnetZone(ctx context.Context, subnetID string) (string, error) {
	out, err := c.api.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: []string{subnetID}})
	if errorCode(err) == codeSubnetNotFound {
		return "", defect(fmt.Sprintf("Set subnet_id in %s to a subnet that exists in %s: DescribeSubnets returned no subnet %s: %v", settingsFile, c.Region(), subnetID, err))
	}
	if err != nil {
		return "", wrap("DescribeSubnets", err)
	}
	for _, s := range out.Subnets {
		if aws.ToString(s.SubnetId) == subnetID && aws.ToString(s.AvailabilityZone) != "" {
			return aws.ToString(s.AvailabilityZone), nil
		}
	}
	return "", defect(fmt.Sprintf("Set subnet_id in %s to a subnet that exists in %s: DescribeSubnets returned no zone for subnet %s", settingsFile, c.Region(), subnetID))
}

// ZoneTypes returns the names of the instance types that zone offers, with paginated
// DescribeInstanceTypeOfferings calls of the location type availability-zone, filtered
// to zone, of 1000 offerings each.
func (c *Client) ZoneTypes(ctx context.Context, zone string) ([]string, error) {
	in := &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: types.LocationTypeAvailabilityZone,
		Filters:      []types.Filter{filter("location", zone)},
		MaxResults:   aws.Int32(1000),
	}
	var out []string
	seen := map[string]bool{}
	p := ec2.NewDescribeInstanceTypeOfferingsPaginator(c.api, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, wrap("DescribeInstanceTypeOfferings", err)
		}
		for _, o := range page.InstanceTypeOfferings {
			if name := string(o.InstanceType); aws.ToString(o.Location) == zone && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out, nil
}
