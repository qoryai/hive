// Package awsec2 is the EC2 machine adapter, qory-machines-aws-ec2: a
// [adapterio.Handler] that makes, lists, reads and deletes EC2 instances with the calls
// of package machine. Check and make read the adapter's settings file aws-ec2.json;
// list, log and delete need no setting and do not read it. Every command calls EC2
// at its standard endpoint with the host's instance role: [machine.LoadConfig] returns
// an error when the environment selects another credential source or endpoint.
package awsec2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"

	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/hive/internal/machine"
	"github.com/qoryai/integrations/contracts"
)

// The placeholder tags of check's dry run, in the shapes of a pool id and a key. The
// dry run makes no machine, so no instance gets these values.
const (
	checkPool = "np_0000000000000000"
	checkKey  = "dp_0000000000000000"
)

// The states of an EC2 instance that list asks EC2 for. A terminated instance is left
// out.
var listStates = []string{machine.StatePending, machine.StateRunning, machine.StateStopping, machine.StateStopped, machine.StateShuttingDown}

// Handler runs the commands of the EC2 machine adapter.
type Handler struct {
	// client returns the EC2 client after the check that the credentials come from the
	// instance role.
	client func(ctx context.Context) (*machine.Client, error)
	log    *slog.Logger
}

var _ adapterio.Handler = (*Handler)(nil)

// New returns the handler that calls EC2 with the configuration of
// [machine.LoadConfig]. It writes its log lines to stderr.
func New(stderr io.Writer) *Handler {
	return &Handler{client: loadClient, log: slog.New(slog.NewTextHandler(stderr, nil))}
}

// loadClient loads the AWS configuration and returns the EC2 client.
func loadClient(ctx context.Context) (*machine.Client, error) {
	cfg, err := machine.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	return machine.NewFromConfig(cfg), nil
}

// Check reads the settings, checks the credentials, reads the image's root device,
// sends one RunInstances dry run for each instance type of sizes, in sorted order and
// each type once, and sends one DescribeInstances dry run, the call of list. It makes
// no machine. It checks no right of TerminateInstances or GetConsoleOutput, because
// their calls need the id of an instance that exists.
func (h *Handler) Check(ctx context.Context, path string, _ contracts.MachinesCheck) (contracts.MachinesCheckAnswer, error) {
	s, err := ReadSettings(path)
	if err != nil {
		return contracts.MachinesCheckAnswer{}, err
	}
	c, err := h.client(ctx)
	if err != nil {
		return contracts.MachinesCheckAnswer{}, failure(err)
	}
	root, err := c.RootDevice(ctx, s.ImageID)
	if err != nil {
		return contracts.MachinesCheckAnswer{}, failure(err)
	}
	instanceTypes := slices.Compact(slices.Sorted(maps.Values(s.Sizes)))
	for _, t := range instanceTypes {
		if err := c.CheckRights(ctx, spec(s, t, root, checkPool, checkKey, nil)); err != nil {
			return contracts.MachinesCheckAnswer{}, failure(err)
		}
	}
	if err := c.CheckListRights(ctx); err != nil {
		return contracts.MachinesCheckAnswer{}, failure(err)
	}
	return contracts.MachinesCheckAnswer{}, nil
}

// Make makes one machine of the size's instance type, with the start data as its user
// data and the key as the client token. A second make with the same key returns the
// first machine's id, also when that machine is terminated and EC2 still returns it.
func (h *Handler) Make(ctx context.Context, path string, req contracts.MachinesMake, startData []byte) (contracts.MachinesMakeAnswer, error) {
	s, err := ReadSettings(path)
	if err != nil {
		return contracts.MachinesMakeAnswer{}, err
	}
	instanceType, ok := s.Sizes[req.Size]
	if !ok {
		return contracts.MachinesMakeAnswer{}, &adapterio.Failure{Kind: contracts.FailureDefect, Message: fmt.Sprintf("Add the size %s to sizes in %s: the make request has a size that sizes does not contain", req.Size, settingsFile)}
	}
	c, err := h.client(ctx)
	if err != nil {
		return contracts.MachinesMakeAnswer{}, failure(err)
	}
	root, err := c.RootDevice(ctx, s.ImageID)
	if err != nil {
		return contracts.MachinesMakeAnswer{}, failure(err)
	}
	m, err := c.Create(ctx, spec(s, instanceType, root, req.Pool, req.Key, startData))
	if machine.TokenTerminated(err) {
		found, ok, findErr := c.FindByToken(ctx, req.Key)
		if findErr != nil {
			return contracts.MachinesMakeAnswer{}, failure(findErr)
		}
		if ok {
			return contracts.MachinesMakeAnswer{ID: found.ID}, nil
		}
	}
	if err != nil {
		return contracts.MachinesMakeAnswer{}, failure(err)
	}
	return contracts.MachinesMakeAnswer{ID: m.ID}, nil
}

