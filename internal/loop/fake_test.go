package loop_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qoryai/hive/internal/journal"
	"github.com/qoryai/hive/internal/loop"
	"github.com/qoryai/integrations/contracts"
)

const (
	pool      = "np_0123456789abcdef"
	dispatchA = "dp_0123456789abcdef"
	dispatchB = "dp_ghjkmnpqrstvwxyz"
	dispatchC = "dp_abcdefghjkmnpq01"
	dispatchD = "dp_abcdefghjkmnpq02"
	dispatchE = "dp_abcdefghjkmnpq03"
	dispatchF = "dp_abcdefghjkmnpq04"
	// runCode is a marker run code in the shape of the contract. No other test value
	// contains 12 of its characters in a row, the window of adapterio.ContainsSecret.
	runCode    = "qecr_MARKER00000000000000000000.MARKERruncode000000000"
	gatewayURL = "https://gateway.example.com:8443"
	lifetime   = 60
)

// patience bounds each wait of a test for the loop, in real time. A correct loop
// never makes a test wait this long.
const patience = 10 * time.Second

// epoch is the fake clock's time at the start of a test.
var epoch = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

// fakeClock is a clock whose time moves only by Advance.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
	changed chan struct{}
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

func newClock() *fakeClock { return &fakeClock{now: epoch, changed: make(chan struct{})} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waiters = append(c.waiters, waiter{c.now.Add(d), ch})
	c.signal()
	return ch
}

// signal wakes each waitPending. c.mu is held.
func (c *fakeClock) signal() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// Advance moves the time by d and fires each waiter that is due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)
		} else {
			w.ch <- c.now
		}
	}
	c.waiters = kept
	c.signal()
}

// pending is the time left for each waiter, in order.
func (c *fakeClock) pending() []time.Duration {
	var out []time.Duration
	for _, w := range c.waiters {
		out = append(out, w.at.Sub(c.now))
	}
	slices.Sort(out)
	return out
}

// waitPending waits until the times left of the waiters are want, in order.
func (c *fakeClock) waitPending(t *testing.T, want ...time.Duration) {
	t.Helper()
	slices.Sort(want)
	deadline := time.After(patience)
	for {
		c.mu.Lock()
		got, changed := c.pending(), c.changed
		c.mu.Unlock()
		if slices.Equal(got, want) {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("the clock's waiters have %v left, want %v", got, want)
		}
	}
}

// claimResult is one answer of the fake Claim. before runs inside Claim, before it
// returns.
type claimResult struct {
	claim  loop.Claim
	ok     bool
	err    error
	before func()
}

// upload is one machine log that the fake Qory Apiary received.
type upload struct {
	dispatchID string
	log        string
	truncated  bool
}

// fakeApiary is Qory Apiary. Claim takes each answer from claims, which has no buffer,
// so a test's send returns once the loop asks for a claim.
type fakeApiary struct {
	claims     chan claimResult
	mu         sync.Mutex
	calls      []string
	claimed    int
	dispatched map[string]error
	releaseErr error
	logErr     error
	uploads    []upload
}

func newApiary() *fakeApiary {
	return &fakeApiary{claims: make(chan claimResult), dispatched: map[string]error{}}
}

func (a *fakeApiary) Claim(ctx context.Context) (loop.Claim, bool, error) {
	a.mu.Lock()
	a.claimed++
	a.mu.Unlock()
	select {
	case r := <-a.claims:
		if r.before != nil {
			r.before()
		}
		return r.claim, r.ok, r.err
	case <-ctx.Done():
		return loop.Claim{}, false, ctx.Err()
	}
}

func (a *fakeApiary) Dispatched(_ context.Context, dispatchID, machineID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, "dispatched "+dispatchID+" "+machineID)
	return a.dispatched[dispatchID]
}

func (a *fakeApiary) Release(_ context.Context, dispatchID, reason string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, "release "+dispatchID+" "+reason)
	return a.releaseErr
}

func (a *fakeApiary) MachineLog(_ context.Context, dispatchID, log string, truncated bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, fmt.Sprintf("machine-log %s %t", dispatchID, truncated))
	a.uploads = append(a.uploads, upload{dispatchID, log, truncated})
	return a.logErr
}

// setDispatched sets the error of each later dispatched of the dispatch.
func (a *fakeApiary) setDispatched(dispatchID string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dispatched[dispatchID] = err
}

// takeCalls returns the calls since the last takeCalls.
func (a *fakeApiary) takeCalls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.calls
	a.calls = nil
	return out
}

// takeCallsKeep returns the calls since the last takeCalls, and keeps them.
func (a *fakeApiary) takeCallsKeep() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.calls)
}

// claimCount is the number of calls of Claim.
func (a *fakeApiary) claimCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.claimed
}

