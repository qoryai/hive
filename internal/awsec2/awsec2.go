// Package awsec2 is the EC2 machine adapter, qory-machines-aws-ec2: a
// [machines.Handler] that makes, lists, reads and deletes EC2 instances with the calls
// of package machine. Check and make read the adapter's settings file aws-ec2.json;
// list, log and delete need no setting and do not read it. Every command calls EC2
// only with the host's instance role.
package awsec2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/qoryai/hive/internal/machine"
	"github.com/qoryai/hive/machines"
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

// keyShape is the shape of a key in the qory:key tag.
var keyShape = regexp.MustCompile(machines.KeyPattern)

// Handler runs the commands of the EC2 machine adapter.
type Handler struct {
	// client returns the EC2 client after the check that the credentials come from the
	// instance role.
	client func(ctx context.Context) (*machine.Client, error)
	log    *slog.Logger
}

var _ machines.Handler = (*Handler)(nil)

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
func (h *Handler) Check(ctx context.Context, path string, _ machines.CheckRequest) (machines.CheckAnswer, error) {
	s, err := ReadSettings(path)
	if err != nil {
		return machines.CheckAnswer{}, err
	}
	c, err := h.client(ctx)
	if err != nil {
		return machines.CheckAnswer{}, failure(err)
	}
	root, err := c.RootDevice(ctx, s.ImageID)
	if err != nil {
		return machines.CheckAnswer{}, failure(err)
	}
	instanceTypes := slices.Compact(slices.Sorted(maps.Values(s.Sizes)))
	for _, t := range instanceTypes {
		if err := c.CheckRights(ctx, spec(s, t, root, checkPool, checkKey, nil)); err != nil {
			return machines.CheckAnswer{}, failure(err)
		}
	}
	if err := c.CheckListRights(ctx); err != nil {
		return machines.CheckAnswer{}, failure(err)
	}
	return machines.CheckAnswer{}, nil
}

// Make makes one machine of the size's instance type, with the start data as its user
// data and the key as the client token. A second make with the same key returns the
// first machine's id, also when that machine is terminated and EC2 still returns it.
func (h *Handler) Make(ctx context.Context, path string, req machines.MakeRequest) (machines.MakeAnswer, error) {
	s, err := ReadSettings(path)
	if err != nil {
		return machines.MakeAnswer{}, err
	}
	instanceType, ok := s.Sizes[req.Size]
	if !ok {
		return machines.MakeAnswer{}, &machines.Error{Kind: machines.KindDefect, Message: fmt.Sprintf("Add the size %s to sizes in %s: the make request has a size that sizes does not contain", req.Size, settingsFile)}
	}
	c, err := h.client(ctx)
	if err != nil {
		return machines.MakeAnswer{}, failure(err)
	}
	root, err := c.RootDevice(ctx, s.ImageID)
	if err != nil {
		return machines.MakeAnswer{}, failure(err)
	}
	m, err := c.Create(ctx, spec(s, instanceType, root, req.Pool, req.Key, req.StartData))
	if machine.TokenTerminated(err) {
		found, ok, findErr := c.FindByToken(ctx, req.Key)
		if findErr != nil {
			return machines.MakeAnswer{}, failure(findErr)
		}
		if ok {
			return machines.MakeAnswer{ID: found.ID}, nil
		}
	}
	if err != nil {
		return machines.MakeAnswer{}, failure(err)
	}
	return machines.MakeAnswer{ID: m.ID}, nil
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
// qory:key tag and the start time from LaunchTime. An instance whose qory:key tag is
// not a key is left out, with a log line.
func (h *Handler) List(ctx context.Context, _ string, req machines.ListRequest) (machines.ListAnswer, error) {
	c, err := h.client(ctx)
	if err != nil {
		return machines.ListAnswer{}, failure(err)
	}
	ms, err := c.List(ctx, req.Pool, listStates...)
	if err != nil {
		return machines.ListAnswer{}, failure(err)
	}
	a := machines.ListAnswer{Machines: []machines.Machine{}}
	for _, m := range ms {
		state, ok := listState(m.State)
		if !ok {
			continue
		}
		if !keyShape.MatchString(m.Key) {
			h.log.Warn("Set the instance's qory:key tag to the key of its make request, or remove its qory:managed tag: list leaves out an instance whose qory:key tag is not a key", "instance", m.ID)
			continue
		}
		a.Machines = append(a.Machines, machines.Machine{ID: m.ID, Key: m.Key, State: state, Started: m.LaunchTime.UTC()})
	}
	return a, nil
}

// listState maps an EC2 instance state to the state of a list answer. It returns false
// for terminated and for a state that EC2 does not document.
func listState(s string) (machines.State, bool) {
	switch s {
	case machine.StatePending:
		return machines.StateStarting, true
	case machine.StateRunning, machine.StateStopping:
		return machines.StateRunning, true
	case machine.StateStopped:
		return machines.StateStopped, true
	case machine.StateShuttingDown:
		return machines.StateDeleting, true
	}
	return "", false
}

// Log reads the machine's latest console output and keeps its last 768 KiB as valid
// UTF-8.
func (h *Handler) Log(ctx context.Context, _ string, req machines.LogRequest) (machines.LogAnswer, error) {
	c, err := h.client(ctx)
	if err != nil {
		return machines.LogAnswer{}, failure(err)
	}
	text, err := c.Console(ctx, req.ID)
	if err != nil {
		return machines.LogAnswer{}, failure(err)
	}
	return machines.LogAnswer{Log: lastPart(text, machines.MaxLog)}, nil
}

// lastPart returns the last bytes of text, at most limit, as valid UTF-8: each invalid
// byte sequence becomes U+FFFD, then the text is cut at the start of a character.
func lastPart(text string, limit int) string {
	text = strings.ToValidUTF8(text, string(utf8.RuneError))
	if len(text) <= limit {
		return text
	}
	text = text[len(text)-limit:]
	for len(text) > 0 && !utf8.RuneStart(text[0]) {
		text = text[1:]
	}
	return text
}

// Delete terminates the machine. An id that EC2 does not know, and a machine that is
// terminated, return gone; any other machine returns deleting.
func (h *Handler) Delete(ctx context.Context, _ string, req machines.DeleteRequest) (machines.DeleteAnswer, error) {
	c, err := h.client(ctx)
	if err != nil {
		return machines.DeleteAnswer{}, failure(err)
	}
	state, found, err := c.Terminate(ctx, req.ID)
	if err != nil {
		return machines.DeleteAnswer{}, failure(err)
	}
	if !found || state == machine.StateTerminated {
		return machines.DeleteAnswer{State: machines.StateGone}, nil
	}
	return machines.DeleteAnswer{State: machines.StateDeleting}, nil
}

// failure returns err as a *machines.Error: a *machines.Error unchanged, a failed EC2
// call with its kind, and any other error with the kind other.
func failure(err error) error {
	var done *machines.Error
	if errors.As(err, &done) {
		return done
	}
	var call *machine.Error
	if errors.As(err, &call) {
		return &machines.Error{Kind: call.Kind, Message: call.Error()}
	}
	return &machines.Error{Kind: machines.KindOther, Message: err.Error()}
}
