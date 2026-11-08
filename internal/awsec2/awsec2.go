// Package awsec2 is the EC2 machine adapter, qory-machines-aws-ec2: a
// [adapterio.Handler] that makes, lists, reads and deletes EC2 instances with the calls
// of package machine. Check and make read the adapter's settings file aws-ec2.json;
// list, log and delete need no setting and do not read it. Every command calls EC2
// at its standard endpoint with the host's instance role: [machine.LoadConfig] returns
// an error when the environment selects another credential source or endpoint. Check
// and make also read the prices of instance types from the AWS Price List Query API,
// and keep the instance types and their prices in a cache file; see
// [Handler.catalog].
package awsec2

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

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
	// cacheDir returns the folder that contains the adapter's cache folder, as
	// os.UserCacheDir does.
	cacheDir func() (string, error)
	// now returns the time of the cache's reads.
	now func() time.Time
}

var _ adapterio.Handler = (*Handler)(nil)

// New returns the handler that calls EC2 with the configuration of
// [machine.LoadConfig]. It writes its log lines to stderr, and its cache file in the
// folder of os.UserCacheDir.
func New(stderr io.Writer) *Handler {
	return &Handler{client: loadClient, log: slog.New(slog.NewTextHandler(stderr, nil)), cacheDir: os.UserCacheDir, now: time.Now}
}

// loadClient loads the AWS configuration and returns the EC2 client.
func loadClient(ctx context.Context) (*machine.Client, error) {
	cfg, err := machine.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	return machine.NewFromConfig(cfg), nil
}

// Check reads the settings, checks the credentials, and reads from EC2 the image and
// the zone of the subnet. It then reads, through the cache of [Handler.catalog], the
// instance types that the zone offers and their prices. It fails with the kind defect
// when the zone offers no on-demand instance type that runs the image's architecture.
// It sends at most 2 RunInstances dry runs, as [checkRights] lists them, and then one
// DescribeInstances dry run, the call of list. It makes no machine. It checks no right
// of TerminateInstances or GetConsoleOutput, because their calls need the id of an
// instance that exists. The answer contains the offers of [offers].
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
	zone, err := c.SubnetZone(ctx, s.SubnetID)
	if err != nil {
		return contracts.MachinesCheckAnswer{}, failure(err)
	}
	all, prices, err := h.catalog(ctx, c, zone)
	if err != nil {
		return contracts.MachinesCheckAnswer{}, failure(err)
	}
	ts := usable(all, img)
	order(ts, prices)
	o := offers(ts, img)
	if len(o) == 0 {
		return contracts.MachinesCheckAnswer{}, defect(fmt.Sprintf("Set subnet_id in %s to a subnet in another zone, or image_id to an image of another architecture: zone %s offers no on-demand instance type for %s", settingsFile, zone, img.Arch))
	}
	if err := checkRights(ctx, c, s, ts, img); err != nil {
		return contracts.MachinesCheckAnswer{}, failure(err)
	}
	if err := c.CheckListRights(ctx); err != nil {
		return contracts.MachinesCheckAnswer{}, failure(err)
	}
	return contracts.MachinesCheckAnswer{Offers: o}, nil
}

// checkRights sends at most 2 RunInstances dry runs, one after the other: one for the
// first type of ts, the type that make starts first for the smallest machine spec, and,
// when a type of ts needs CpuOptions for nested virtualization, one with CpuOptions
// for the first such type. ts is in the order of [order] and is not empty. It returns
// the error of the first dry run that failed, or nil.
func checkRights(ctx context.Context, c *machine.Client, s Settings, ts []machine.InstanceType, img machine.Image) error {
	if err := c.CheckRights(ctx, spec(s, ts[0].Name, img, img.RootSizeGiB, checkPool, checkKey, nil)); err != nil {
		return err
	}
	i := slices.IndexFunc(ts, machine.InstanceType.NestedCPUOptions)
	if i < 0 {
		return nil
	}
	sp := spec(s, ts[i].Name, img, img.RootSizeGiB, checkPool, checkKey, nil)
	sp.NestedVirtualization = true
	return c.CheckRights(ctx, sp)
}

// offer is the machine that the adapter offers for the instance type t and the image
// img: t's vCPUs and memory, the largest gp3 root volume, img's architecture, and
// virtualization when t is bare metal or has nested virtualization.
func offer(t machine.InstanceType, img machine.Image) contracts.MachineSpec {
	return contracts.MachineSpec{
		VCPUs:          t.VCPUs,
		MemoryMiB:      t.MemoryMiB,
		DiskGiB:        machine.MaxRootGiB,
		Arch:           img.Arch,
		Virtualization: t.Virtualization(),
	}
}

// The bounds of an offer's vCPUs and memory, the bounds of machine_spec in the
// provisioner contract, and the most offers of a check answer.
const (
	minVCPUs     = 1
	maxVCPUs     = 1024
	minMemoryMiB = 512
	maxMemoryMiB = 8388608
	maxOffers    = 64
)

