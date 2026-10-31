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
	"github.com/qoryai/integrations/contracts"
)

// The errors of a post of machine-end: one that a later try can change, and one that it
// cannot, as the client returns for a signed 403.
var (
	endFailed = errors.New("no answer from Qory Apiary")
	endDone   = fmt.Errorf("Qory Apiary returned 403 forbidden: %w", loop.ErrMachineEndDone)
)

// deleteFailure is a delete that the vendor throttled.
var deleteFailure = &adapter.Error{Command: "delete", Kind: contracts.FailureRetry, Message: "Try again later: the vendor throttled the call"}

// ended is the entry of the dispatch id in the phase ended, with the machine id.
func ended(id, machine string) journal.Entry {
	return journal.Entry{DispatchID: id, Phase: journal.PhaseEnded, MachineID: machine, MaxLifetimeMinutes: lifetime}
}

// stoppedMachine starts h with a dispatched entry of dispatchA whose machine i-a is
// stopped, so that the first watch cleans it up. The calls before the watch are taken.
func stoppedMachine(h *harness) {
	h.t.Helper()
	h.setEntries(dispatched(dispatchA, "i-a"))
	h.machines.add("i-a", dispatchA, contracts.MachineStopped, epoch)
	h.start()
	h.apiary.takeCalls()
	h.machines.takeCalls()
}

// TestGoneOnTheFirstDeleteRemovesTheEntry posts gone once and removes the entry in the
// same watch.
func TestGoneOnTheFirstDeleteRemovesTheEntry(t *testing.T) {
	h := newHarness(t)
	stoppedMachine(h)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-log " + dispatchA + " false", "machine-end " + dispatchA + " gone"})
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"log i-a", "delete i-a"})
	equal(t, "the journal", h.entries(), nil)
	lines := h.find("sent machine-end")
	if len(lines) != 1 || lines[0]["state"] != "gone" || lines[0]["dispatch_id"] != dispatchA {
		t.Errorf("the log lines are %v", lines)
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestDeletingThenGone posts deleting once while the vendor shuts the machine down,
// sends one delete per watch, and removes the entry at gone. The entry gets no
// dispatched, and its lifetime does not count.
func TestDeletingThenGone(t *testing.T) {
	h := newHarness(t)
	h.machines.lingering = true
	stoppedMachine(h)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-log " + dispatchA + " false", "machine-end " + dispatchA + " deleting"})
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"log i-a", "delete i-a"})
	equal(t, "the journal", h.entries(), []journal.Entry{deleting(dispatchA, "i-a")})
	h.tick()
	h.clock.Advance(lifetimeBound)
	h.clock.waitPending(t, loop.WatchInterval)
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"delete i-a", "delete i-a"})
	equal(t, "the journal", h.entries(), []journal.Entry{deleting(dispatchA, "i-a")})
	h.machines.drop("i-a")
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " gone"})
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// lifetimeBound is more than the lifetime and 15 minutes of a test's machine.
const lifetimeBound = 2 * (lifetime*time.Minute + loop.LifetimeGrace)

// TestOnePostPerChange runs delete_failed, deleting and gone, with two watches in each
// of the first two, and posts each state once.
func TestOnePostPerChange(t *testing.T) {
	h := newHarness(t)
	h.machines.lingering = true
	h.machines.deleteErr = []error{deleteFailure, deleteFailure}
	stoppedMachine(h)
	h.tick()
	h.tick()
	equal(t, "the journal", h.entries(), []journal.Entry{deleting(dispatchA, "i-a")})
	h.tick()
	h.tick()
	h.machines.drop("i-a")
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{
		"machine-log " + dispatchA + " false",
		"machine-end " + dispatchA + " delete_failed",
		"machine-end " + dispatchA + " deleting",
		"machine-end " + dispatchA + " gone",
	})
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAFailedReadRunsAgainUntilADeleteIsAccepted fails each read of the log. Each watch
// reads again while the delete fails, and no watch reads after an accepted delete.
func TestAFailedReadRunsAgainUntilADeleteIsAccepted(t *testing.T) {
	h := newHarness(t)
	h.machines.lingering = true
	h.machines.logErr = &adapter.Error{Command: "log", Kind: contracts.FailureOther, Message: "Check the vendor"}
	h.machines.deleteErr = []error{deleteFailure}
	stoppedMachine(h)
	h.tick()
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"log i-a", "delete i-a"})
	h.tick()
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"log i-a", "delete i-a"})
	h.tick()
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"delete i-a"})
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAFailedPostIsRetried keeps the entry while the post of deleting fails, and posts
// the state again at the next watch, after another delete. A 410 gone, which the
// contract does not have for machine-end, counts as a failure too. Once accepted, the
// state goes out no more.
func TestAFailedPostIsRetried(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
	}{
		{"no answer", endFailed},
		{"an unexpected 410", fmt.Errorf("Qory Apiary answered: %w", loop.ErrGone)},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.machines.lingering = true
			h.apiary.setEnd("deleting", c.err)
			stoppedMachine(h)
			h.tick()
			equal(t, "the journal", h.entries(), []journal.Entry{deleting(dispatchA, "i-a")})
			h.apiary.takeCalls()
			h.machines.takeCalls()
			if len(h.find("sending machine-end failed")) != 1 {
				t.Error("the log has no line for the failed post")
			}
			h.tick()
			equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " deleting"})
			equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"delete i-a"})
			h.apiary.setEnd("deleting", nil)
			h.tick()
			equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " deleting"})
			h.tick()
			equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
			equal(t, "the journal", h.entries(), []journal.Entry{deleting(dispatchA, "i-a")})
			if err := h.stop(); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestAnAnswerThatEndsTheTriesCountsAsDone posts deleting once when the answer is a
