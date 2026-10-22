// Package machine makes, lists, reads and terminates EC2 instances for the EC2 machine
// adapter, qory-machines-aws-ec2: one instance per key of a make request. It calls EC2
// at its standard endpoint with the host's instance role, whose credentials it reads
// from the instance metadata service; see [LoadConfig].
package machine

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
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

// The reasons of the refusals of [refusedVariables].
const (
	reasonCredentials = "the EC2 machine adapter uses only the instance role's credentials"
	reasonFiles       = "the EC2 machine adapter reads no AWS config or credentials file"
	reasonMetadata    = "the EC2 machine adapter reads the instance role's credentials from the instance metadata service at its standard address"
	reasonEndpoint    = "the EC2 machine adapter calls EC2 only at its standard endpoint, with the instance role's credentials"
	reasonCABundle    = "the EC2 machine adapter checks the EC2 endpoint's certificate only against the host's certificate authorities"
)

// refusedVariables are the AWS SDK's environment variables that select another
// credential source, an AWS config or credentials file, another address of the
// instance metadata service, another EC2 endpoint or another certificate authority.
// [LoadConfig] returns an error when any of them is set and not empty.
var refusedVariables = []struct{ name, reason string }{
	{"AWS_ACCESS_KEY_ID", reasonCredentials},
	{"AWS_ACCESS_KEY", reasonCredentials},
	{"AWS_SECRET_ACCESS_KEY", reasonCredentials},
	{"AWS_SECRET_KEY", reasonCredentials},
	{"AWS_SESSION_TOKEN", reasonCredentials},
	{"AWS_WEB_IDENTITY_TOKEN_FILE", reasonCredentials},
	{"AWS_ROLE_ARN", reasonCredentials},
	{"AWS_CONTAINER_CREDENTIALS_FULL_URI", reasonCredentials},
	{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", reasonCredentials},
	{"AWS_PROFILE", reasonFiles},
	{"AWS_DEFAULT_PROFILE", reasonFiles},
	{"AWS_CONFIG_FILE", reasonFiles},
	{"AWS_SHARED_CREDENTIALS_FILE", reasonFiles},
	{"AWS_EC2_METADATA_SERVICE_ENDPOINT", reasonMetadata},
	{"AWS_EC2_METADATA_DISABLED", reasonMetadata},
	{"AWS_ENDPOINT_URL", reasonEndpoint},
	{"AWS_ENDPOINT_URL_EC2", reasonEndpoint},
	{"AWS_CA_BUNDLE", reasonCABundle},
}

// imdsEndpoint is the address of the instance metadata service. "" is the SDK's
// standard address: http://169.254.169.254, or http://[fd00:ec2::254] when
// AWS_EC2_METADATA_SERVICE_ENDPOINT_MODE is IPv6. Tests set a local server.
var imdsEndpoint string

// LoadConfig loads the AWS configuration of the EC2 machine adapter. Its errors are
// *adapterio.Failure.
//   - When a variable of [refusedVariables] is set, it returns an error of the kind
//     defect before any call.
//   - It reads no shared config or credentials file, so no profile applies.
//   - The region comes from AWS_REGION, or else from the instance metadata.
//     AWS_DEFAULT_REGION does not apply.
//   - The credentials come only from the instance role, which the SDK's role provider
//     reads from the instance metadata service at its standard address, with no HTTP
//     proxy.
//   - The configuration has no EC2 endpoint, so the EC2 client calls the standard
//     endpoint of the region.
//   - The SDK's defaults mode is legacy, the SDK's default when AWS_DEFAULTS_MODE is
//     not set. AWS_DEFAULTS_MODE does not apply: its value auto makes the SDK read the
//     region from the instance metadata with a client that uses HTTP_PROXY.
//
// LoadConfig retrieves the credentials once, so a host with no instance role fails
// here.
func LoadConfig(ctx context.Context) (aws.Config, error) {
	for _, v := range refusedVariables {
		if os.Getenv(v.name) != "" {
			return aws.Config{}, &adapterio.Failure{Kind: contracts.FailureDefect, Message: fmt.Sprintf("Unset %s: %s", v.name, v.reason)}
		}
	}
	env, err := config.NewEnvConfig()
	if err != nil {
		return aws.Config{}, &adapterio.Failure{Kind: contracts.FailureOther, Message: fmt.Sprintf("Fix the AWS variables in the EC2 machine adapter's environment: reading them failed: %v", err)}
	}
	meta := metadataClient(env)
	region := os.Getenv("AWS_REGION")
	if region == "" {
		out, err := meta.GetRegion(ctx, nil)
		if err != nil {
			return aws.Config{}, &adapterio.Failure{Kind: contracts.FailureOther, Message: fmt.Sprintf("Set AWS_REGION, or run the EC2 machine adapter on an EC2 instance with IMDSv2 reachable: loading the AWS configuration failed: %v", err)}
		}
		region = out.Region
	}
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithSharedConfigFiles([]string{}),
		config.WithSharedCredentialsFiles([]string{}),
		config.WithRegion(region),
		config.WithDefaultsMode(aws.DefaultsModeLegacy),
		config.WithCredentialsProvider(ec2rolecreds.New(func(o *ec2rolecreds.Options) { o.Client = meta })),
	)
	if err != nil {
		return aws.Config{}, &adapterio.Failure{Kind: contracts.FailureOther, Message: fmt.Sprintf("Fix the AWS variables in the EC2 machine adapter's environment: loading the AWS configuration failed: %v", err)}
	}
	if cfg.Region == "" {
		return aws.Config{}, &adapterio.Failure{Kind: contracts.FailureDefect, Message: "Set AWS_REGION: the environment has no region, and the instance metadata returned none"}
	}
	if cfg.BaseEndpoint != nil {
		return aws.Config{}, &adapterio.Failure{Kind: contracts.FailureDefect, Message: "Report this defect in the EC2 machine adapter: its AWS configuration has an EC2 endpoint, and the adapter calls EC2 only at its standard endpoint"}
	}
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return aws.Config{}, &adapterio.Failure{Kind: contracts.FailureOther, Message: fmt.Sprintf("Attach an instance role to the host, and run the EC2 machine adapter with IMDSv2 reachable: retrieving the AWS credentials failed: %v", err)}
	}
	if creds.Source != ec2rolecreds.ProviderName {
		return aws.Config{}, &adapterio.Failure{Kind: contracts.FailureDefect, Message: fmt.Sprintf("Report this defect in the EC2 machine adapter: its AWS credentials came from %s, not %s", creds.Source, ec2rolecreds.ProviderName)}
	}
	return cfg, nil
}

// metadataClient is the client of the instance metadata service at [imdsEndpoint],
// with the endpoint mode and the IMDSv1 setting of env. Its HTTP client uses no
// proxy: the service is reachable only from the host, and with HTTP_PROXY set the
// SDK's client would send the plain HTTP requests for the credentials to the proxy.
func metadataClient(env config.EnvConfig) *imds.Client {
	return imds.NewFromConfig(aws.Config{ConfigSources: []any{env}}, func(o *imds.Options) {
		o.Endpoint = imdsEndpoint
		o.HTTPClient = awshttp.NewBuildableClient().WithTransportOptions(func(tr *http.Transport) { tr.Proxy = nil })
	})
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
