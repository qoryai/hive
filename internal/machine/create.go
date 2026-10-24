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

// maxClientToken is the longest client token EC2 accepts, in ASCII characters.
const maxClientToken = 64

// Spec is the input for one machine.
type Spec struct {
	// Key is the key of the make request, sent as RunInstances' client token, so a
	// repeated call for one key makes no second machine: 1 to 64 ASCII letters, digits
	// and punctuation marks. It is also the value of the qory:key tag.
	Key string
	// PoolID is the value of the qory:pool tag.
	PoolID string
	// InstanceType is the EC2 instance type, such as m7g.large.
	InstanceType string
	// ImageID is the machine image, such as ami-0123456789abcdef0.
	ImageID string
	// RootDevice is the image's root device name, such as /dev/sda1, as
	// Client.RootDevice returns it. Create marks the volume on that device encrypted.
	RootDevice string
	// SubnetID and SecurityGroupID place the machine's network interface in the run
	// subnet.
	SubnetID, SecurityGroupID string
	// UserData is the raw user data, at most MaxUserData bytes. Create encodes it
	// with base64.
	UserData []byte
	// Tags maps more tag keys to their values. The machine, its volume and its
	// network interface get them after qory:managed, qory:pool and qory:key, sorted
	// by key, so a repeated Create sends the same input. EC2 accepts at most 50 tags
	// on a resource.
	Tags map[string]string
}

// Create makes one machine for spec and returns it as RunInstances returned it,
// usually in state pending. A repeated Create with the same spec returns the same
// machine, because the key is the client token. Errors from the call are *Error.
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
// sends it as the client token.
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
			Ebs:        &types.EbsBlockDevice{Encrypted: aws.Bool(true), DeleteOnTermination: aws.Bool(true)},
		}},
		// RunInstances makes the network interface too, and a condition on request
		// tags in the instance role's policy can cover it, so it gets the same tags.
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeInstance, Tags: tags},
			{ResourceType: types.ResourceTypeVolume, Tags: tags},
			{ResourceType: types.ResourceTypeNetworkInterface, Tags: tags},
		},
	}
	if withToken {
		in.ClientToken = aws.String(spec.Key)
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
		if err := validToken(spec.Key); err != nil {
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
	if len(spec.UserData) > MaxUserData {
		return defect(fmt.Sprintf("Report this defect in the provisioner: the start data has %d bytes, and EC2 accepts at most %d bytes of user data", len(spec.UserData), MaxUserData))
	}
	return nil
}

// validToken checks that key is a client token EC2 accepts.
func validToken(key string) error {
	if key == "" || len(key) > maxClientToken {
		return defect(fmt.Sprintf("Report this defect in the provisioner: the key has %d bytes, and a client token has 1 to %d characters", len(key), maxClientToken))
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return defect(fmt.Sprintf("Report this defect in the provisioner: byte %d of the key is 0x%02x, and a client token has only ASCII letters, digits and punctuation", i, key[i]))
		}
	}
	return nil
}
