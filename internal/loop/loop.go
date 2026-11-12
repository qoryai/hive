// Package loop is the provisioner's loop. It claims dispatches from Qory Apiary one at
// a time, makes one machine for each through the machine adapter, sends dispatched,
// and deletes each machine once its run ends, for any reason. The loop reads nothing
// about the run: a claim gives it a run code, which it passes to the machine in the
// start data and never reads, logs or writes to a file.
//
// The loop has three parts. [Run] first runs the adapter's check, which returns the
// machines the adapter can make, its offers, and recovers the journal of live
// dispatches after a restart. Then the claim path and the watch run in parallel until
// the context ends:
//   - The claim path claims one dispatch at a time, with the check's offers in each
//     claim, and claims again as soon as it has handled one. The pool's Machine limit
//     in Qory Apiary is the only limit on how many machines exist at once. It records
//     the dispatch, makes a machine that fits the claim's machine spec and sends
//     dispatched, with the vendor's machine type that the make returned. When the
//     claim has no machine spec or the make fails, it releases the dispatch. When the
//     release after a make returns 204 or 409 lease_expired, it then uploads the
//     make's error as the dispatch's machine log.
//   - The watch lists the pool's machines every 60 seconds. It sends dispatched again
//     for each machine, and it cleans up each machine whose dispatch ended, that
//     stopped, or that ran longer than its lifetime and 15 minutes. It also cleans up
//     a machine that is deleting, or that is still not listed 5 minutes after its
//     make, with no read of its log. It adopts the listed machine of a claimed entry
//     that stays because its release did not return 204. For each listed machine
//     that has no journal entry and a key in the shape of a dispatch id, it sends
//     dispatched and, on 204, takes the machine back with a journal entry. It cleans
//     up every other listed machine that has no journal entry.
//
// To clean up a machine, the loop reads the machine's log through the adapter,
// uploads it to Qory Apiary, deletes the machine and sends machine-end, which tells
// Qory Apiary how the removal went. Each step runs after a failure of the step before
// it. When the delete returns gone, the loop sends gone and removes the journal entry.
// Otherwise the entry moves to the phase deleting, and each watch cleans up the machine
// again. A log read that failed runs again only until a delete is accepted. When the
// post of gone fails, the entry moves to the phase ended, and the watch sends gone
// again until Qory Apiary accepts it, with waits up to [GoneRetryMax]. A claimed
// entry that never had a listed machine gets gone with no delete once its lifetime
// and 15 minutes have passed. Each post of machine-end is one try, the first failure
// pauses the posts until the next watch, and a deleting or delete_failed that keeps
// failing stops at [MachineEndLimit].
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
	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/hive/internal/journal"
	"github.com/qoryai/integrations/contracts"
)

// The times of the loop.
const (
	// WatchInterval is the time between two watches. It is also the time between two
	// dispatched of a machine, as the provisioner contract requires.
	WatchInterval = 60 * time.Second
	// LifetimeGrace is the time after a machine's lifetime, counted from the start
	// time in the adapter's list, after which the watch cleans up the machine.
	LifetimeGrace = 15 * time.Minute
	// ListGrace is the time during which the watch keeps an entry whose machine is
	// not in the adapter's list. A vendor's list can lag behind its make, so a
	// machine that was just made can be missing from the list for a while. The time
	// counts from the make that returned the machine, or from the start of [Run] for
	// an entry that the journal had at the start.
	ListGrace = 5 * time.Minute
	// MachineEndLimit is the time during which the loop keeps sending a failing
	// deleting or delete_failed, counted from the first failure of that state. A
	// restart starts the count again. After it, the state is dropped, as the contract
	// allows for a call that keeps failing. Gone has no limit: Qory Apiary keeps the
	// dispatch's pool slot until it accepts gone.
	MachineEndLimit = 24 * time.Hour
	// GoneRetryMax is the longest wait between two posts of gone for an ended entry.
	// After the first failed post, the next goes out [WatchInterval] later, and each
	// later failure doubles the wait, up to GoneRetryMax. A restart starts the waits
	// again.
	GoneRetryMax = time.Hour
	// LongWait is the wait before the next claim after a make that failed with the
	// kind limit or defect, after the start data of a claim could not be built, after
	// a claim for another pool than the provisioner's, and after a claim with no
	// machine spec.
	LongWait = 60 * time.Second
	// ShortWait is the wait before the next claim after a make that failed with the
	// kind retry or other, and after a claim that failed.
	ShortWait = 15 * time.Second
	// releaseTimeout bounds a release, and the upload of a make's error after it.
	// Both run also after the context ends.
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
	// ErrForbidden is a signed 403 forbidden: the dispatch is unknown or of another node
	// pool.
	ErrForbidden = errors.New("403 forbidden")
	// ErrMachineEndUnusable is a signed answer to machine-end that breaks the
	// contract, such as a 2xx other than 204. A post of deleting or delete_failed
	// counts as done. A post of gone counts as failed and is sent again, because gone
	// needs the 204.
	ErrMachineEndUnusable = errors.New("a signed machine-end answer that breaks the contract")
	// ErrMachineEndStopped is a signed 400, 403 or 404 to machine-end. The post counts as
	// done, the dispatch gets no more machine-end, and its deletes go on.
	ErrMachineEndStopped = errors.New("machine-end is stopped for the dispatch")
)