// spec is the machine spec of the settings s with the instance type, the image's root
// device, the pool, the key and the user data.
func spec(s Settings, instanceType, root, pool, key string, userData []byte) machine.Spec {
	return machine.Spec{
		Key:             key,
		PoolID:          pool,
		InstanceType:    instanceType,
		ImageID:         s.ImageID,
		RootDevice:      root,
		SubnetID:        s.SubnetID,
		SecurityGroupID: s.SecurityGroupID,
		UserData:        userData,
	}
}

// List lists the pool's instances that are not terminated, with the key from the
// qory:key tag and the start time from LaunchTime, in UTC and whole seconds. An
// instance whose qory:key tag is not a key, and an instance that EC2 returns with no
// LaunchTime, are left out, each with a log line.
func (h *Handler) List(ctx context.Context, _ string, req contracts.MachinesList) (contracts.MachinesListAnswer, error) {
	c, err := h.client(ctx)
	if err != nil {
		return contracts.MachinesListAnswer{}, failure(err)
	}
	ms, err := c.List(ctx, req.Pool, listStates...)
	if err != nil {
		return contracts.MachinesListAnswer{}, failure(err)
	}
	a := contracts.MachinesListAnswer{Machines: []contracts.Machine{}}
	for _, m := range ms {
		state, ok := listState(m.State)
		if !ok {
			continue
		}
		if adapterio.CheckKey(m.Key) != nil {
			h.log.Warn("Set the instance's qory:key tag to the key of its make request, or remove its qory:managed tag: list leaves out an instance whose qory:key tag is not a key", "instance", m.ID)
			continue
		}
		if m.LaunchTime.IsZero() {
			h.log.Warn("Check the instance in EC2: list leaves out an instance that DescribeInstances returned with no launch time", "instance", m.ID)
			continue
		}
		a.Machines = append(a.Machines, contracts.Machine{ID: m.ID, Key: m.Key, State: state, Started: contracts.FormatTime(m.LaunchTime)})
	}
	return a, nil
}

// listState maps an EC2 instance state to the state of a list answer. It returns false
// for terminated and for a state that EC2 does not document.
func listState(s string) (string, bool) {
	switch s {
	case machine.StatePending:
		return contracts.MachineStarting, true
	case machine.StateRunning, machine.StateStopping:
		return contracts.MachineRunning, true
	case machine.StateStopped:
		return contracts.MachineStopped, true
	case machine.StateShuttingDown:
		return contracts.MachineDeleting, true
	}
	return "", false
}

// Log reads the machine's latest console output and keeps the end that fits a log
// answer, as adapterio.LogAnswer cuts it: valid UTF-8 with U+0000 replaced, at most
// 768 KiB, and an answer of at most 1 MiB.
func (h *Handler) Log(ctx context.Context, _ string, req contracts.MachinesLog) (contracts.MachinesLogAnswer, error) {
	c, err := h.client(ctx)
	if err != nil {
		return contracts.MachinesLogAnswer{}, failure(err)
	}
	text, err := c.Console(ctx, req.ID)
	if err != nil {
		return contracts.MachinesLogAnswer{}, failure(err)
	}
	return adapterio.LogAnswer(text), nil
}

// Delete terminates the machine. An id that EC2 does not know, and a machine that is
// terminated, return gone; any other machine returns deleting.
func (h *Handler) Delete(ctx context.Context, _ string, req contracts.MachinesDelete) (contracts.MachinesDeleteAnswer, error) {
	c, err := h.client(ctx)
	if err != nil {
		return contracts.MachinesDeleteAnswer{}, failure(err)
	}
	state, found, err := c.Terminate(ctx, req.ID)
	if err != nil {
		return contracts.MachinesDeleteAnswer{}, failure(err)
	}
	if !found || state == machine.StateTerminated {
		return contracts.MachinesDeleteAnswer{State: contracts.MachineGone}, nil
	}
	return contracts.MachinesDeleteAnswer{State: contracts.MachineDeleting}, nil
}

// failure returns err as a *adapterio.Failure: a *adapterio.Failure unchanged, a
// failed EC2 call with its kind, and any other error with the kind other.
func failure(err error) error {
	var done *adapterio.Failure
	if errors.As(err, &done) {
		return done
	}
	var call *machine.Error
	if errors.As(err, &call) {
		return &adapterio.Failure{Kind: call.Kind, Message: call.Error()}
	}
	return &adapterio.Failure{Kind: contracts.FailureOther, Message: err.Error()}
}
