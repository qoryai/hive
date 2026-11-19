package machine

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// pageSize is DescribeInstances' MaxResults for a filtered call, the largest EC2
// accepts.
const pageSize = 1000

// FindByToken returns the machine whose client token is token, and false when EC2 has
// none. A terminated machine stays visible for about one hour; when EC2 returns more
// than one machine, FindByToken returns one that is not terminated.
func (c *Client) FindByToken(ctx context.Context, token string) (Machine, bool, error) {
	ms, err := c.describe(ctx, []types.Filter{filter("client-token", token)})
	if err != nil {
		return Machine{}, false, err
	}
	return pick(ms)
}

// FindByKey returns the machine of the pool poolID whose qory:key tag is key, in any
// state, and false when EC2 has none. A terminated machine stays visible for about
// one hour; when EC2 returns more than one machine, FindByKey returns one that is not
// terminated. The tags are set at launch, so it also finds a machine whose client
// token is of another instance type than the one tried first.
func (c *Client) FindByKey(ctx context.Context, poolID, key string) (Machine, bool, error) {
	ms, err := c.describe(ctx, []types.Filter{filter("tag:"+TagManaged, "true"), filter("tag:"+TagPool, poolID), filter("tag:"+TagKey, key)})
	if err != nil {
		return Machine{}, false, err
	}
	return pick(ms)
}

// pick returns a machine of ms that is not terminated, or else the first one, and
// false when ms is empty.
func pick(ms []Machine) (Machine, bool, error) {
	if len(ms) == 0 {
		return Machine{}, false, nil
	}
	for _, m := range ms {
		if m.State != StateTerminated {
			return m, true, nil
		}
	}
	return ms[0], true, nil
}

// List returns the machines with the tags qory:managed=true and qory:pool=poolID in
// any of states, or in every state when states is empty, with one paginated
// DescribeInstances for all of them.
func (c *Client) List(ctx context.Context, poolID string, states ...string) ([]Machine, error) {
	fs := []types.Filter{filter("tag:"+TagManaged, "true"), filter("tag:"+TagPool, poolID)}
	if len(states) > 0 {
		fs = append(fs, filter("instance-state-name", states...))
	}
	return c.describe(ctx, fs)
}

// describe returns every machine that matches filters, page by page.
func (c *Client) describe(ctx context.Context, filters []types.Filter) ([]Machine, error) {
	in := &ec2.DescribeInstancesInput{Filters: filters, MaxResults: aws.Int32(pageSize)}
	var ms []Machine
	p := ec2.NewDescribeInstancesPaginator(c.api, in)
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return nil, wrap("DescribeInstances", err)
		}
		for _, r := range out.Reservations {
			for _, i := range r.Instances {
				ms = append(ms, fromInstance(i))
			}
		}
	}
	return ms, nil
}

// Terminate sends TerminateInstances for the one instance id, and returns the
// instance's state after the call, as TerminateInstances returns it, or "" when the
// answer has no state for the id. It returns false when EC2 returns
// InvalidInstanceID.NotFound: EC2 has no instance with the id, or the instance was
// terminated long enough ago that EC2 no longer returns it. EC2 accepts the call again
// for an instance that is already terminated.
func (c *Client) Terminate(ctx context.Context, id string) (string, bool, error) {
	out, err := c.api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}})
	if errorCode(err) == codeInstanceIDNotFound {
		return "", false, nil
	}
	if err != nil {
		return "", false, wrap("TerminateInstances", err)
	}
	for _, change := range out.TerminatingInstances {
		if aws.ToString(change.InstanceId) == id && change.CurrentState != nil {
			return string(change.CurrentState.Name), true, nil
		}
	}
	return "", true, nil
}

// CheckListRights sends DescribeInstances with DryRun set, so EC2 checks the instance
// role's right to list machines. It returns nil when EC2 returns DryRunOperation.
func (c *Client) CheckListRights(ctx context.Context) error {
	_, err := c.api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{DryRun: aws.Bool(true)})
	switch {
	case err == nil:
		return other("Check the EC2 endpoint: a DescribeInstances dry run returned success, and EC2 returns DryRunOperation for a dry run")
	case errorCode(err) == codeDryRun:
		return nil
	}
	return wrap("DescribeInstances dry run", err)
}

// Console returns the machine's latest console output as text, or "" when EC2 has
// none. EC2 returns the latest output only for an instance on the Nitro system; for
// any other instance it returns UnsupportedOperation, and Console reads the output
// again without Latest.
func (c *Client) Console(ctx context.Context, id string) (string, error) {
	out, err := c.api.GetConsoleOutput(ctx, &ec2.GetConsoleOutputInput{InstanceId: aws.String(id), Latest: aws.Bool(true)})
	if errorCode(err) == codeUnsupported {
		out, err = c.api.GetConsoleOutput(ctx, &ec2.GetConsoleOutputInput{InstanceId: aws.String(id)})
	}
	if err != nil {
		return "", wrap("GetConsoleOutput", err)
	}
	text, err := base64.StdEncoding.DecodeString(aws.ToString(out.Output))
	if err != nil {
		return "", other(fmt.Sprintf("If this repeats, check EC2's service health in this region: GetConsoleOutput returned output for %s that is not base64: %v", id, err))
	}
	return string(text), nil
}

// filter returns the EC2 filter name=values.
func filter(name string, values ...string) types.Filter {
	return types.Filter{Name: aws.String(name), Values: values}
}
