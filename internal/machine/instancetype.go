package machine

import (
	"context"
	"fmt"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// The EC2 error codes of an instance type or a subnet that does not exist.
const (
	codeInstanceTypeInvalid = "InvalidInstanceType"
	codeSubnetNotFound      = "InvalidSubnetID.NotFound"
)

// InstanceType is what make and check read of one EC2 instance type.
type InstanceType struct {
	// Name is the instance type, such as m7i.large.
	Name string
	// VCPUs is the number of virtual CPUs.
	VCPUs int
	// MemoryMiB is the memory in MiB.
	MemoryMiB int
	// Archs are the architectures the type runs, as the provisioner contract writes
	// them: [ArchAMD64] and [ArchARM64]. An EC2 architecture that the contract does not
	// have is left out.
	Archs []string
	// NestedVirtualization is true when the type's processor lists the feature
	// nested-virtualization, which RunInstances turns on with CpuOptions.
	NestedVirtualization bool
	// BareMetal is true for a bare metal type, whose machine runs virtual machines
	// without CpuOptions.
	BareMetal bool
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

// InstanceTypes returns each instance type of names, by name, with paginated
// DescribeInstanceTypes calls for those names. A name that EC2 does not know fails
// with the kind defect, and the error contains the name and the region.
func (c *Client) InstanceTypes(ctx context.Context, names []string) (map[string]InstanceType, error) {
	in := &ec2.DescribeInstanceTypesInput{}
	for _, n := range names {
		in.InstanceTypes = append(in.InstanceTypes, types.InstanceType(n))
	}
	out := make(map[string]InstanceType, len(names))
	p := ec2.NewDescribeInstanceTypesPaginator(c.api, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if errorCode(err) == codeInstanceTypeInvalid {
			return nil, defect(fmt.Sprintf("Set instance_types in %s to EC2 instance types that exist in %s: DescribeInstanceTypes failed: %v", settingsFile, c.Region(), err))
		}
		if err != nil {
			return nil, wrap("DescribeInstanceTypes", err)
		}
		for _, info := range page.InstanceTypes {
			t := instanceTypeOf(info)
			out[t.Name] = t
		}
	}
	for _, n := range names {
		if _, ok := out[n]; !ok {
			return nil, defect(fmt.Sprintf("Remove %s from instance_types in %s: DescribeInstanceTypes returned no such type in %s", n, settingsFile, c.Region()))
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

// Offered returns the names of the instance types of names that zone offers, with
// paginated DescribeInstanceTypeOfferings calls of the location type
// availability-zone, filtered to zone and to names.
func (c *Client) Offered(ctx context.Context, zone string, names []string) (map[string]bool, error) {
	in := &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: types.LocationTypeAvailabilityZone,
		Filters:      []types.Filter{filter("location", zone), filter("instance-type", names...)},
	}
	out := map[string]bool{}
	p := ec2.NewDescribeInstanceTypeOfferingsPaginator(c.api, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, wrap("DescribeInstanceTypeOfferings", err)
		}
		for _, o := range page.InstanceTypeOfferings {
			if aws.ToString(o.Location) == zone {
				out[string(o.InstanceType)] = true
			}
		}
	}
	return out, nil
}