// fakeMachines is a machine adapter with machines in memory.
type fakeMachines struct {
	mu        sync.Mutex
	clock     *fakeClock
	machines  []contracts.Machine
	made      int
	calls     []string
	checkErr  error
	listErr   error
	makeErr   error
	logErr    error
	deleteErr []error
	logText   string
	startData []byte
	// makeStarted, when set, receives a value as make starts, and make then waits
	// for makeGate.
	makeStarted chan struct{}
	makeGate    chan struct{}
}

func (m *fakeMachines) Check(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "check")
	return m.checkErr
}

func (m *fakeMachines) Make(_ context.Context, key, pool, size string, startData []byte) (contracts.MachinesMakeAnswer, error) {
	if m.makeStarted != nil {
		m.makeStarted <- struct{}{}
		<-m.makeGate
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "make "+key+" "+pool+" "+size)
	m.startData = bytes.Clone(startData)
	if m.makeErr != nil {
		return contracts.MachinesMakeAnswer{}, m.makeErr
	}
	// One key makes at most one machine.
	for _, x := range m.machines {
		if x.Key == key {
			return contracts.MachinesMakeAnswer{ID: x.ID}, nil
		}
	}
	m.made++
	id := machineID(m.made)
	m.machines = append(m.machines, contracts.Machine{ID: id, Key: key, State: contracts.MachineRunning, Started: contracts.FormatTime(m.clock.Now())})
	return contracts.MachinesMakeAnswer{ID: id}, nil
}

func (m *fakeMachines) List(_ context.Context, p string) (contracts.MachinesListAnswer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "list "+p)
	if m.listErr != nil {
		return contracts.MachinesListAnswer{}, m.listErr
	}
	return contracts.MachinesListAnswer{Machines: slices.Clone(m.machines)}, nil
}

func (m *fakeMachines) Log(_ context.Context, id string) (contracts.MachinesLogAnswer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "log "+id)
	if m.logErr != nil {
		return contracts.MachinesLogAnswer{}, m.logErr
	}
	return contracts.MachinesLogAnswer{Log: m.logText}, nil
}

func (m *fakeMachines) Delete(_ context.Context, id string) (contracts.MachinesDeleteAnswer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "delete "+id)
	if len(m.deleteErr) > 0 {
		err := m.deleteErr[0]
		m.deleteErr = m.deleteErr[1:]
		if err != nil {
			return contracts.MachinesDeleteAnswer{}, err
		}
	}
	for i := range m.machines {
		if m.machines[i].ID == id {
			m.machines[i].State = contracts.MachineDeleting
		}
	}
	return contracts.MachinesDeleteAnswer{State: contracts.MachineDeleting}, nil
}

// add adds a machine to the adapter's list.
func (m *fakeMachines) add(id, key, state string, started time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.machines = append(m.machines, contracts.Machine{ID: id, Key: key, State: state, Started: contracts.FormatTime(started)})
}

// drop takes the machine id out of the adapter's list.
func (m *fakeMachines) drop(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.machines = slices.DeleteFunc(m.machines, func(x contracts.Machine) bool { return x.ID == id })
}

// setState sets the state of the machine id.
func (m *fakeMachines) setState(id string, state string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.machines {
		if m.machines[i].ID == id {
			m.machines[i].State = state
		}
	}
}

// takeCalls returns the calls since the last takeCalls, without list.
func (m *fakeMachines) takeCalls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := slices.DeleteFunc(m.calls, func(c string) bool { return strings.HasPrefix(c, "list ") })
	m.calls = nil
	return out
}

// lastStartData is the start data of the last make.
func (m *fakeMachines) lastStartData() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startData
}

// syncBuffer is a buffer that several goroutines write.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// harness is one run of the loop with fakes.
type harness struct {
	t        *testing.T
	clock    *fakeClock
	apiary   *fakeApiary
	machines *fakeMachines
	journal  *journal.Journal
	dir      string
	logs     *syncBuffer
	cancel   context.CancelFunc
	done     chan error
	config   loop.Config
	// wrap, when set, wraps the journal that the loop uses.
	wrap func(loop.Journal) loop.Journal
}

// newHarness returns a harness with an empty journal and no machine, and with
// max_machines 10. The loop starts at start.
func newHarness(t *testing.T) *harness {
	t.Helper()
	clock := newClock()
	h := &harness{
		t:        t,
		clock:    clock,
		apiary:   newApiary(),
		machines: &fakeMachines{clock: clock},
		dir:      t.TempDir(),
		logs:     &syncBuffer{},
		done:     make(chan error, 1),
	}
	h.openJournal()
	h.config = loop.Config{
		PoolID:         pool,
		MaxMachines:    10,
		GatewayAddress: netip.MustParseAddr("10.0.1.5"),
	}
	return h
}