// signed 403, logs it once, and still removes the entry when gone gets the same answer.
func TestAnAnswerThatEndsTheTriesCountsAsDone(t *testing.T) {
	h := newHarness(t)
	h.machines.lingering = true
	h.apiary.setEnd("deleting", endDone)
	h.apiary.setEnd("gone", endDone)
	stoppedMachine(h)
	h.tick()
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-log " + dispatchA + " false", "machine-end " + dispatchA + " deleting"})
	h.machines.drop("i-a")
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " gone"})
	equal(t, "the journal", h.entries(), nil)
	if n := len(h.find("sending machine-end failed, and a later try cannot change the answer")); n != 2 {
		t.Errorf("the log has %d lines for the two refused posts", n)
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestADeletingEntryCountsTowardMaxMachines keeps the claim path waiting until gone.
func TestADeletingEntryCountsTowardMaxMachines(t *testing.T) {
	h := newHarness(t)
	h.config.MaxMachines = 1
	h.machines.lingering = true
	stoppedMachine(h)
	h.tick()
	h.tick()
	equal(t, "the journal", h.entries(), []journal.Entry{deleting(dispatchA, "i-a")})
	if n := h.apiary.claimCount(); n != 0 {
		t.Fatalf("the loop asked for %d claims with a deleting entry at max_machines", n)
	}
	h.machines.drop("i-a")
	h.tick()
	h.idle()
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestRestartKeepsADeletingEntry resumes a deleting entry with no dispatched, whatever
// the list has, and the first watch deletes the machine. A restart has no record of the
// last post, so the state goes out once more, and the log is read once more.
func TestRestartKeepsADeletingEntry(t *testing.T) {
	h := newHarness(t)
	h.machines.lingering = true
	stoppedMachine(h)
	h.tick()
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	h.apiary.takeCalls()
	h.machines.takeCalls()
	h.restart()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	equal(t, "the journal", h.entries(), []journal.Entry{deleting(dispatchA, "i-a")})
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-log " + dispatchA + " false", "machine-end " + dispatchA + " deleting"})
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"check", "log i-a", "delete i-a"})
	h.machines.drop("i-a")
	h.tick()
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestPathsWithNoDeleteToday goes through the phase deleting from each path that
// removed an entry with no delete: no read of the log, and gone is confirmed.
func TestPathsWithNoDeleteToday(t *testing.T) {
	t.Run("a dispatched entry whose machine is deleting", func(t *testing.T) {
		h := newHarness(t)
		h.machines.lingering = true
		h.setEntries(dispatched(dispatchA, "i-a"))
		h.machines.add("i-a", dispatchA, contracts.MachineRunning, epoch)
		h.start()
		h.machines.setState("i-a", contracts.MachineDeleting)
		h.apiary.takeCalls()
		h.machines.takeCalls()
		h.tick()
		equal(t, "the journal", h.entries(), []journal.Entry{deleting(dispatchA, "i-a")})
		equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " deleting"})
		equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"delete i-a"})
		h.tick()
		equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"delete i-a"})
		if err := h.stop(); err != nil {
			t.Error(err)
		}
	})
	t.Run("a claimed entry whose machine is deleting", func(t *testing.T) {
		h := newHarness(t)
		h.machines.lingering = true
		h.machines.makeErr = &adapter.Error{Command: "make", Kind: contracts.FailureRetry, Message: "Try again later"}
		h.apiary.releaseErr = machineExists
		h.start()
		h.send(claimResult{claim: newClaim(dispatchA), ok: true})
		h.clock.waitPending(t, loop.ShortWait, loop.WatchInterval)
		h.clock.Advance(loop.ShortWait)
		h.idle()
		h.machines.add("i-late", dispatchA, contracts.MachineDeleting, epoch)
		h.apiary.takeCalls()
		h.machines.takeCalls()
		h.tick()
		equal(t, "the journal", h.entries(), []journal.Entry{deleting(dispatchA, "i-late")})
		equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " deleting"})
		equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"delete i-late"})
		if err := h.stop(); err != nil {
			t.Error(err)
		}
	})
	t.Run("a machine not listed after the grace", func(t *testing.T) {
		h := newHarness(t)
		h.setEntries(dispatched(dispatchA, "i-a"))
		h.start()
		h.clock.Advance(loop.ListGrace)
		h.clock.waitPending(t, loop.WatchInterval)
		h.apiary.takeCalls()
		h.machines.takeCalls()
		h.tick()
		equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " gone"})
		equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"delete i-a"})
		equal(t, "the journal", h.entries(), nil)
		if err := h.stop(); err != nil {
			t.Error(err)
		}
	})
	t.Run("a restart with a machine that is deleting", func(t *testing.T) {
		h := newHarness(t)
		h.setEntries(journal.Entry{DispatchID: dispatchA, Phase: journal.PhaseClaimed, MaxLifetimeMinutes: lifetime})
		h.machines.add("i-a", dispatchA, contracts.MachineDeleting, epoch)
		h.start()
		equal(t, "the journal", h.entries(), []journal.Entry{deleting(dispatchA, "i-a")})
		equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
		h.machines.takeCalls()
		h.tick()
		equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " gone"})
		equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"delete i-a"})
		equal(t, "the journal", h.entries(), nil)
		if err := h.stop(); err != nil {
			t.Error(err)
		}
	})
}

