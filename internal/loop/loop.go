// Package loop is the provisioner's loop. It claims dispatches from Qory Apiary one at
// a time, makes one machine for each through the machine adapter, sends dispatched,
// and deletes each machine once its run ends, for any reason. The loop reads nothing
// about the run: a claim gives it a run code, which it passes to the machine in the
// start data and never reads, logs or writes to a file.
//
// The loop has three parts. [Run] first runs the adapter's check and recovers the
// journal of live dispatches after a restart. Then the claim path and the watch run in
// parallel until the context ends:
//   - The claim path claims a dispatch while the journal has fewer entries than
//     max_machines, records it, makes its machine and sends dispatched.
//   - The watch lists the pool's machines every 60 seconds. It sends dispatched again
//     for each machine, and it cleans up each machine whose dispatch ended, that
//     stopped, or that ran longer than its lifetime and 15 minutes. It also cleans up
//     each listed machine that has no journal entry.
//
// To clean up a machine, the loop reads the machine's log through the adapter,
// uploads it to Qory Apiary, deletes the machine and removes the journal entry. Each
// step runs after a failure of the step before it, and a failed delete runs again at
// the next watch.
//
// When the context ends, the claim path stops claiming. A claim that has no make yet
// is released, and a make in flight finishes and its machine is recorded. The loop
// leaves every machine as it is.
package loop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/qoryai/hive/internal/adapter"
	"github.com/qoryai/hive/internal/journal"
	"github.com/qoryai/hive/machines"
)

// DefaultSize is the size that the loop asks the machine adapter for when a claim
// names no machine type. Each adapter's settings map size names to the vendor's
// machine types, so an operator who sets "standard" in them sets the machine of every
// claim that names no type.
const DefaultSize = "standard"

// The times of the loop.
const (
	// WatchInterval is the time between two watches. It is also the time between two
	// dispatched of a machine, as the provisioner contract requires.
	WatchInterval = 60 * time.Second
	// LifetimeGrace is the time after a machine's lifetime, counted from the start
	// time in the adapter's list, after which the watch cleans up the machine.
	LifetimeGrace = 15 * time.Minute
	// LongWait is the wait before the next claim after a make that failed with the
	// kind limit or defect, and after the start data of a claim could not be built.
	LongWait = 60 * time.Second
	// ShortWait is the wait before the next claim after a make that failed with the
	// kind retry or other, and after a claim that failed.
	ShortWait = 15 * time.Second
	// releaseTimeout bounds a release, which runs also after the context ends.
	releaseTimeout = 30 * time.Second
)

// The refusals of Qory Apiary that the loop acts on. An [Apiary] returns an error that
// wraps one of them, so that errors.Is finds it.
var (
	// ErrGone is 410 gone: the dispatch ended.
	ErrGone = errors.New("410 gone")
	// ErrLeaseExpired is 409 lease_expired: the first dispatched came at or after
	// lease_until, so the dispatch is stale.
	ErrLeaseExpired = errors.New("409 lease_expired")
	// ErrMachineExists is 409 machine_exists: a release came after dispatched, or after
	// the machine redeemed its run code.
	ErrMachineExists = errors.New("409 machine_exists")
)

// Claim is a claim answer: a request for one machine.
type Claim struct {
	// DispatchID is the dispatch, dp_ and 16 characters. It is the key of the machine.
	DispatchID string
	// PoolID is the node pool whose key made the claim, np_ and 16 characters.
	PoolID string
	// LeaseUntil is the time before which the first dispatched must arrive.
	LeaseUntil time.Time
	// Machine is the type of machine, a size name for the adapter, or "" when the
	// claim names none.
	Machine string
	// RunCode is the run code that the machine redeems.
	RunCode RunCode
	// GatewayURL is the gateway's URL that the machine passes to qory run.
	GatewayURL string
	// MaxLifetimeMinutes is the time after which the machine powers off, 15 to 1470.
	MaxLifetimeMinutes int
}

// Apiary is the provisioner's client of Qory Apiary: the four requests of the
// provisioner contract that the loop sends.
type Apiary interface {
	// Claim claims a request for one machine. It returns false and no error when no
	// work could be claimed within the claim's wait.
	Claim(ctx context.Context) (Claim, bool, error)
	// Dispatched sends dispatched for the dispatch, with the machine id as the
	// external ref. Its error wraps [ErrGone] or [ErrLeaseExpired] for those
	// refusals.
	Dispatched(ctx context.Context, dispatchID, machineID string) error
	// Release gives the dispatch back, with a reason of 1 to 200 characters. Its error
	// wraps [ErrGone], [ErrLeaseExpired] or [ErrMachineExists] for those refusals.
	Release(ctx context.Context, dispatchID, reason string) error
	// MachineLog uploads the machine's log of the dispatch. truncated is true when
	// the log is the last part of a longer one. Its error wraps [ErrGone] for that
	// refusal.
	MachineLog(ctx context.Context, dispatchID, log string, truncated bool) error
}

