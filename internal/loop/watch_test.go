package loop_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/qoryai/hive/internal/adapter"
	"github.com/qoryai/hive/internal/journal"
	"github.com/qoryai/hive/internal/loop"
	"github.com/qoryai/hive/machines"
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
	h.machines.setState(machineID(1), machines.StateStopped)
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

// TestAnEntryWhoseMachineIsDeletingOrNotListedIsRemoved removes the entry, and sends
// nothing and deletes nothing.
func TestAnEntryWhoseMachineIsDeletingOrNotListedIsRemoved(t *testing.T) {
	h := newHarness(t)
	h.setEntries(dispatched(dispatchA, "i-a"), dispatched(dispatchB, "i-b"))
	h.machines.add("i-a", dispatchA, machines.StateRunning, epoch)
	h.machines.add("i-b", dispatchB, machines.StateRunning, epoch)
	h.start()
	h.machines.setState("i-a", machines.StateDeleting)
	h.machines.drop("i-b")
	h.machines.takeCalls()
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
	equal(t, "the journal", h.entries(), nil)
	if n := len(h.find("the machine is deleting or not listed")); n != 2 {
		t.Errorf("the log has %d lines for the removed entries", n)
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
	h.machines.add("i-a", dispatchA, machines.StateRunning, at.Add(-time.Second))
	h.machines.add("i-b", dispatchB, machines.StateRunning, at)
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
	h.machines.add("i-s", dispatchC, machines.StateRunning, epoch)
	h.machines.add("i-d", dispatchD, machines.StateDeleting, epoch)
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
	h.machines.add("i-early", dispatchA, machines.StateStarting, epoch)
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
	h.machines.deleteErr = []error{&adapter.Error{Command: "delete", Kind: machines.KindRetry, Message: "Try again later: the vendor throttled the call"}}
	h.start()
	h.claim(newClaim(dispatchA))
	h.machines.setState(machineID(1), machines.StateStopped)
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
// deleted and the entry removed all the same. A 410 gone to the upload is not logged
// as a failure.
func TestEachCleanupStepRunsAfterAFailure(t *testing.T) {
	for _, c := range []struct {
		name      string
		readErr   error
		uploadErr error
		uploads   int
		msg       string
	}{
		{"the read fails", &adapter.Error{Command: "log", Kind: machines.KindOther, Message: "Check the vendor"}, nil, 0, "reading a machine's log failed"},
		{"the upload fails", nil, errors.New("no answer from Qory Apiary"), 1, "uploading a machine's log failed"},
		{"the upload is gone", nil, fmt.Errorf("Qory Apiary answered: %w", loop.ErrGone), 1, ""},
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
			if c.msg != "" && len(h.find(c.msg)) != 1 {
				t.Errorf("the log has no line %q", c.msg)
			}
			if c.msg == "" && (len(h.find("uploading a machine's log failed")) != 0 || len(h.find("uploaded a machine's log")) != 0) {
				t.Error("the log has a line for the upload")
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
		{machines.MaxLog, true},
		{machines.MaxLog - 1, false},
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
	h.machines.listErr = &adapter.Error{Command: "list", Kind: machines.KindRetry, Message: "Try again later"}
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