// TestAStrayWithADispatchKeyGetsAnEntry writes a deleting entry with the shortest
// lifetime for a machine with no entry, and follows the path of any other entry.
func TestAStrayWithADispatchKeyGetsAnEntry(t *testing.T) {
	h := newHarness(t)
	h.machines.lingering = true
	h.start()
	h.machines.add("i-s", dispatchC, contracts.MachineRunning, epoch)
	h.machines.takeCalls()
	h.tick()
	want := journal.Entry{DispatchID: dispatchC, Phase: journal.PhaseDeleting, MachineID: "i-s", MaxLifetimeMinutes: journal.MinLifetime}
	equal(t, "the journal", h.entries(), []journal.Entry{want})
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-log " + dispatchC + " false", "machine-end " + dispatchC + " deleting"})
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"log i-s", "delete i-s"})
	h.machines.drop("i-s")
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchC + " gone"})
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestTwoStraysWithOneKeyKeepTheFirstEntry leaves the second machine of a key for a
// later watch, so that the second does not replace the entry of the first.
func TestTwoStraysWithOneKeyKeepTheFirstEntry(t *testing.T) {
	h := newHarness(t)
	h.machines.lingering = true
	h.start()
	h.machines.add("i-1", dispatchC, contracts.MachineRunning, epoch)
	h.machines.add("i-2", dispatchC, contracts.MachineRunning, epoch)
	h.machines.takeCalls()
	h.tick()
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"log i-1", "delete i-1"})
	if got := h.entries(); len(got) != 1 || got[0].MachineID != "i-1" {
		t.Errorf("the journal is %v", got)
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAStrayWithNoEntryGetsOneCleanupAndNoPost keeps today's one delete, with no post,
// for a key of another shape and after a failed write of the journal.
func TestAStrayWithNoEntryGetsOneCleanupAndNoPost(t *testing.T) {
	for _, c := range []struct {
		name, key string
		failSet   bool
	}{
		{"a key of another shape", "not-a-dispatch", false},
		{"a failed write", dispatchC, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			failing := &setFailure{}
			failing.setFail(c.failSet)
			h.wrap = func(j loop.Journal) loop.Journal { failing.Journal = j; return failing }
			h.start()
			h.machines.add("i-s", c.key, contracts.MachineRunning, epoch)
			h.machines.takeCalls()
			h.tick()
			equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-log " + c.key + " false"})
			equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"log i-s", "delete i-s"})
			equal(t, "the journal", h.entries(), nil)
			if n := len(h.find("writing the journal failed")); (n == 1) != c.failSet {
				t.Errorf("the log has %d lines for a failed write", n)
			}
			if err := h.stop(); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestAFailedWriteLeavesTheEntry keeps the entry as it is and posts nothing when the
// write of the phase deleting fails. The next watch acts on the entry again.
func TestAFailedWriteLeavesTheEntry(t *testing.T) {
	h := newHarness(t)
	h.machines.lingering = true
	failing := &setFailure{}
	h.wrap = func(j loop.Journal) loop.Journal { failing.Journal = j; return failing }
	stoppedMachine(h)
	failing.setFail(true)
	h.tick()
	equal(t, "the journal", h.entries(), []journal.Entry{dispatched(dispatchA, "i-a")})
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-log " + dispatchA + " false"})
	if len(h.find("writing the journal failed")) != 1 {
		t.Error("the log has no line for the failed write")
	}
	failing.setFail(false)
	h.machines.takeCalls()
	h.tick()
	equal(t, "the journal", h.entries(), []journal.Entry{deleting(dispatchA, "i-a")})
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"delete i-a"})
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " deleting"})
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAFailedGoneFreesTheSlot moves the entry to the phase ended when the first post of
// gone fails, which wakes a claim path that waits at max_machines. The next machine
// starts while the entry is in the journal.
func TestAFailedGoneFreesTheSlot(t *testing.T) {
	h := newHarness(t)
	h.config.MaxMachines = 1
	h.apiary.setEnd("gone", endFailed)
	stoppedMachine(h)
	if n := h.apiary.claimCount(); n != 0 {
		t.Fatalf("the loop asked for %d claims with a full journal", n)
	}
	h.tick()
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a")})
	// The ended entry counts for nothing, so the new machine fills max_machines and no
	// second claim follows. The claim is handled once "sent dispatched" is in the log.
	h.send(claimResult{claim: newClaim(dispatchB), ok: true})
	for deadline := time.Now().Add(patience); len(h.find("sent dispatched")) < 1; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the claim path did not log sent dispatched")
		}
	}
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a"), dispatched(dispatchB, machineID(1))})
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAnEndedEntryGetsOneTryAndNoDelete sends gone with the client's tries right after
// the delete, then with one try at each watch, with no delete, until the post is
// accepted.
func TestAnEndedEntryGetsOneTryAndNoDelete(t *testing.T) {
	h := newHarness(t)
	h.apiary.setEnd("gone", endFailed)
	stoppedMachine(h)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-log " + dispatchA + " false", "machine-end " + dispatchA + " gone"})
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"log i-a", "delete i-a"})
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end-once " + dispatchA + " gone"})
	equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a")})
	h.apiary.setEnd("gone", nil)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end-once " + dispatchA + " gone"})
	equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAnEndedEntryLeavesOnAnAnswerThatEndsTheTries removes the entry on a signed 403.