// Claim is a claim answer: a request for one machine.
type Claim struct {
	// DispatchID is the dispatch, dp_ and 16 characters. It is the key of the machine.
	DispatchID string
	// PoolID is the node pool whose key made the claim, np_ and 16 characters.
	PoolID string
	// LeaseUntil is the time before which the first dispatched must arrive.
	LeaseUntil time.Time
	// Machine is the run's machine type, a name such as standard, or "" when the
	// claim has none.
	Machine string
	// MachineSpec is the spec of the run's machine type, which the make passes to the
	// adapter unchanged, or nil when the claim has none. The loop releases a claim
	// without one, because each claim has offers.
	MachineSpec *contracts.MachineSpec
	// RunCode is the run code that the machine redeems.
	RunCode RunCode
	// GatewayURL is the gateway's URL that the machine passes to qory run.
	GatewayURL string
	// MaxLifetimeMinutes is the time after which the machine powers off, 15 to 1470.
	MaxLifetimeMinutes int
}

// Apiary is the provisioner's client of Qory Apiary: the five requests of the
// provisioner contract that the loop sends.
type Apiary interface {
	// Claim claims a request for one machine, with offers, the machines the adapter
	// can make. It returns false and no error when no work could be claimed within the
	// claim's wait.
	Claim(ctx context.Context, offers []contracts.MachineSpec) (Claim, bool, error)
	// Dispatched sends dispatched for the dispatch, whose machine is machineID, of the
	// vendor's type machineType, or "" when no make returned one. Its error wraps
	// [ErrGone], [ErrLeaseExpired] or [ErrForbidden] for those refusals.
	Dispatched(ctx context.Context, dispatchID, machineID, machineType string) error
	// Release gives the dispatch back, with a reason of 1 to 200 characters. Its error
	// wraps [ErrGone], [ErrLeaseExpired] or [ErrMachineExists] for those refusals.
	Release(ctx context.Context, dispatchID, reason string) error
	// MachineLog uploads the machine's log of the dispatch, or the error of a make
	// that made no machine. truncated is true when the log is the last part of a
	// longer one.
	MachineLog(ctx context.Context, dispatchID, log string, truncated bool) error
	// MachineEnd sends how the removal of the dispatch's machine went, with one try and
	// no retries: deleting, gone or delete_failed. Its error wraps
	// [ErrMachineEndUnusable] or [ErrMachineEndStopped] for those answers.
	MachineEnd(ctx context.Context, dispatchID, state string) error
}

// Journal is the journal of live dispatches, as [journal.Journal] keeps it.
type Journal interface {
	// List returns the entries, in the order of their claims.
	List() []journal.Entry
	// Set writes the entry in place of the entry of its dispatch, or after the others.
	Set(e journal.Entry) error
	// Remove takes the entry of the dispatch id out of the journal. It can return an
	// error after the entry left the list, when syncing the folder fails.
	Remove(dispatchID string) error
}

var _ Journal = (*journal.Journal)(nil)

