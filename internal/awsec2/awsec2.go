// Package awsec2 is the EC2 machine adapter, qory-machines-aws-ec2: a
// [adapterio.Handler] that makes, lists, reads and deletes EC2 instances with the calls
// of package machine. Check and make read the adapter's settings file aws-ec2.json;
// list, log and delete need no setting and do not read it. Every command calls EC2
// at its standard endpoint with the host's instance role: [machine.LoadConfig] returns
// an error when the environment selects another credential source or endpoint.
package awsec2

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"

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

// Check reads the settings, checks the credentials, and reads from EC2 the image, the
// zone of the subnet, each instance type of instance_types, and which of those types
// the zone offers. It fails with the kind defect when max_root_gib is smaller than the
// image's root volume, and for an instance type that does not
// run the image's architecture, that the zone does not offer, or whose size is outside
// the bounds of an offer. It then sends one RunInstances dry run per instance type, at
// most 8 at once, and returns the error of the first type in the order of
// instance_types whose dry run fails. Then it sends one DescribeInstances dry run, the
// call of list. It makes no machine. It checks no right of TerminateInstances or
// GetConsoleOutput, because their calls need the id of an instance that exists. The
// answer has one offer per instance type, in the order of instance_types, each with
// a disk of max_root_gib.
func (h *Handler) Check(ctx context.Context, path string, _ contracts.MachinesCheck) (contracts.MachinesCheckAnswer, error) {
	s, err := ReadSettings(path)
	if err != nil {
		return contracts.MachinesCheckAnswer{}, err
	}
	c, err := h.client(ctx)
	if err != nil {
		return contracts.MachinesCheckAnswer{}, failure(err)
	}
	img, err := c.Image(ctx, s.ImageID)
	if err != nil {
		return contracts.MachinesCheckAnswer{}, failure(err)
	}
	if err := rootFits(s, img); err != nil {
		return contracts.MachinesCheckAnswer{}, err
	}
	zone, err := c.SubnetZone(ctx, s.SubnetID)
	if err != nil {
		return contracts.MachinesCheckAnswer{}, failure(err)
	}
	infos, err := c.InstanceTypes(ctx, s.InstanceTypes)
	if err != nil {
		return contracts.MachinesCheckAnswer{}, failure(err)
	}
	offered, err := c.Offered(ctx, zone, s.InstanceTypes)
	if err != nil {
		return contracts.MachinesCheckAnswer{}, failure(err)
	}
	offers := make([]contracts.MachineSpec, 0, len(s.InstanceTypes))
	for _, name := range s.InstanceTypes {
		t := infos[name]
		switch {
		case !slices.Contains(t.Archs, img.Arch):
			return contracts.MachinesCheckAnswer{}, defect(fmt.Sprintf("Remove %s from instance_types in %s, or set image_id to an image it runs: in %s, it does not run the image's architecture, %s", name, settingsFile, c.Region(), img.Arch))
		case !offered[name]:
			return contracts.MachinesCheckAnswer{}, defect(fmt.Sprintf("Remove %s from instance_types in %s, or set subnet_id to a subnet in a zone that offers it: in %s, zone %s does not offer it", name, settingsFile, c.Region(), zone))
		}
		o := offer(t, img, s.MaxRootGiB)
		if !inBounds(o) {
			return contracts.MachinesCheckAnswer{}, defect(fmt.Sprintf("Remove %s from instance_types in %s: in %s, it has %d vCPUs and %d MiB, and an offer has 1 to 1024 vCPUs and 512 to 8388608 MiB", name, settingsFile, c.Region(), o.VCPUs, o.MemoryMiB))
		}
		offers = append(offers, o)
	}
	if err := checkRights(ctx, c, s, infos, img); err != nil {
		return contracts.MachinesCheckAnswer{}, failure(err)
	}
	if err := c.CheckListRights(ctx); err != nil {
		return contracts.MachinesCheckAnswer{}, failure(err)
	}
	return contracts.MachinesCheckAnswer{Offers: offers}, nil
}

// rootFits returns a failure of the kind defect when max_root_gib of s is smaller than
// the root volume of img, and nil otherwise.
func rootFits(s Settings, img machine.Image) error {
	if img.RootSizeGiB <= s.MaxRootGiB {
		return nil
	}
	return defect(fmt.Sprintf("Raise max_root_gib in %s. Where the deploy recipe writes the file, raise the recipe's MaxRootVolumeGiB setting. The image's root volume has %d GiB, above max_root_gib %d GiB", settingsFile, img.RootSizeGiB, s.MaxRootGiB))
}

