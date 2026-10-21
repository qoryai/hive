package machine

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

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
	// DispatchID is sent as RunInstances' client token, so a repeated call for one
	// dispatch starts no second machine: 1 to 64 ASCII letters, digits and
	// punctuation marks.
	DispatchID string
	// RunID and PoolID are the values of the qory:run and qory:pool tags.
	RunID, PoolID string
	// InstanceType is the EC2 instance type, such as m7g.large.
	InstanceType string
	// ImageID is the machine image, such as ami-0123456789abcdef0.
	ImageID string
	// RootDevice is the image's root device name, such as /dev/sda1, as
	// Client.RootDevice returns it. Create marks the volume on that device encrypted.
	RootDevice string
	// SubnetID and SecurityGroupID place the machine in the run subnet.
	SubnetID, SecurityGroupID string
	// UserData is the raw user data, at most MaxUserData bytes. Create encodes it
	// with base64.
	UserData []byte
}

// Create starts one machine for spec and returns it as RunInstances returned it,
// usually in state pending. A repeated Create with the same spec returns the same
// machine, because the dispatch id is the client token. Errors from the call are
// *Error; Classify returns their kind.
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
		return Machine{}, fmt.Errorf("If this repeats, check EC2's service health in this region: RunInstances for client token %s returned %d instances, not 1", spec.DispatchID, len(out.Instances))
	}
	return fromInstance(out.Instances[0]), nil
}

// CheckRights sends RunInstances with DryRun set and the input Create sends for spec,
// so EC2 checks the instance role's rights and starts no machine. It returns nil when
// EC2 returns DryRunOperation, which means the rights are there. The dry run sends no
// client token of spec's: the SDK sets a random one, and spec.DispatchID may be
// empty.
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
		return errors.New("Check the EC2 endpoint: a RunInstances dry run returned success, and EC2 returns DryRunOperation for a dry run")
	case errorCode(err) == codeDryRun:
		return nil
	case errorCode(err) == codeUnauthorized:
		return &Error{Op: "RunInstances dry run", Kind: KindOther, Code: codeUnauthorized, Err: err}
	}
	return wrap("RunInstances dry run", err)
}

// runInput builds the RunInstances input for spec. withToken requires the dispatch id
// and sends it as the client token.
func runInput(spec Spec, withToken bool) (*ec2.RunInstancesInput, error) {
	if err := validate(spec, withToken); err != nil {
		return nil, err
	}
	tags := []types.Tag{
		{Key: aws.String(TagManaged), Value: aws.String("true")},
		{Key: aws.String(TagPool), Value: aws.String(spec.PoolID)},
		{Key: aws.String(TagRun), Value: aws.String(spec.RunID)},
	}
	in := &ec2.RunInstancesInput{
		MinCount:         aws.Int32(1),
		MaxCount:         aws.Int32(1),
		ImageId:          aws.String(spec.ImageID),
		InstanceType:     types.InstanceType(spec.InstanceType),
		SubnetId:         aws.String(spec.SubnetID),
		SecurityGroupIds: []string{spec.SecurityGroupID},
		// stop, not terminate: the provisioner reads the console output of a stopped
		// machine before it terminates it.
		InstanceInitiatedShutdownBehavior: types.ShutdownBehaviorStop,
		MetadataOptions: &types.InstanceMetadataOptionsRequest{
			HttpTokens:              types.HttpTokensStateRequired,
			HttpPutResponseHopLimit: aws.Int32(1),
			HttpEndpoint:            types.InstanceMetadataEndpointStateEnabled,
		},
		BlockDeviceMappings: []types.BlockDeviceMapping{{
			DeviceName: aws.String(spec.RootDevice),
			Ebs:        &types.EbsBlockDevice{Encrypted: aws.Bool(true)},
		}},
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeInstance, Tags: tags},
			{ResourceType: types.ResourceTypeVolume, Tags: tags},
		},
	}
	if withToken {
		in.ClientToken = aws.String(spec.DispatchID)
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
		if err := validToken(spec.DispatchID); err != nil {
			return err
		}
	}
	for _, f := range []struct{ name, value string }{
		{"the run id", spec.RunID},
		{"the pool id", spec.PoolID},
		{"the instance type", spec.InstanceType},
		{"the image id", spec.ImageID},
		{"the root device name", spec.RootDevice},
		{"the subnet id", spec.SubnetID},
		{"the security group id", spec.SecurityGroupID},
	} {
		if f.value == "" {
			return fmt.Errorf("Report this defect in the provisioner: the machine spec has no %s", f.name)
		}
	}
	if len(spec.UserData) > MaxUserData {
		return fmt.Errorf("Report this defect in the provisioner: the user data has %d bytes, and EC2 accepts at most %d", len(spec.UserData), MaxUserData)
	}
	return nil
}

// validToken checks that id is a client token EC2 accepts.
func validToken(id string) error {
	if id == "" || len(id) > maxClientToken {
		return fmt.Errorf("Report this defect in the provisioner: the dispatch id has %d bytes, and a client token has 1 to %d characters", len(id), maxClientToken)
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return fmt.Errorf("Report this defect in the provisioner: byte %d of the dispatch id is 0x%02x, and a client token has only ASCII letters, digits and punctuation", i, id[i])
		}
	}
	return nil
}
