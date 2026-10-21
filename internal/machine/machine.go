// Package machine creates, lists, reads and terminates the EC2 instances that the
// provisioner starts for runs, one instance per run. It calls EC2 with the host's
// instance role, through the AWS SDK's default credential chain.
package machine

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// The tags on every machine and its volume. A machine has all three.
const (
	TagManaged = "qory:managed"
	TagPool    = "qory:pool"
	TagRun     = "qory:run"
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
}

// Client creates, lists, reads and terminates machines through an API.
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
// instance metadata. On the host, the credentials come from the instance role.
func LoadConfig(ctx context.Context) (aws.Config, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithEC2IMDSRegion())
	if err != nil {
		return aws.Config{}, fmt.Errorf("Set AWS_REGION, or run the provisioner on an EC2 instance with IMDSv2 reachable: loading the AWS configuration failed: %w", err)
	}
	if cfg.Region == "" {
		return aws.Config{}, fmt.Errorf("Set AWS_REGION: the environment has no region, and the instance metadata returned none")
	}
	return cfg, nil
}

// Machine is one EC2 instance as DescribeInstances or RunInstances returns it.
type Machine struct {
	// ID is the instance id, such as i-0123456789abcdef0.
	ID string
	// State is one of the State constants.
	State string
	// DispatchID is the instance's client token, which Create sets to the dispatch id.
	DispatchID string
	// PoolID and RunID are the values of the qory:pool and qory:run tags.
	PoolID, RunID string
	// LaunchTime is the time EC2 started the instance; zero when EC2 returned none.
	LaunchTime time.Time
}

// fromInstance converts one EC2 instance.
func fromInstance(in types.Instance) Machine {
	m := Machine{
		ID:         aws.ToString(in.InstanceId),
		DispatchID: aws.ToString(in.ClientToken),
		LaunchTime: aws.ToTime(in.LaunchTime),
	}
	if in.State != nil {
		m.State = string(in.State.Name)
	}
	for _, t := range in.Tags {
		switch aws.ToString(t.Key) {
		case TagPool:
			m.PoolID = aws.ToString(t.Value)
		case TagRun:
			m.RunID = aws.ToString(t.Value)
		}
	}
	return m
}
