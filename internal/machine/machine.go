// Package machine makes, lists, reads and terminates EC2 instances for the EC2 machine
// adapter, qory-machines-aws-ec2: one instance per key of a make request. It calls EC2
// with the host's instance role, through the AWS SDK's default credential chain.
package machine

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/integrations/contracts"
)

// The tags on every machine and its volume. A machine has all three.
const (
	TagManaged = "qory:managed"
	TagPool    = "qory:pool"
	TagKey     = "qory:key"
)

// The machine states that EC2 reports, as Machine.State has them.
const (
	StatePending      = string(types.InstanceStateNamePending)
	StateRunning      = string(types.InstanceStateNameRunning)
	StateStopping     = string(types.InstanceStateNameStopping)
	StateStopped      = string(types.InstanceStateNameStopped)
	StateShuttingDown = string(types.InstanceStateNameShuttingDown)
	StateTerminated   = string(types.InstanceStateNameTerminated)
)

// API is the part of the EC2 client that this package calls. *ec2.Client implements
// it; tests use a fake.
type API interface {
	RunInstances(ctx context.Context, in *ec2.RunInstancesInput, optFns ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	GetConsoleOutput(ctx context.Context, in *ec2.GetConsoleOutputInput, optFns ...func(*ec2.Options)) (*ec2.GetConsoleOutputOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	DescribeImages(ctx context.Context, in *ec2.DescribeImagesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
}

// Client makes, lists, reads and terminates machines through an API.
type Client struct {
	api API
}

// New returns a Client that calls api.
func New(api API) *Client {
	return &Client{api: api}
}

// NewFromConfig returns a Client that calls EC2 with the SDK's client built from cfg.
func NewFromConfig(cfg aws.Config) *Client {
	return New(ec2.NewFromConfig(cfg))
}

// LoadConfig loads the AWS configuration from the SDK's default chain. The region
// comes from AWS_REGION or AWS_DEFAULT_REGION, then the shared config file, then the
// instance metadata. LoadConfig then retrieves the credentials once, and returns an
// error unless they come from the host's instance role: the adapter uses no stored AWS
// key. Its errors are *adapterio.Failure.
func LoadConfig(ctx context.Context) (aws.Config, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithEC2IMDSRegion())
	if err != nil {
		return aws.Config{}, &adapterio.Failure{Kind: contracts.FailureOther, Message: fmt.Sprintf("Set AWS_REGION, or run the EC2 machine adapter on an EC2 instance with IMDSv2 reachable: loading the AWS configuration failed: %v", err)}
	}
	if cfg.Region == "" {
		return aws.Config{}, &adapterio.Failure{Kind: contracts.FailureDefect, Message: "Set AWS_REGION: the environment has no region, and the instance metadata returned none"}
	}
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return aws.Config{}, &adapterio.Failure{Kind: contracts.FailureOther, Message: fmt.Sprintf("Attach an instance role to the host, and run the EC2 machine adapter with IMDSv2 reachable: retrieving the AWS credentials failed: %v", err)}
	}
	if creds.Source != ec2rolecreds.ProviderName {
		return aws.Config{}, &adapterio.Failure{Kind: contracts.FailureDefect, Message: fmt.Sprintf("Remove AWS_ACCESS_KEY_ID, AWS_PROFILE and any ~/.aws folder from the EC2 machine adapter's environment, so the AWS SDK uses the instance role: the credentials came from %s, not %s", creds.Source, ec2rolecreds.ProviderName)}
	}
	return cfg, nil
}

// Machine is one EC2 instance as DescribeInstances or RunInstances returns it.
type Machine struct {
	// ID is the instance id, such as i-0123456789abcdef0.
	ID string
	// State is one of the State constants.
	State string
	// Key is the value of the qory:key tag.
	Key string
	// LaunchTime is the time EC2 started the instance; zero when EC2 returned none.
	LaunchTime time.Time
}

// fromInstance converts one EC2 instance.
func fromInstance(in types.Instance) Machine {
	m := Machine{
		ID:         aws.ToString(in.InstanceId),
		LaunchTime: aws.ToTime(in.LaunchTime),
	}
	if in.State != nil {
		m.State = string(in.State.Name)
	}
	for _, t := range in.Tags {
		if aws.ToString(t.Key) == TagKey {
			m.Key = aws.ToString(t.Value)
		}
	}
	return m
}