// Machines is the runner of the machine adapter's commands, as [adapter.Runner] runs
// them. A failed command returns an *adapter.Error with the kind of failure.
type Machines interface {
	// Check shows that the adapter's rights and settings work.
	Check(ctx context.Context) error
	// Make makes one machine of size in pool for key and delivers startData to it.
	Make(ctx context.Context, key, pool, size string, startData []byte) (machines.MakeAnswer, error)
	// List lists the machines of pool.
	List(ctx context.Context, pool string) (machines.ListAnswer, error)
	// Log reads the console text of the machine id.
	Log(ctx context.Context, id string) (machines.LogAnswer, error)
	// Delete deletes the machine id.
	Delete(ctx context.Context, id string) (machines.DeleteAnswer, error)
}

var _ Machines = (*adapter.Runner)(nil)

// Clock is the loop's source of time.
type Clock interface {
	// Now returns the time.
	Now() time.Time
	// After returns a channel that receives the time once d has passed.
	After(d time.Duration) <-chan time.Time
}

// systemClock is the clock of the system.
type systemClock struct{}

// Now returns the system's time.
func (systemClock) Now() time.Time { return time.Now() }

// After returns time.After(d).
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Config is what the loop runs with.
type Config struct {
	// Apiary is the client of Qory Apiary.
	Apiary Apiary
	// Machines is the runner of the machine adapter.
	Machines Machines
	// Journal is the journal of live dispatches in the state folder.
	Journal *journal.Journal
	// PoolID is the node pool of the provisioner's access key, np_ and 16 characters.
	PoolID string
	// MaxMachines is max_machines of the settings: the most journal entries at once.
	MaxMachines int
	// GatewayAddress is gateway_address of the settings, which the start data contains.
	GatewayAddress netip.Addr
	// Log receives the loop's log lines.
	Log *slog.Logger
	// Clock is the source of time. Nil is the system's clock.
	Clock Clock
}

// Run runs the loop until ctx ends, and then returns nil. It returns an error that
// stops the program when the adapter's check fails, when listing the machines for the
// recovery fails, and when the journal cannot be written before a make or after it.
func Run(ctx context.Context, cfg Config) error {
	l := &loop{
		Config: cfg,
		busy:   map[string]bool{},
		logged: map[string]bool{},
		freed:  make(chan struct{}, 1),
	}
	if l.Clock == nil {
		l.Clock = systemClock{}
	}
	if err := l.Machines.Check(ctx); err != nil {
		return fmt.Errorf("Fix the machine adapter's settings or the rights it uses, then start the provisioner again: the machine adapter's check failed: %s", shown(err, nil))
	}
	if err := l.recoverJournal(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() { l.watch(ctx) })
	err := l.claim(ctx)
	cancel()
	wg.Wait()
	return err
}

// loop is one run of the loop.
type loop struct {
	Config
	mu sync.Mutex
	// busy are the dispatches that the claim path works on. The watch skips them.
	busy map[string]bool
	// logged are the machine ids whose log is uploaded, or whose upload failed, in a
	// cleanup whose delete has not succeeded yet. A later try of the delete does not
	// upload the log again.
	logged map[string]bool
	// freed receives a value when an entry leaves the journal.
	freed chan struct{}
}

// wait waits d on the clock, or until ctx ends.
func (l *loop) wait(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-l.Clock.After(d):
	}
}

// setBusy marks the dispatch id as the claim path's, or no longer.
func (l *loop) setBusy(id string, busy bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if busy {
		l.busy[id] = true
	} else {
		delete(l.busy, id)
	}
}

// isBusy is whether the claim path works on the dispatch id.
func (l *loop) isBusy(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.busy[id]
}

// remove takes the dispatch out of the journal and lets the claim path claim again. A
// failure is logged, and the entry stays: the next watch removes a made or dispatched
// entry again, and the recovery after a restart removes a claimed one.
func (l *loop) remove(id string) {
	if err := l.Journal.Remove(id); err != nil {
		l.Log.Error("removing a dispatch from the journal failed", slog.String("dispatch_id", id), errorAttr(err, nil))
		return
	}
	select {
	case l.freed <- struct{}{}:
	default:
	}
}

// release releases the dispatch with reason. It runs also after ctx ends, for a
// claim whose make has not started. A failure is logged: a release has no retry.
func (l *loop) release(ctx context.Context, id, reason string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if err := l.Apiary.Release(ctx, id, reason); err != nil {
		l.Log.Warn("releasing a dispatch failed", slog.String("dispatch_id", id), errorAttr(err, nil))
		return
	}
	l.Log.Info("released a dispatch", slog.String("dispatch_id", id))
}

// hiddenError replaces an error's text that contains what looks like a secret.
const hiddenError = "the error's text contains what looks like a run code, a run key, an access key secret or the start data, so the provisioner does not show it"

// shown is err's text, or [hiddenError] when the text contains what looks like a run
// code, a piece of startData, a run key or an access key secret.
func shown(err error, startData []byte) string {
	text := err.Error()
	lower := strings.ToLower(text)
	if machines.ContainsSecret(text, startData) || strings.Contains(lower, "qrk_") || strings.Contains(lower, "qak_") {
		return hiddenError
	}
	return text
}

// errorAttr is the attribute error with err's text as [shown] gives it.
func errorAttr(err error, startData []byte) slog.Attr {
	return slog.String("error", shown(err, startData))
}