// openJournal opens the journal in the harness's folder.
func (h *harness) openJournal() {
	h.t.Helper()
	j, err := journal.Open(h.dir)
	if err != nil {
		h.t.Fatal(err)
	}
	h.journal = j
}

// setEntries writes the entries into the journal.
func (h *harness) setEntries(entries ...journal.Entry) {
	h.t.Helper()
	for _, e := range entries {
		if err := h.journal.Set(e); err != nil {
			h.t.Fatal(err)
		}
	}
}

// start runs the loop and waits until the watch waits for its first tick.
func (h *harness) start() {
	h.t.Helper()
	h.startNoWait()
	h.clock.waitPending(h.t, loop.WatchInterval)
}

// startNoWait runs the loop.
func (h *harness) startNoWait() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.t.Cleanup(cancel)
	cfg := h.config
	cfg.Apiary, cfg.Machines, cfg.Journal, cfg.Clock = h.apiary, h.machines, h.journal, h.clock
	if h.wrap != nil {
		cfg.Journal = h.wrap(h.journal)
	}
	cfg.Log = slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	go func() { h.done <- loop.Run(ctx, cfg) }()
}

// restart runs the loop again after Run returned, with the journal read again from
// the folder, a new clock at epoch, and the same adapter and Qory Apiary.
func (h *harness) restart() {
	h.t.Helper()
	h.clock = newClock()
	h.machines.mu.Lock()
	h.machines.clock = h.clock
	h.machines.makeStarted, h.machines.makeGate = nil, nil
	h.machines.mu.Unlock()
	h.openJournal()
	h.start()
}

// stop cancels the loop's context and returns Run's error.
func (h *harness) stop() error {
	h.t.Helper()
	h.cancel()
	return h.result()
}

// result waits for Run to return.
func (h *harness) result() error {
	h.t.Helper()
	select {
	case err := <-h.done:
		return err
	case <-time.After(patience):
		h.t.Fatal("the loop did not return")
		return nil
	}
}

// send gives the loop the claim answer r, once it asks for a claim.
func (h *harness) send(r claimResult) {
	h.t.Helper()
	select {
	case h.apiary.claims <- r:
	case err := <-h.done:
		h.t.Fatalf("the loop returned %v before it asked for a claim", err)
	case <-time.After(patience):
		h.t.Fatal("the loop did not ask for a claim")
	}
}

// claim gives the loop the claim c, and then an answer with no work, so that the
// claim is handled when claim returns.
func (h *harness) claim(c loop.Claim) {
	h.t.Helper()
	h.send(claimResult{claim: c, ok: true})
	h.idle()
}

// idle gives the loop an answer with no work, so that it has handled each claim
// before.
func (h *harness) idle() {
	h.t.Helper()
	h.send(claimResult{})
}

// tick runs one watch and waits until it is done, while the claim path waits for a
// claim.
func (h *harness) tick() {
	h.t.Helper()
	h.clock.Advance(loop.WatchInterval)
	h.clock.waitPending(h.t, loop.WatchInterval)
}

// entries is the journal's entries.
func (h *harness) entries() []journal.Entry { return h.journal.List() }

// logLine is one line of the loop's log.
type logLine map[string]any

// lines is the loop's log lines.
func (h *harness) lines() []logLine {
	h.t.Helper()
	var out []logLine
	for line := range strings.Lines(h.logs.String()) {
		var l logLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			h.t.Fatalf("the log line %q is not JSON: %v", line, err)
		}
		out = append(out, l)
	}
	return out
}

// find returns the log lines with the message msg.
func (h *harness) find(msg string) []logLine {
	h.t.Helper()
	var out []logLine
	for _, l := range h.lines() {
		if l["msg"] == msg {
			out = append(out, l)
		}
	}
	return out
}

// newClaim is a claim for the dispatch id in the pool, with no machine type.
func newClaim(id string) loop.Claim {
	return loop.Claim{
		DispatchID:         id,
		PoolID:             pool,
		LeaseUntil:         epoch.Add(5 * time.Minute),
		RunCode:            loop.NewRunCode(runCode),
		GatewayURL:         gatewayURL,
		MaxLifetimeMinutes: lifetime,
	}
}

// machineID is the id of the nth machine that the fake adapter makes.
func machineID(n int) string { return fmt.Sprintf("i-0123456789abcd%03d", n) }

// syncFailure is a journal whose next Remove removes the entry and then returns the
// error of a failed sync of the folder, as journal.Journal returns it.
type syncFailure struct {
	loop.Journal
	mu   sync.Mutex
	fail bool
}

// setFail makes the next Remove return the error.
func (j *syncFailure) setFail() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.fail = true
}

func (j *syncFailure) Remove(id string) error {
	if err := j.Journal.Remove(id); err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.fail {
		j.fail = false
		return errors.New("check the state folder: the journal of live dispatches is written, but syncing its folder failed: input/output error")
	}
	return nil
}
