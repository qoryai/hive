package machine

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// MaxUserData is the largest user data EC2 accepts, in bytes before base64.
const MaxUserData = 16 * 1024

// MaxRootGiB is the largest gp3 volume EC2 makes, in GiB.
const MaxRootGiB = 16384

// maxClientToken is the longest client token EC2 accepts, in ASCII characters.
const maxClientToken = 64

// Spec is the input for one machine.
type Spec struct {
	// Key is the key of the make request, the value of the qory:key tag. Create sends
	// [ClientToken] of Key and InstanceType as RunInstances' client token, so a
	// repeated call for one key and one instance type makes no second machine.
	Key string
	// PoolID is the value of the qory:pool tag.
	PoolID string
	// InstanceType is the EC2 instance type, such as m7g.large.
	InstanceType string
	// ImageID is the machine image, such as ami-0123456789abcdef0.
	ImageID string
	// RootDevice is the image's root device name, such as /dev/sda1, as
	// Client.Image returns it. Create sets the volume on that device to RootSizeGiB,
	// gp3 and encryption.
	RootDevice string
	// RootSizeGiB is the size of the root volume in GiB, at least 1.
	RootSizeGiB int
	// NestedVirtualization turns on nested virtualization with CpuOptions, for an
	// instance type that has it.
	NestedVirtualization bool
	// SubnetID and SecurityGroupID place the machine's network interface in the run
	// subnet.
	SubnetID, SecurityGroupID string
	// UserData is the raw user data, at most MaxUserData bytes. Create encodes it
	// with base64.
	UserData []byte
	// Tags maps more tag keys to their values. The machine and its volume get them
	// after qory:managed, qory:pool and qory:key, sorted by key, so a repeated Create
	// sends the same input. EC2 accepts at most 50 tags
	// on a resource.
	Tags map[string]string
}

// ClientToken is the client token of RunInstances for the key and the instance type:
// key, a dot and the instance type. The make of one key sends one token per instance
// type that it tries, so a try of the next type after a failed one is a new request.
func ClientToken(key, instanceType string) string {
	return key + "." + instanceType
}

// Create makes one machine for spec and returns it as RunInstances returned it,
// usually in state pending. A repeated Create with the same spec returns the same
// machine, because the client token is [ClientToken] of the key and the instance type.
// Errors from the call are *Error.
func (c *Client) Create(ctx context.Context, spec Spec) (Machine, error) {
	in, err := runInput(spec, true)
	if err != nil {
		return Machine{}, err
	}
	out, err := c.api.RunInstances(ctx, in)
	if err != nil {
		return Machine{}, wrap("RunInstances", err)
	}
	if len(out.Instances) != 1 {
		return Machine{}, other(fmt.Sprintf("If this repeats, check EC2's service health in this region: RunInstances for key %s returned %d instances, not 1", spec.Key, len(out.Instances)))
	}
	return fromInstance(out.Instances[0]), nil
}

// CheckRights sends RunInstances with DryRun set and the input Create sends for spec,
// so EC2 checks the instance role's rights and makes no machine. It returns nil when
// EC2 returns DryRunOperation, which means the rights are there. The dry run sends
// spec.Key only as the qory:key tag; the SDK sets a random client token.
func (c *Client) CheckRights(ctx context.Context, spec Spec) error {
	in, err := runInput(spec, false)
	if err != nil {
		return err
	}
	in.ClientToken = nil
	in.DryRun = aws.Bool(true)
	_, err = c.api.RunInstances(ctx, in)
	switch {
	case err == nil:
		return other("Check the EC2 endpoint: a RunInstances dry run returned success, and EC2 returns DryRunOperation for a dry run")
	case errorCode(err) == codeDryRun:
		return nil
	}
	return wrap("RunInstances dry run", err)
}