// maxDryRuns is the most RunInstances dry runs that check sends at once.
const maxDryRuns = 8

// checkRights sends one RunInstances dry run per type of s.InstanceTypes, at most
// [maxDryRuns] at once, so that check ends within the command's time limit also for
// 64 types. It returns the error of the first type in the order of instance_types whose
// dry run failed, or nil.
func checkRights(ctx context.Context, c *machine.Client, s Settings, infos map[string]machine.InstanceType, img machine.Image) error {
	errs := make([]error, len(s.InstanceTypes))
	slots := make(chan struct{}, maxDryRuns)
	var wg sync.WaitGroup
	for i, name := range s.InstanceTypes {
		sp := spec(s, name, img, img.RootSizeGiB, checkPool, checkKey, nil)
		sp.NestedVirtualization = infos[name].NestedCPUOptions()
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			errs[i] = c.CheckRights(ctx, sp)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// offer is the machine that the adapter offers for the instance type t and the image
// img: t's vCPUs and memory, a root volume of maxRootGiB, img's architecture, and
// virtualization when t is bare metal or has nested virtualization.
func offer(t machine.InstanceType, img machine.Image, maxRootGiB int) contracts.MachineSpec {
	return contracts.MachineSpec{
		VCPUs:          t.VCPUs,
		MemoryMiB:      t.MemoryMiB,
		DiskGiB:        maxRootGiB,
		Arch:           img.Arch,
		Virtualization: t.Virtualization(),
	}
}

// The bounds of an offer's vCPUs and memory, the bounds of machine_spec in the
// provisioner contract.
const (
	minVCPUs     = 1
	maxVCPUs     = 1024
	minMemoryMiB = 512
	maxMemoryMiB = 8388608
)

// inBounds reports whether the vCPUs and the memory of o are within the bounds of an
// offer.
func inBounds(o contracts.MachineSpec) bool {
	return o.VCPUs >= minVCPUs && o.VCPUs <= maxVCPUs && o.MemoryMiB >= minMemoryMiB && o.MemoryMiB <= maxMemoryMiB
}

// fitting returns the instance types of names whose offer, with a root volume of
// maxRootGiB, fits want, smallest first: by vCPUs, then by memory, a tie going to the
// order of names. A type that does not run img's architecture is left out.
func fitting(names []string, infos map[string]machine.InstanceType, img machine.Image, maxRootGiB int, want contracts.MachineSpec) []machine.InstanceType {
	var out []machine.InstanceType
	for _, name := range names {
		t := infos[name]
		if slices.Contains(t.Archs, img.Arch) && offer(t, img, maxRootGiB).Fits(want) {
			out = append(out, t)
		}
	}
	slices.SortStableFunc(out, func(a, b machine.InstanceType) int {
		return cmp.Or(cmp.Compare(a.VCPUs, b.VCPUs), cmp.Compare(a.MemoryMiB, b.MemoryMiB))
	})
	return out
}

// Make makes one machine of the smallest instance type of instance_types that fits
// the request's machine spec, by vCPUs, then memory, then the order of
// instance_types. It passes the start data as the user data, sends
// [machine.ClientToken] of the key and the type as the client token, and sizes the
// root volume to the larger of the spec's disk and the image's root volume. When the
// spec sets virtualization and the type has nested virtualization and is not bare
// metal, it turns that on. When EC2 has no capacity for the type, make tries the next
// type that fits, in the same order, and when every type fails for lack of capacity,
// it returns the last failure. When no type fits, make fails with the kind defect.
// When the spec's disk is larger than max_root_gib, make fails with the kind defect
// before any EC2 call: the instance role's policy limits the root volume to the deploy
// setting MaxRootVolumeGiB, which sets max_root_gib. When the image's root volume is
// larger than max_root_gib, make fails with the kind defect before any RunInstances
// call.
//
// Before the first try, make looks up a machine of the pool with the key in its qory:key
// tag. When there is one, in any state, make returns its id and makes no machine, so a
// second make with the same key returns the first machine also when that machine is of
// another type than the first that fits, once DescribeInstances lists the first machine:
// EC2 lists a new machine after a short delay. A try whose client token EC2 reports as
// terminated returns the id of the machine with that token.
func (h *Handler) Make(ctx context.Context, path string, req contracts.MachinesMake, startData []byte) (contracts.MachinesMakeAnswer, error) {
	s, err := ReadSettings(path)
	if err != nil {
		return contracts.MachinesMakeAnswer{}, err
	}
	want := req.MachineSpec
	if want.DiskGiB > machine.MaxRootGiB {
		return contracts.MachinesMakeAnswer{}, defect(fmt.Sprintf("Report this defect in the provisioner: the machine spec has disk_gib %d GiB, and a gp3 volume has at most %d GiB", want.DiskGiB, machine.MaxRootGiB))
	}
	if want.DiskGiB > s.MaxRootGiB {
		return contracts.MachinesMakeAnswer{}, defect(fmt.Sprintf("Raise max_root_gib in %s. Where the deploy recipe writes the file, raise the recipe's MaxRootVolumeGiB setting. The machine spec has disk_gib %d GiB, above max_root_gib %d GiB", settingsFile, want.DiskGiB, s.MaxRootGiB))
	}
	c, err := h.client(ctx)
	if err != nil {
		return contracts.MachinesMakeAnswer{}, failure(err)
	}
	img, err := c.Image(ctx, s.ImageID)
	if err != nil {
		return contracts.MachinesMakeAnswer{}, failure(err)
	}
	// The spec's disk is at most max_root_gib, so the root volume, the larger of the
	// disk and the image's root volume, is larger only when the image's root is.
	if err := rootFits(s, img); err != nil {
		return contracts.MachinesMakeAnswer{}, err
	}
	infos, err := c.InstanceTypes(ctx, s.InstanceTypes)
	if err != nil {
		return contracts.MachinesMakeAnswer{}, failure(err)
	}
	fits := fitting(s.InstanceTypes, infos, img, s.MaxRootGiB, want)
	if len(fits) == 0 {
		virt := ""
		if want.Virtualization {
			virt = ", with virtualization"
		}
		return contracts.MachinesMakeAnswer{}, defect(fmt.Sprintf("Add an instance type that fits the machine spec to instance_types in %s: in %s, no type fits %d vCPUs, %d MiB, %d GiB, %s%s", settingsFile, c.Region(), want.VCPUs, want.MemoryMiB, want.DiskGiB, want.Arch, virt))
	}
	found, ok, err := c.FindByKey(ctx, req.Pool, req.Key)
	if err != nil {
		return contracts.MachinesMakeAnswer{}, failure(err)
	}
	if ok {
		return answer(found, ""), nil
	}
	root := max(want.DiskGiB, img.RootSizeGiB)
	var last error
	for i, t := range fits {
		sp := spec(s, t.Name, img, root, req.Pool, req.Key, startData)
		sp.NestedVirtualization = want.Virtualization && t.NestedCPUOptions()
		m, err := c.Create(ctx, sp)
		if machine.TokenTerminated(err) {
			found, ok, findErr := c.FindByToken(ctx, machine.ClientToken(req.Key, t.Name))
			if findErr != nil {
				return contracts.MachinesMakeAnswer{}, failure(findErr)
			}
			if ok {
				return answer(found, t.Name), nil
			}
		}
		if machine.NoCapacity(err) {
			last = err
			if i+1 < len(fits) {
				h.log.Warn("EC2 has no capacity for the instance type in the subnet's zone, so make tries the next type that fits", "instance_type", t.Name, "next", fits[i+1].Name)
			}
			continue
		}
		if err != nil {
			return contracts.MachinesMakeAnswer{}, failure(err)
		}
		return answer(m, t.Name), nil
	}
	return contracts.MachinesMakeAnswer{}, failure(last)
}

// answer is the make answer of the machine m: its id, and its instance type, or else
// instanceType.
func answer(m machine.Machine, instanceType string) contracts.MachinesMakeAnswer {
	t := m.InstanceType
	if t == "" {
		t = instanceType
	}
	return contracts.MachinesMakeAnswer{ID: m.ID, MachineType: t}
}

// spec is the machine spec of the settings s, with their tags, and the instance type,
// the image's root device, the root volume size, the pool, the key and the user data.
func spec(s Settings, instanceType string, img machine.Image, rootGiB int, pool, key string, userData []byte) machine.Spec {
	return machine.Spec{
		Key:             key,
		PoolID:          pool,
		InstanceType:    instanceType,
		ImageID:         s.ImageID,
		RootDevice:      img.RootDevice,
		RootSizeGiB:     rootGiB,
		SubnetID:        s.SubnetID,
		SecurityGroupID: s.SecurityGroupID,
		UserData:        userData,
		Tags:            s.Tags,
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

// defect returns a failure of the kind defect with message.
func defect(message string) error {
	return &adapterio.Failure{Kind: contracts.FailureDefect, Message: message}
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