// offers returns the offers of check for the instance types ts, which run img's
// architecture: the shapes of [offer] that no other shape covers. A shape covers
// another when it fits it, [contracts.MachineSpec.Fits]: at least its vCPUs, memory
// and disk, the same architecture, and virtualization when the other has it.
//
// Offers are only the shapes that no other shape covers, because Qory Apiary uses
// offers only to test whether any offer fits a run's machine type. A covered shape
// adds no machine type that a shape covering it does not fit, and a zone offers more
// distinct shapes than the 64 offers that a check answer may have.
//
// A type with fewer vCPUs or less memory than an offer may have is left out. A
// type's vCPUs and memory above the bounds of an offer are cut to those bounds, so the
// offer is smaller than the machine and still true. When more than 64 shapes remain,
// offers keeps the 64 with the most vCPUs, then the most memory. The offers are in
// that order, with virtualization first.
func offers(ts []machine.InstanceType, img machine.Image) []contracts.MachineSpec {
	var shapes []contracts.MachineSpec
	for _, t := range ts {
		if t.VCPUs < minVCPUs || t.MemoryMiB < minMemoryMiB {
			continue
		}
		o := offer(t, img)
		o.VCPUs = min(o.VCPUs, maxVCPUs)
		o.MemoryMiB = min(o.MemoryMiB, maxMemoryMiB)
		if !slices.Contains(shapes, o) {
			shapes = append(shapes, o)
		}
	}
	var out []contracts.MachineSpec
	for i, s := range shapes {
		covered := false
		for j, o := range shapes {
			if i != j && o.Fits(s) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b contracts.MachineSpec) int {
		return cmp.Or(cmp.Compare(b.VCPUs, a.VCPUs), cmp.Compare(b.MemoryMiB, a.MemoryMiB), compareBool(b.Virtualization, a.Virtualization))
	})
	return out[:min(len(out), maxOffers)]
}

// compareBool orders false before true.
func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// usable returns the instance types of ts that make may start for an image of img's
// architecture: each is sold on demand and runs that architecture. It keeps the
// order of ts.
func usable(ts []machine.InstanceType, img machine.Image) []machine.InstanceType {
	var out []machine.InstanceType
	for _, t := range ts {
		if t.OnDemand && slices.Contains(t.Archs, img.Arch) {
			out = append(out, t)
		}
	}
	return out
}

// order sorts the instance types ts in the order in which make tries them: the fewest
// vCPUs, then the least memory, then the lowest price of prices, a type without a
// price after the types with one, then the type's name.
func order(ts []machine.InstanceType, prices map[string]float64) {
	slices.SortFunc(ts, func(a, b machine.InstanceType) int {
		pa, okA := prices[a.Name]
		pb, okB := prices[b.Name]
		byPrice := compareBool(!okA, !okB)
		if okA && okB {
			byPrice = cmp.Compare(pa, pb)
		}
		return cmp.Or(cmp.Compare(a.VCPUs, b.VCPUs), cmp.Compare(a.MemoryMiB, b.MemoryMiB), byPrice, strings.Compare(a.Name, b.Name))
	})
}

// fitting returns the instance types of ts whose offer fits want, in the order of ts.
func fitting(ts []machine.InstanceType, img machine.Image, want contracts.MachineSpec) []machine.InstanceType {
	var out []machine.InstanceType
	for _, t := range ts {
		if offer(t, img).Fits(want) {
			out = append(out, t)
		}
	}
	return out
}

// maxTries is the most instance types that one make tries. When EC2 has no capacity
// for the first 8 types that fit, make returns the last capacity error, and the
// provisioner tries the dispatch again later.
const maxTries = 8

// Make makes one machine of the first instance type that fits the request's machine
// spec, in the order of [order]: the fewest vCPUs, then the least memory, then the
// lowest on-demand price, then the name. It considers the types that the subnet's
// zone offers on demand and that run the image's architecture, which it reads through
// the cache of [Handler.catalog]. It passes the start data as the user data, sends
// [machine.ClientToken] of the key and the type as the client token, and sizes the
// root volume to the larger of the spec's disk and the image's root volume. When the
// spec sets virtualization and the type has nested virtualization and is not bare
// metal, it turns that on. When EC2 has no capacity for the type, make tries the next
// type that fits, in the same order, at most [maxTries] types, and when each of them
// fails for lack of capacity, it returns the last failure. When no type fits, make
// fails with the kind defect. When the spec's disk is larger than the largest gp3
// volume, [machine.MaxRootGiB], make fails with the kind defect before any EC2 call:
// an offer of check has at most that disk, so such a spec is a defect of the
// provisioner.
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
	c, err := h.client(ctx)
	if err != nil {
		return contracts.MachinesMakeAnswer{}, failure(err)
	}
	img, err := c.Image(ctx, s.ImageID)
	if err != nil {
		return contracts.MachinesMakeAnswer{}, failure(err)
	}
	zone, err := c.SubnetZone(ctx, s.SubnetID)
	if err != nil {
		return contracts.MachinesMakeAnswer{}, failure(err)
	}
	all, prices, err := h.catalog(ctx, c, zone)
	if err != nil {
		return contracts.MachinesMakeAnswer{}, failure(err)
	}
	fits := fitting(usable(all, img), img, want)
	if len(fits) == 0 {
		virt := ""
		if want.Virtualization {
			virt = ", with virtualization"
		}
		return contracts.MachinesMakeAnswer{}, defect(fmt.Sprintf("Run check again, or report this defect in the provisioner: zone %s offers no type for image_id that fits %d vCPUs, %d MiB, %d GiB, %s%s", zone, want.VCPUs, want.MemoryMiB, want.DiskGiB, want.Arch, virt))
	}
	order(fits, prices)
	fits = fits[:min(len(fits), maxTries)]
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