// Machines is the runner of the machine adapter's commands, as [adapter.Runner] runs
// them. A failed command returns an *adapter.Error with the kind of failure.
type Machines interface {
	// Check shows that the adapter's rights and settings work, and returns the
	// machines the adapter can make, its offers.
	Check(ctx context.Context) ([]contracts.MachineSpec, error)
	// Make makes one machine that fits spec in pool for key and delivers startData to
	// it.
	Make(ctx context.Context, key, pool string, spec contracts.MachineSpec, startData []byte) (contracts.MachinesMakeAnswer, error)
	// List lists the machines of pool.
	List(ctx context.Context, pool string) (contracts.MachinesListAnswer, error)
	// Log reads the console text of the machine id.
	Log(ctx context.Context, id string) (contracts.MachinesLogAnswer, error)
	// Delete deletes the machine id.
	Delete(ctx context.Context, id string) (contracts.MachinesDeleteAnswer, error)
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
	Journal Journal
	// PoolID is the node pool of the provisioner's access key, np_ and 16 characters.
	PoolID string
	// GatewayAddress is gateway_address of the settings, which the start data contains.
	GatewayAddress netip.Addr
	// Log receives the loop's log lines.
	Log *slog.Logger
	// Clock is the source of time. Nil is the system's clock.
	Clock Clock
}

// Run runs the loop until ctx ends, and then returns nil. It returns an error that
// stops the program when the adapter's check fails, when listing the machines for the
// recovery fails, when the recovery cannot write a resumed entry to the journal, and
// when the journal cannot be written before a make or after it.
func Run(ctx context.Context, cfg Config) error {
	l := &loop{
		Config:    cfg,
		busy:      map[string]bool{},
		logged:    map[string]bool{},
		sentEnd:   map[string]string{},
		failedEnd: map[string]endFailure{},
		made:      map[string]time.Time{},
		claimed:   map[string]time.Time{},
		goneRetry: map[string]goneRetry{},
		pending:   map[string]string{},
		goneDone:  map[string]time.Time{},
	}
	if l.Clock == nil {
		l.Clock = systemClock{}
	}
	l.start = l.Clock.Now()
	offers, err := l.Machines.Check(ctx)
	if err != nil {
		return fmt.Errorf("Fix the machine adapter's settings or the rights it uses, then start the provisioner again: the machine adapter's check failed: %s", shown(err, nil))
	}
	l.offers = offers
	if err := l.recoverJournal(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() { l.watch(ctx) })
	err = l.claim(ctx)
	cancel()
	wg.Wait()
	return err
}

// endFailure is one machine-end state that keeps failing, and when it first failed.
type endFailure struct {
	state string
	at    time.Time
}

// goneRetry is the number of failed posts of gone for an ended entry, and the time
// before which the watch sends gone for it no more.
type goneRetry struct {
	failures int
	next     time.Time
}

// loop is one run of the loop.
type loop struct {
	Config
	// offers are the machines the adapter can make, from its check at the start of
	// Run. Each claim sends them.
	offers []contracts.MachineSpec
	mu     sync.Mutex
	// busy are the dispatches that the claim path works on. The watch skips them.
	busy map[string]bool
	// logged are the machine ids that get no more log reads in a cleanup: the read
	// succeeded, a delete was accepted, or the machine reached its cleanup with no log
	// read. An id leaves when its entry leaves the journal.
	logged map[string]bool
	// sentEnd is the last machine-end state done for each entry, so that a state goes
	// out once per change, or [machineEndStopped]. A restart starts with no record and
	// sends the state once more.
	sentEnd map[string]string
	// failedEnd is the deleting or delete_failed that each entry has not got done, with
	// the time of its first failure.
	failedEnd map[string]endFailure
	// endsPaused is true after a failed machine-end, until the next watch starts. The
	// claim path reads and sets it too.
	endsPaused bool
	// made is the time of the make that returned each entry's machine, and claimed
	// the time of each entry's claim. An entry that the journal had at the start has
	// no time in them and counts from start.
	made    map[string]time.Time
	claimed map[string]time.Time
	// goneRetry is the wait of each ended entry or pending stray whose post of gone
	// failed. One with none gets gone at the next watch.
	goneRetry map[string]goneRetry
	// pending is the machine id that each pending stray still deletes, or "" when only
	// its gone is left, by dispatch id, as [loop.watchPending] describes.
	pending map[string]string
	// goneDone is the time at which gone was done for each dispatch id, for
	// [ListGrace].
	goneDone map[string]time.Time
	start    time.Time
}