func TestAnEndedEntryLeavesOnAnAnswerThatEndsTheTries(t *testing.T) {
	h := newHarness(t)
	h.setEntries(ended(dispatchA, "i-a"))
	h.apiary.setEnd("gone", endDone)
	h.start()
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end-once " + dispatchA + " gone"})
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAFailedRemovalAfterGoneMovesTheEntryToEnded keeps no entry able to reach a
// delete or a dispatched when the journal cannot drop it, and the next watch posts
// gone and removes it.
func TestAFailedRemovalAfterGoneMovesTheEntryToEnded(t *testing.T) {
	h := newHarness(t)
	failing := &setFailure{}
	failing.setFailRemove(true)
	h.wrap = func(j loop.Journal) loop.Journal { failing.Journal = j; return failing }
	stoppedMachine(h)
	h.tick()
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a")})
	failing.setFailRemove(false)
	h.apiary.takeCalls()
	h.machines.takeCalls()
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end-once " + dispatchA + " gone"})
	equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAFailedWriteOfEndedKeepsTheEntry keeps the entry as it is when the write of the
// phase ended fails, and the next watch acts on it again. The slot stays held.
func TestAFailedWriteOfEndedKeepsTheEntry(t *testing.T) {
	h := newHarness(t)
	h.machines.lingering = true
	h.apiary.setEnd("gone", endFailed)
	failing := &setFailure{}
	h.wrap = func(j loop.Journal) loop.Journal { failing.Journal = j; return failing }
	stoppedMachine(h)
	h.tick()
	equal(t, "the journal", h.entries(), []journal.Entry{deleting(dispatchA, "i-a")})
	h.machines.drop("i-a")
	failing.setFail(true)
	h.tick()
	equal(t, "the journal", h.entries(), []journal.Entry{deleting(dispatchA, "i-a")})
	if len(h.find("writing the journal failed")) != 1 {
		t.Error("the log has no line for the failed write")
	}
	failing.setFail(false)
	h.machines.takeCalls()
	h.tick()
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"delete i-a"})
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a")})
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAWatchStopsPostingAtTheFirstFailure sends gone for the first of two ended entries
// only, and live machines get dispatched in that watch. Once posts work, both leave.
func TestAWatchStopsPostingAtTheFirstFailure(t *testing.T) {
	h := newHarness(t)
	h.setEntries(ended(dispatchA, "i-a"), ended(dispatchB, "i-b"), dispatched(dispatchC, "i-c"))
	h.machines.add("i-c", dispatchC, contracts.MachineRunning, epoch)
	h.apiary.setEnd("gone", endFailed)
	h.start()
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{
		"machine-end-once " + dispatchA + " gone",
		"dispatched " + dispatchC + " i-c",
	})
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a"), ended(dispatchB, "i-b"), dispatched(dispatchC, "i-c")})
	h.apiary.setEnd("gone", nil)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{
		"machine-end-once " + dispatchA + " gone",
		"machine-end-once " + dispatchB + " gone",
		"dispatched " + dispatchC + " i-c",
	})
	equal(t, "the journal", h.entries(), []journal.Entry{dispatched(dispatchC, "i-c")})
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAnEndedEntryLeavesAtTheLimit retries a failing post of gone, keeps the entry until
// 24 hours after the first gone, and then logs an error and removes the entry.
func TestAnEndedEntryLeavesAtTheLimit(t *testing.T) {
	h := newHarness(t)
	h.apiary.setEnd("gone", endFailed)
	stoppedMachine(h)
	h.tick()
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a")})
	h.clock.Advance(loop.GoneLimit - loop.WatchInterval)
	h.clock.waitPending(t, loop.WatchInterval)
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a")})
	h.tick()
	equal(t, "the journal", h.entries(), nil)
	lines := h.find("stopped sending machine-end gone after 24 hours, so Qory Apiary shows no removal of this machine")
	if len(lines) != 1 || lines[0]["level"] != "ERROR" || lines[0]["dispatch_id"] != dispatchA {
		t.Errorf("the log lines are %v", lines)
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestASkippedEndedEntryLeavesAtTheLimit removes the second of two entries at the limit
// although the watch sent it no post.
func TestASkippedEndedEntryLeavesAtTheLimit(t *testing.T) {
	h := newHarness(t)
	h.setEntries(ended(dispatchA, "i-a"), ended(dispatchB, "i-b"))
	h.apiary.setEnd("gone", endFailed)
	h.start()
	h.clock.Advance(loop.GoneLimit)
	h.clock.waitPending(t, loop.WatchInterval)
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end-once " + dispatchA + " gone"})
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestRestartKeepsAnEndedEntry resumes an ended entry with no dispatched and no delete,
// also when the list has a machine with its key. The limit starts again at the restart.
func TestRestartKeepsAnEndedEntry(t *testing.T) {
	h := newHarness(t)
	h.apiary.setEnd("gone", endFailed)
	stoppedMachine(h)
	h.tick()
	h.clock.Advance(loop.GoneLimit - 2*loop.WatchInterval)
	h.clock.waitPending(t, loop.WatchInterval)
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	h.machines.add("i-other", dispatchA, contracts.MachineRunning, epoch)
	h.apiary.takeCalls()
	h.machines.takeCalls()
	h.restart()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"check"})
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a")})
	// One line is the dispatched entry of the first start.
	if n := len(h.find("resumed a dispatch")); n != 2 {
		t.Errorf("the log has %d lines for the resumed entries", n)
	}
	h.clock.Advance(loop.GoneLimit - loop.WatchInterval)
	h.clock.waitPending(t, loop.WatchInterval)
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a")})
	h.tick()
	equal(t, "the journal", h.entries(), nil)
	calls := h.apiary.takeCalls()
	for _, c := range calls {
		if c != "machine-end-once "+dispatchA+" gone" {
			t.Errorf("Qory Apiary's calls are %q", strings.Join(calls, ", "))
		}
	}
	equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}