// runInput builds the RunInstances input for spec. withToken requires the key and
// sends [ClientToken] of the key and the instance type as the client token.
func runInput(spec Spec, withToken bool) (*ec2.RunInstancesInput, error) {
	if err := validate(spec, withToken); err != nil {
		return nil, err
	}
	tags := []types.Tag{
		{Key: aws.String(TagManaged), Value: aws.String("true")},
		{Key: aws.String(TagPool), Value: aws.String(spec.PoolID)},
		{Key: aws.String(TagKey), Value: aws.String(spec.Key)},
	}
	for _, k := range slices.Sorted(maps.Keys(spec.Tags)) {
		tags = append(tags, types.Tag{Key: aws.String(k), Value: aws.String(spec.Tags[k])})
	}
	in := &ec2.RunInstancesInput{
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		ImageId:      aws.String(spec.ImageID),
		InstanceType: types.InstanceType(spec.InstanceType),
		// The subnet and the security group are on the network interface, because
		// EC2 accepts AssociatePublicIpAddress only there.
		NetworkInterfaces: []types.InstanceNetworkInterfaceSpecification{{
			DeviceIndex:              aws.Int32(0),
			AssociatePublicIpAddress: aws.Bool(false),
			SubnetId:                 aws.String(spec.SubnetID),
			Groups:                   []string{spec.SecurityGroupID},
		}},
		// stop, not terminate: EC2 keeps the console output of a stopped machine, so
		// the log command reads it after the machine powers off.
		InstanceInitiatedShutdownBehavior: types.ShutdownBehaviorStop,
		MetadataOptions: &types.InstanceMetadataOptionsRequest{
			HttpTokens:              types.HttpTokensStateRequired,
			HttpPutResponseHopLimit: aws.Int32(1),
			HttpEndpoint:            types.InstanceMetadataEndpointStateEnabled,
		},
		// The root volume is deleted when the machine is terminated, whatever the
		// image's mapping sets for it.
		BlockDeviceMappings: []types.BlockDeviceMapping{{
			DeviceName: aws.String(spec.RootDevice),
			Ebs: &types.EbsBlockDevice{
				Encrypted:           aws.Bool(true),
				DeleteOnTermination: aws.Bool(true),
				VolumeSize:          aws.Int32(int32(spec.RootSizeGiB)),
				VolumeType:          types.VolumeTypeGp3,
			},
		}},
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeInstance, Tags: tags},
			{ResourceType: types.ResourceTypeVolume, Tags: tags},
		},
	}
	if withToken {
		in.ClientToken = aws.String(ClientToken(spec.Key, spec.InstanceType))
	}
	if spec.NestedVirtualization {
		in.CpuOptions = &types.CpuOptionsRequest{NestedVirtualization: types.NestedVirtualizationSpecificationEnabled}
	}
	if len(spec.UserData) > 0 {
		// The SDK sends UserData as given; EC2 requires base64.
		in.UserData = aws.String(base64.StdEncoding.EncodeToString(spec.UserData))
	}
	return in, nil
}

// validate checks spec before any call.
func validate(spec Spec, withToken bool) error {
	if withToken {
		if err := validToken(ClientToken(spec.Key, spec.InstanceType)); err != nil {
			return err
		}
	}
	for _, f := range []struct{ name, value string }{
		{"key", spec.Key},
		{"pool id", spec.PoolID},
		{"instance type", spec.InstanceType},
		{"image id", spec.ImageID},
		{"root device name", spec.RootDevice},
		{"subnet id", spec.SubnetID},
		{"security group id", spec.SecurityGroupID},
	} {
		if f.value == "" {
			return defect(fmt.Sprintf("Fix the EC2 machine adapter: the machine spec has no %s", f.name))
		}
	}
	if spec.RootSizeGiB < 1 || spec.RootSizeGiB > MaxRootGiB {
		return defect(fmt.Sprintf("Fix the EC2 machine adapter: the root volume size is %d GiB, and a gp3 volume has 1 to %d GiB", spec.RootSizeGiB, MaxRootGiB))
	}
	if len(spec.UserData) > MaxUserData {
		return defect(fmt.Sprintf("Report this defect in the provisioner: the start data has %d bytes, and EC2 accepts at most %d bytes of user data", len(spec.UserData), MaxUserData))
	}
	return nil
}

// validToken checks that token is a client token EC2 accepts.
func validToken(token string) error {
	if token == "" || len(token) > maxClientToken {
		return defect(fmt.Sprintf("Report this defect in the provisioner: the client token of the key has %d bytes, and a client token has 1 to %d characters", len(token), maxClientToken))
	}
	for i := 0; i < len(token); i++ {
		if token[i] < 0x21 || token[i] > 0x7e {
			return defect(fmt.Sprintf("Report this defect in the provisioner: byte %d of the client token of the key is 0x%02x, and a client token has only ASCII letters, digits and punctuation", i, token[i]))
		}
	}
	return nil
}