// madeAt is the time from which the list grace of the dispatch id counts.
func (l *loop) madeAt(id string) time.Time { return l.timeOf(l.made, id) }

// claimedAt is the time of the claim of the dispatch id, or the start of Run, for an
// entry that the journal had at the start.
func (l *loop) claimedAt(id string) time.Time { return l.timeOf(l.claimed, id) }

// timeOf is the time of the dispatch id in times, or the start.
func (l *loop) timeOf(times map[string]time.Time, id string) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := times[id]; ok {
		return t
	}
	return l.start
}

// setTime sets the time of the dispatch id in times to now.
func (l *loop) setTime(times map[string]time.Time, id string) {
	now := l.Clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	times[id] = now
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

// remove takes the dispatch out of the journal. An entry that has left the journal's
// list counts as removed, also when Remove returned an error after the entry left, as
// when syncing the folder fails. A failure is logged. When the entry is still in the
// list, it stays, and the next watch acts on it again. remove returns whether the
// entry has left the list.
func (l *loop) remove(id string) bool {
	if err := l.Journal.Remove(id); err != nil {
		l.Log.Error("removing a dispatch from the journal failed", slog.String("dispatch_id", id), errorAttr(err, nil))
	}
	for _, e := range l.Journal.List() {
		if e.DispatchID == id {
			return false
		}
	}
	l.mu.Lock()
	delete(l.made, id)
	delete(l.claimed, id)
	delete(l.goneRetry, id)
	l.mu.Unlock()
	l.forgetEnds(id)
	return true
}

// forgetEnds drops the record of the machine-end posts of the dispatch id.
func (l *loop) forgetEnds(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.sentEnd, id)
	delete(l.failedEnd, id)
}

// release releases the dispatch with reason. It runs also after ctx ends, for a
// claim whose make has not started. A failure is logged: a release has no retry. It
// returns the error of [Apiary.Release], nil on 204. An error that wraps
// [ErrMachineExists] means that a machine redeemed the run code, so the dispatch is
// not released.
func (l *loop) release(ctx context.Context, id, reason string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	err := l.Apiary.Release(ctx, id, reason)
	switch {
	case err == nil:
		l.Log.Info("released a dispatch", slog.String("dispatch_id", id))
	case errors.Is(err, ErrMachineExists):
		l.Log.Warn("the release returned machine_exists: a machine redeemed the run code", slog.String("dispatch_id", id))
	default:
		l.Log.Warn("releasing a dispatch failed", slog.String("dispatch_id", id), errorAttr(err, nil))
	}
	return err
}

// releaseEntry releases the dispatch of a claimed entry, and returns the error of the
// release, as release returns it. Only a release that returns 204 removes the entry.
// After any other answer, such as 409 machine_exists, 410 gone, 409 lease_expired or
// a failed request, a machine may exist, so the entry stays in the phase claimed. The
// watch then adopts a machine listed with its key, and no watch deletes it as a stray.
// When no machine is listed once the entry's lifetime and 15 minutes have passed, the
// watch sends gone for the dispatch with no delete.
func (l *loop) releaseEntry(ctx context.Context, id, reason string) error {
	err := l.release(ctx, id, reason)
	if err == nil {
		l.remove(id)
	}
	return err
}

// hiddenError replaces an error's text that contains what looks like a secret.
const hiddenError = "the error's text contains what looks like a run code, a run key, an access key secret or the start data, so the provisioner does not show it"

// shown is err's text, or [hiddenError] when the text contains what looks like a run
// code, a piece of startData, a run key or an access key secret.
func shown(err error, startData []byte) string {
	text := err.Error()
	lower := strings.ToLower(text)
	if adapterio.ContainsSecret(text, startData) || strings.Contains(lower, "qrk_") || strings.Contains(lower, "qak_") {
		return hiddenError
	}
	return text
}

// errorAttr is the attribute error with err's text as [shown] gives it.
func errorAttr(err error, startData []byte) slog.Attr {
	return slog.String("error", shown(err, startData))
}
