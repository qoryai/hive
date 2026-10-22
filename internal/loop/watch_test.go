package loop_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qoryai/hive/internal/adapter"
	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/hive/internal/journal"
	"github.com/qoryai/hive/internal/loop"
	"github.com/qoryai/integrations/contracts"
)

// dispatched is the dispatched entry of the dispatch id and the machine id.
func dispatched(id, machine string) journal.Entry {
	return journal.Entry{DispatchID: id, Phase: journal.PhaseDispatched, MachineID: machine, MaxLifetimeMinutes: lifetime}
}

// TestTheWatchSendsDispatchedAgain sends dispatched for each dispatched machine at
// each watch, and changes nothing else.
func TestTheWatchSendsDispatchedAgain(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.claim(newClaim(dispatchA))
	h.claim(newClaim(dispatchB))
	h.apiary.takeCalls()
	h.machines.takeCalls()
	for range 2 {
		h.tick()
		equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{
			"dispatched " + dispatchA + " " + machineID(1),
			"dispatched " + dispatchB + " " + machineID(2),
		})
		equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
		equal(t, "the journal", h.entries(), []journal.Entry{dispatched(dispatchA, machineID(1)), dispatched(dispatchB, machineID(2))})
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestA410OnTheRepeatCleansUp cleans up a machine whose repeated dispatched returns
// 410 gone, and keeps the other machine.
func TestA410OnTheRepeatCleansUp(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.claim(newClaim(dispatchA))
	h.claim(newClaim(dispatchB))
	h.apiary.takeCalls()
	h.machines.takeCalls()
	h.apiary.setDispatched(dispatchA, fmt.Errorf("Qory Apiary answered: %w", loop.ErrGone))
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{
		"dispatched " + dispatchA + " " + machineID(1),
		"machine-log " + dispatchA + " false",
		"dispatched " + dispatchB + " " + machineID(2),
	})
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"log " + machineID(1), "delete " + machineID(1)})
	equal(t, "the journal", h.entries(), []journal.Entry{dispatched(dispatchB, machineID(2))})
	if len(h.find("the dispatch ended")) != 1 {
		t.Error("the log has no line for the ended dispatch")
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAStoppedMachineIsCleanedUp cleans up a stopped machine without sending
// dispatched.
func TestAStoppedMachineIsCleanedUp(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.claim(newClaim(dispatchA))
	h.apiary.takeCalls()
	h.machines.takeCalls()
	h.machines.setState(machineID(1), contracts.MachineStopped)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-log " + dispatchA + " false"})
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"log " + machineID(1), "delete " + machineID(1)})
	equal(t, "the journal", h.entries(), nil)
	lines := h.find("the machine stopped")
	if len(lines) != 1 || lines[0]["state"] != "stopped" || lines[0]["machine_id"] != machineID(1) || lines[0]["dispatch_id"] != dispatchA {
		t.Errorf("the log lines are %v", lines)
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAnEntryWhoseMachineIsDeletingIsRemoved removes the entry at once, and sends
// nothing and deletes nothing.
func TestAnEntryWhoseMachineIsDeletingIsRemoved(t *testing.T) {
	h := newHarness(t)
	h.setEntries(dispatched(dispatchA, "i-a"))
	h.machines.add("i-a", dispatchA, contracts.MachineRunning, epoch)
	h.start()
	h.machines.setState("i-a", contracts.MachineDeleting)
	h.machines.takeCalls()
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
	equal(t, "the journal", h.entries(), nil)
	lines := h.find("the machine is deleting")
	if len(lines) != 1 || lines[0]["state"] != "deleting" {
		t.Errorf("the log lines are %v", lines)
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// ticksInGrace is the number of watches inside the list grace of a make at the start
// of a test: the watches at 60 s to 300 s.
const ticksInGrace = int(loop.ListGrace / loop.WatchInterval)

// TestANotListedMachineIsKeptInsideTheGrace keeps the entry of a made machine that the
// adapter's list does not contain, for 5 minutes after the make, and sends dispatched
// for it at each watch.
func TestANotListedMachineIsKeptInsideTheGrace(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.claim(newClaim(dispatchA))
	h.machines.drop(machineID(1))
	h.apiary.takeCalls()
	h.machines.takeCalls()
	for range ticksInGrace {
		h.tick()
		equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"dispatched " + dispatchA + " " + machineID(1)})
		equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
		equal(t, "the journal", h.entries(), []journal.Entry{dispatched(dispatchA, machineID(1))})
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestANotListedMachineIsRemovedAfterTheGrace removes the entry at the first watch
// more than 5 minutes after the make, and after the start for an entry that the
// journal had at the start. It sends nothing for the removed entries and deletes
// nothing.
func TestANotListedMachineIsRemovedAfterTheGrace(t *testing.T) {
	h := newHarness(t)
	h.setEntries(dispatched(dispatchB, "i-b"))
	h.start()
	h.claim(newClaim(dispatchA))
	h.machines.drop(machineID(1))
	for range ticksInGrace {
		h.tick()
	}
	equal(t, "the journal", h.entries(), []journal.Entry{dispatched(dispatchB, "i-b"), dispatched(dispatchA, machineID(1))})
	h.apiary.takeCalls()
	h.machines.takeCalls()
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
	equal(t, "the journal", h.entries(), nil)
	lines := h.find("the machine is still not listed 5 minutes after its make")
	if len(lines) != 2 || lines[0]["dispatch_id"] != dispatchB || lines[1]["dispatch_id"] != dispatchA {
		t.Errorf("the log lines are %v", lines)
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAMachineListedLateIsNotAStray lists a made machine only from the second watch
// on. The loop keeps its entry, never deletes the machine and keeps sending
// dispatched.
func TestAMachineListedLateIsNotAStray(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.claim(newClaim(dispatchA))
	h.machines.drop(machineID(1))
	h.apiary.takeCalls()
	h.machines.takeCalls()
	h.tick()
	h.machines.add(machineID(1), dispatchA, contracts.MachineRunning, epoch)
	for range ticksInGrace + 2 {
		h.tick()
	}
	equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
	calls := h.apiary.takeCalls()
	if len(calls) != ticksInGrace+3 || slices.ContainsFunc(calls, func(c string) bool { return c != "dispatched "+dispatchA+" "+machineID(1) }) {
		t.Errorf("Qory Apiary's calls are %q", calls)
	}
	equal(t, "the journal", h.entries(), []journal.Entry{dispatched(dispatchA, machineID(1))})
	if n := len(h.find("found a machine with no journal entry")); n != 0 {
		t.Errorf("the log has %d lines for a stray", n)
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAMachinePastItsLifetimeIsCleanedUp cleans up a machine that has run longer than
// its lifetime and 15 minutes, counted from its start time in the list, and sends
// dispatched for a machine that is exactly at that bound.
func TestAMachinePastItsLifetimeIsCleanedUp(t *testing.T) {
	h := newHarness(t)
	short := func(e journal.Entry) journal.Entry { e.MaxLifetimeMinutes = 15; return e }
	h.setEntries(short(dispatched(dispatchA, "i-a")), short(dispatched(dispatchB, "i-b")))
	// The watch runs at epoch + 60 s. The bound is the start time + 30 minutes.
	at := epoch.Add(loop.WatchInterval - 30*time.Minute)
	h.machines.add("i-a", dispatchA, contracts.MachineRunning, at.Add(-time.Second))
	h.machines.add("i-b", dispatchB, contracts.MachineRunning, at)
	h.start()
	h.machines.takeCalls()
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{
		"machine-log " + dispatchA + " false",
		"dispatched " + dispatchB + " i-b",
	})
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"log i-a", "delete i-a"})
	equal(t, "the journal", h.entries(), []journal.Entry{short(dispatched(dispatchB, "i-b"))})
	lines := h.find("the machine ran longer than its lifetime and 15 minutes")
	if len(lines) != 1 || lines[0]["machine_id"] != "i-a" || lines[0]["level"] != "WARN" {
		t.Errorf("the log lines are %v", lines)
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAStrayIsCleanedUp cleans up a listed machine whose key has no journal entry,
// with its key as the dispatch id, and leaves a stray that is deleting as it is.
func TestAStrayIsCleanedUp(t *testing.T) {
	h := newHarness(t)
	h.machines.logText = "stray log"
	h.start()
	h.machines.add("i-s", dispatchC, contracts.MachineRunning, epoch)
	h.machines.add("i-d", dispatchD, contracts.MachineDeleting, epoch)
	h.machines.takeCalls()
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-log " + dispatchC + " false"})
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"log i-s", "delete i-s"})
	equal(t, "the journal", h.entries(), nil)
	lines := h.find("found a machine with no journal entry")
	if len(lines) != 1 || lines[0]["dispatch_id"] != dispatchC || lines[0]["machine_id"] != "i-s" || lines[0]["state"] != "running" {
		t.Errorf("the log lines are %v", lines)
	}
	// The stray is deleting now, so the next watch leaves it.
	h.tick()
	equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAMachineInFlightIsNotAStray runs a watch while a make is in flight and the
// adapter already lists its machine. The claimed entry keeps the machine, and the
// claim path then records it.
func TestAMachineInFlightIsNotAStray(t *testing.T) {
	h := newHarness(t)
	h.machines.makeStarted = make(chan struct{})
	h.machines.makeGate = make(chan struct{})
	h.start()
	h.send(claimResult{claim: newClaim(dispatchA), ok: true})
	select {
	case <-h.machines.makeStarted:
	case <-time.After(patience):
		t.Fatal("the make did not start")
	}
	h.machines.add("i-early", dispatchA, contracts.MachineStarting, epoch)
	h.tick()
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"check"})
	close(h.machines.makeGate)
	h.idle()
	equal(t, "the journal", h.entries(), []journal.Entry{dispatched(dispatchA, "i-early")})
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAFailedDeleteIsTriedAgain keeps the entry after a failed delete. The next watch
// deletes the machine again, without a second upload of its log.
func TestAFailedDeleteIsTriedAgain(t *testing.T) {
	h := newHarness(t)
	h.machines.logText = "log"
	h.machines.deleteErr = []error{&adapter.Error{Command: "delete", Kind: contracts.FailureRetry, Message: "Try again later: the vendor throttled the call"}}
	h.start()
	h.claim(newClaim(dispatchA))
	h.machines.setState(machineID(1), contracts.MachineStopped)
	h.apiary.takeCalls()
	h.machines.takeCalls()
	h.tick()
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"log " + machineID(1), "delete " + machineID(1)})
	equal(t, "the journal", h.entries(), []journal.Entry{dispatched(dispatchA, machineID(1))})
	lines := h.find("deleting a machine failed")
	if len(lines) != 1 || !strings.HasPrefix(lines[0]["error"].(string), "Try again later") {
		t.Errorf("the log lines are %v", lines)
	}
	h.tick()
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"delete " + machineID(1)})
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-log " + dispatchA + " false"})
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestEachCleanupStepRunsAfterAFailure fails the log's read or upload. The machine is
// deleted and the entry removed all the same.
func TestEachCleanupStepRunsAfterAFailure(t *testing.T) {
	for _, c := range []struct {
		name      string
		readErr   error
		uploadErr error
		uploads   int
		msg       string
	}{
		{"the read fails", &adapter.Error{Command: "log", Kind: contracts.FailureOther, Message: "Check the vendor"}, nil, 0, "reading a machine's log failed"},
		{"the upload fails", nil, errors.New("no answer from Qory Apiary"), 1, "uploading a machine's log failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.machines.logErr = c.readErr
			h.apiary.logErr = c.uploadErr
			h.apiary.setDispatched(dispatchA, loop.ErrGone)
			h.start()
			h.claim(newClaim(dispatchA))
			if len(h.apiary.uploads) != c.uploads {
				t.Errorf("the uploads are %v", h.apiary.uploads)
			}
			equal(t, "the adapter's calls", h.machines.takeCalls(), []string{
				"check",
				"make " + dispatchA + " " + pool + " standard",
				"log " + machineID(1),
				"delete " + machineID(1),
			})
			equal(t, "the journal", h.entries(), nil)
			if len(h.find(c.msg)) != 1 {
				t.Errorf("the log has no line %q", c.msg)
			}
			if err := h.stop(); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestALogAtTheBoundIsTruncated uploads a log of the adapter's 768 KiB bound as
// truncated, and a shorter one as whole.
func TestALogAtTheBoundIsTruncated(t *testing.T) {
	for _, c := range []struct {
		size      int
		truncated bool
	}{
		{adapterio.MaxLog, true},
		{adapterio.MaxLog - 1, false},
		{0, false},
	} {
		t.Run(fmt.Sprint(c.size), func(t *testing.T) {
			h := newHarness(t)
			h.machines.logText = strings.Repeat("a", c.size)
			h.apiary.setDispatched(dispatchA, loop.ErrGone)
			h.start()
			h.claim(newClaim(dispatchA))
			if len(h.apiary.uploads) != 1 || len(h.apiary.uploads[0].log) != c.size || h.apiary.uploads[0].truncated != c.truncated {
				t.Errorf("the upload is not %d bytes with truncated %t", c.size, c.truncated)
			}
			if err := h.stop(); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestAListFailureSkipsTheWatch logs the failure, changes nothing, and lists again at
// the next watch.
func TestAListFailureSkipsTheWatch(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.claim(newClaim(dispatchA))
	h.apiary.takeCalls()
	h.machines.mu.Lock()
	h.machines.listErr = &adapter.Error{Command: "list", Kind: contracts.FailureRetry, Message: "Try again later"}
	h.machines.mu.Unlock()
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	equal(t, "the journal", h.entries(), []journal.Entry{dispatched(dispatchA, machineID(1))})
	if len(h.find("listing the pool's machines failed")) != 1 {
		t.Error("the log has no line for the failed list")
	}
	h.machines.mu.Lock()
	h.machines.listErr = nil
	h.machines.mu.Unlock()
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"dispatched " + dispatchA + " " + machineID(1)})
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAFailedFolderSyncStillLetsTheLoopClaim fills the journal with max_machines 1,
// so the claim path waits for an entry to leave. A watch then cleans up the machine,
// whose dispatch ended, and the removal of its entry returns the error of a failed
// sync of the folder after the entry left the journal. The loop claims again.
func TestAFailedFolderSyncStillLetsTheLoopClaim(t *testing.T) {
	h := newHarness(t)
	h.config.MaxMachines = 1
	failing := &syncFailure{}
	h.wrap = func(j loop.Journal) loop.Journal { failing.Journal = j; return failing }
	h.start()
	h.send(claimResult{claim: newClaim(dispatchA), ok: true})
	sent := func() int {
		return len(slices.DeleteFunc(h.apiary.takeCallsKeep(), func(c string) bool {
			return c != "dispatched "+dispatchA+" "+machineID(1)
		}))
	}
	// The fake clock stands still until the claim path has logged "sent dispatched",
	// so that no watch runs while the claim path works on the dispatch. No watch has
	// run yet, so the line is the claim path's.
	for deadline := time.Now().Add(patience); len(h.find("sent dispatched")) < 1; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the claim path did not log sent dispatched")
		}
	}
	// After that line, the claim path only returns and leaves the dispatch, and then
	// waits for an entry to leave the journal. A watch skips the dispatch while the
	// claim path works on it, so the test runs up to 30 watches, 10 ms apart, until
	// one sends dispatched: 30 minutes of the fake clock, below the 75 minutes of the
	// lifetime and 15 minutes.
	for n := 0; sent() < 2; n++ {
		if n == 30 {
			t.Fatal("no watch sent dispatched")
		}
		time.Sleep(10 * time.Millisecond)
		h.tick()
	}
	if n := h.apiary.claimCount(); n != 1 {
		t.Fatalf("the loop asked for %d claims with a full journal", n)
	}
	failing.setFail()
	h.apiary.setDispatched(dispatchA, loop.ErrGone)
	h.tick()
	equal(t, "the journal", h.entries(), nil)
	h.idle()
	if len(h.find("removing a dispatch from the journal failed")) != 1 {
		t.Error("the log has no line for the failed removal")
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// machineExists is the error of a release that returns 409 machine_exists.
var machineExists = fmt.Errorf("Qory Apiary answered: %w", loop.ErrMachineExists)

// TestAMakeFailureWithAMachineThatExistsKeepsTheEntry fails a make after the vendor
// made the machine, and the release returns 409 machine_exists. The claimed entry
// stays, the next watch adopts the listed machine and sends dispatched, and a later
// 410 gone cleans the machine up.
func TestAMakeFailureWithAMachineThatExistsKeepsTheEntry(t *testing.T) {
	h := newHarness(t)
	h.machines.makeErr = &adapter.Error{Command: "make", Kind: contracts.FailureRetry, Message: "Try again later: the vendor's answer was lost"}
	h.apiary.releaseErr = machineExists
	h.start()
	h.send(claimResult{claim: newClaim(dispatchA), ok: true})
	h.clock.waitPending(t, loop.ShortWait, loop.WatchInterval)
	claimed := journal.Entry{DispatchID: dispatchA, Phase: journal.PhaseClaimed, MaxLifetimeMinutes: lifetime}
	equal(t, "the journal", h.entries(), []journal.Entry{claimed})
	if len(h.find("the release returned machine_exists: a machine redeemed the run code")) != 1 || len(h.find("released a dispatch")) != 0 {
		t.Error("the log has no line for machine_exists, or a line for a release")
	}
	h.machines.add("i-late", dispatchA, contracts.MachineRunning, epoch)
	h.apiary.takeCalls()
	h.machines.takeCalls()
	h.clock.Advance(loop.ShortWait)
	h.idle()
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"dispatched " + dispatchA + " i-late"})
	equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
	equal(t, "the journal", h.entries(), []journal.Entry{dispatched(dispatchA, "i-late")})
	lines := h.find("adopted the machine of a claimed dispatch")
	if len(lines) != 1 || lines[0]["machine_id"] != "i-late" {
		t.Errorf("the log lines are %v", lines)
	}
	h.apiary.setDispatched(dispatchA, loop.ErrGone)
	h.tick()
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"log i-late", "delete i-late"})
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestARestartReleaseWithAMachineThatExistsKeepsTheEntry restarts with a claimed
// entry whose machine the list does not have yet, and the release returns 409
// machine_exists. The entry stays, and once the list has the machine, the watch adopts
// it and never deletes it as a stray.
func TestARestartReleaseWithAMachineThatExistsKeepsTheEntry(t *testing.T) {
	h := newHarness(t)
	claimed := journal.Entry{DispatchID: dispatchA, Phase: journal.PhaseClaimed, MaxLifetimeMinutes: lifetime}
	h.setEntries(claimed)
	h.apiary.releaseErr = machineExists
	h.start()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"release " + dispatchA + " " + journal.ReasonRestarted})
	equal(t, "the journal", h.entries(), []journal.Entry{claimed})
	h.tick()
	equal(t, "the journal", h.entries(), []journal.Entry{claimed})
	h.machines.add("i-late", dispatchA, contracts.MachineRunning, epoch)
	h.machines.takeCalls()
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"dispatched " + dispatchA + " i-late"})
	equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
	equal(t, "the journal", h.entries(), []journal.Entry{dispatched(dispatchA, "i-late")})
	if len(h.find("found a machine with no journal entry")) != 0 {
		t.Error("the machine was taken for a stray")
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAKeptClaimedEntryIsRemovedAfterItsLifetime keeps a claimed entry whose release
// returned 409 machine_exists while no list has its machine, until its lifetime and
// 15 minutes have passed since the claim, and then removes it.
func TestAKeptClaimedEntryIsRemovedAfterItsLifetime(t *testing.T) {
	h := newHarness(t)
	h.machines.makeErr = &adapter.Error{Command: "make", Kind: contracts.FailureRetry, Message: "Try again later"}
	h.apiary.releaseErr = machineExists
	h.start()
	c := newClaim(dispatchA)
	c.MaxLifetimeMinutes = 15
	h.send(claimResult{claim: c, ok: true})
	h.clock.waitPending(t, loop.ShortWait, loop.WatchInterval)
	h.clock.Advance(loop.ShortWait)
	h.idle()
	// The claim was at epoch and the clock is at 15 s, so the watches run at 75 s,
	// 135 s and so on. The bound is epoch + 30 minutes: 29 watches keep the entry,
	// and the 30th, at 1815 s, removes it.
	for range 29 {
		h.tick()
	}
	equal(t, "the journal", h.entries(), []journal.Entry{{DispatchID: dispatchA, Phase: journal.PhaseClaimed, MaxLifetimeMinutes: 15}})
	h.tick()
	equal(t, "the journal", h.entries(), nil)
	if len(h.find("no machine is listed for the claimed dispatch after its lifetime and 15 minutes")) != 1 {
		t.Error("the log has no line for the removal")
	}
	equal(t, "the adapter's calls", slices.DeleteFunc(h.machines.takeCalls(), func(c string) bool { return c == "check" || strings.HasPrefix(c, "make ") }), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}
