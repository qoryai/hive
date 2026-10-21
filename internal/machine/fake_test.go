package machine

import (
	"context"
	"sync"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/smithy-go"
)

// fakeAPI records each input and returns the outputs and errors its funcs return. A
// nil func returns an empty output.
type fakeAPI struct {
	mu        sync.Mutex
	runs      []*ec2.RunInstancesInput
	describes []*ec2.DescribeInstancesInput
	consoles  []*ec2.GetConsoleOutputInput
	kills     []*ec2.TerminateInstancesInput
	images    []*ec2.DescribeImagesInput

	run       func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error)
	describe  func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error)
	console   func(*ec2.GetConsoleOutputInput) (*ec2.GetConsoleOutputOutput, error)
	terminate func(*ec2.TerminateInstancesInput) (*ec2.TerminateInstancesOutput, error)
	image     func(*ec2.DescribeImagesInput) (*ec2.DescribeImagesOutput, error)
}

var _ API = (*fakeAPI)(nil)
var _ API = (*ec2.Client)(nil)

func (f *fakeAPI) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.mu.Lock()
	f.runs = append(f.runs, in)
	f.mu.Unlock()
	if f.run == nil {
		return &ec2.RunInstancesOutput{}, nil
	}
	return f.run(in)
}

func (f *fakeAPI) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.mu.Lock()
	f.describes = append(f.describes, in)
	f.mu.Unlock()
	if f.describe == nil {
		return &ec2.DescribeInstancesOutput{}, nil
	}
	return f.describe(in)
}

func (f *fakeAPI) GetConsoleOutput(_ context.Context, in *ec2.GetConsoleOutputInput, _ ...func(*ec2.Options)) (*ec2.GetConsoleOutputOutput, error) {
	f.mu.Lock()
	f.consoles = append(f.consoles, in)
	f.mu.Unlock()
	if f.console == nil {
		return &ec2.GetConsoleOutputOutput{}, nil
	}
	return f.console(in)
}

func (f *fakeAPI) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.mu.Lock()
	f.kills = append(f.kills, in)
	f.mu.Unlock()
	if f.terminate == nil {
		return &ec2.TerminateInstancesOutput{}, nil
	}
	return f.terminate(in)
}

func (f *fakeAPI) DescribeImages(_ context.Context, in *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	f.mu.Lock()
	f.images = append(f.images, in)
	f.mu.Unlock()
	if f.image == nil {
		return &ec2.DescribeImagesOutput{}, nil
	}
	return f.image(in)
}

// apiError returns an EC2 error with code.
func apiError(code string) error {
	return &smithy.GenericAPIError{Code: code, Message: "test message for " + code}
}

// testSpec returns a valid spec with neutral example values.
func testSpec() Spec {
	return Spec{
		DispatchID:      "dsp-0f3a9c2e-1b7d-4e55-8a10-6c2d9e4b7a01",
		RunID:           "run-41",
		PoolID:          "pool-7",
		InstanceType:    "m7g.large",
		ImageID:         "ami-0123456789abcdef0",
		RootDevice:      "/dev/sda1",
		SubnetID:        "subnet-0123456789abcdef0",
		SecurityGroupID: "sg-0123456789abcdef0",
		UserData:        []byte(`{"version": 1, "run_id": "run-41"}`),
	}
}
