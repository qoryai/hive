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

// FindByToken returns the machine whose client token is dispatchID, and false when
// EC2 has none. A terminated machine stays visible for about one hour; when EC2
// returns more than one machine, FindByToken returns one that is not terminated.
func (c *Client) FindByToken(ctx context.Context, dispatchID string) (Machine, bool, error) {
	ms, err := c.describe(ctx, []types.Filter{filter("client-token", dispatchID)})
	if err != nil {
		return Machine{}, false, err
	}
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

// Describe returns the machine with instance id id, and false when EC2 returns
// InvalidInstanceID.NotFound or no instance. Right after Create, EC2 can return
// NotFound for a machine that exists.
func (c *Client) Describe(ctx context.Context, id string) (Machine, bool, error) {
	out, err := c.api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if errorCode(err) == codeInstanceIDNotFound {
		return Machine{}, false, nil
	}
	if err != nil {
		return Machine{}, false, wrap("DescribeInstances", err)
	}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			if aws.ToString(i.InstanceId) == id {
				return fromInstance(i), true, nil
			}
		}
	}
	return Machine{}, false, nil
}

// Terminate sends TerminateInstances for the one instance id. EC2 accepts it again
// for a machine that is already terminated. Terminated confirms the result.
func (c *Client) Terminate(ctx context.Context, id string) error {
	_, err := c.api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}})
	return wrap("TerminateInstances", err)
}

// Terminated reports whether DescribeInstances on instance id id returns state
// terminated. It reads by id, because EC2 removes the tags of a terminated machine.
// A machine that EC2 does not return counts as not confirmed: EC2 documents that
// NotFound after TerminateInstances does not mean the machine is terminated.
func (c *Client) Terminated(ctx context.Context, id string) (bool, error) {
	m, found, err := c.Describe(ctx, id)
	if err != nil || !found {
		return false, err
	}
	return m.State == StateTerminated, nil
}

// Console returns the machine's latest console output as text, or "" when EC2 has
// none.
func (c *Client) Console(ctx context.Context, id string) (string, error) {
	out, err := c.api.GetConsoleOutput(ctx, &ec2.GetConsoleOutputInput{InstanceId: aws.String(id), Latest: aws.Bool(true)})
	if err != nil {
		return "", wrap("GetConsoleOutput", err)
	}
	text, err := base64.StdEncoding.DecodeString(aws.ToString(out.Output))
	if err != nil {
		return "", fmt.Errorf("Read the console output of %s again: GetConsoleOutput returned output that is not base64: %w", id, err)
	}
	return string(text), nil
}

// filter returns the EC2 filter name=values.
func filter(name string, values ...string) types.Filter {
	return types.Filter{Name: aws.String(name), Values: values}
}
