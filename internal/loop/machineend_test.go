package loop_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qoryai/hive/internal/adapter"
	"github.com/qoryai/hive/internal/journal"
	"github.com/qoryai/hive/internal/loop"
	"github.com/qoryai/integrations/contracts"
)

// The errors of a post of machine-end: one that a later try can change, one that the
// client returns for a signed answer that breaks the contract, and one for a signed 403.
var (
	endFailed  = errors.New("no answer from Qory Apiary")
	endDone    = fmt.Errorf("Qory Apiary's answer breaks the contract: %w", loop.ErrMachineEndDone)
	endStopped = fmt.Errorf("Qory Apiary returned 403 forbidden: %w", loop.ErrMachineEndStopped)
)

// stateLimitLine is the log line of a deleting or delete_failed dropped at
// [loop.MachineEndLimit].
const stateLimitLine = "stopped sending this machine-end state after 24 hours"

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

// TestAnUnusableAnswerCountsAsDone posts deleting once when the answer breaks the
// contract, logs it once, and still removes the entry when gone gets the same answer.
func TestAnUnusableAnswerCountsAsDone(t *testing.T) {
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
		t.Errorf("the log has %d lines for the two unusable answers", n)
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestASigned403StopsThePosts sends no more machine-end after a signed 403, while the
// deletes go on. Gone from the adapter then removes the entry with no post.
func TestASigned403StopsThePosts(t *testing.T) {
	h := newHarness(t)
	h.machines.lingering = true
	h.machines.deleteErr = []error{nil, deleteFailure}
	h.apiary.setEnd("deleting", endStopped)
	stoppedMachine(h)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-log " + dispatchA + " false", "machine-end " + dispatchA + " deleting"})
	h.machines.takeCalls()
	// A failed delete changes the state, and the dispatch gets no post for it.
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"delete i-a"})
	equal(t, "the journal", h.entries(), []journal.Entry{deleting(dispatchA, "i-a")})
	h.machines.drop("i-a")
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"delete i-a"})
	equal(t, "the journal", h.entries(), nil)
	if n := len(h.find("sending machine-end failed, and the dispatch gets no more machine-end")); n != 1 {
		t.Errorf("the log has %d lines for the stop", n)
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAStopOnGoneRemovesTheEntry removes the entry when the post of gone gets a signed
// 403, right after the delete.
func TestAStopOnGoneRemovesTheEntry(t *testing.T) {
	h := newHarness(t)
	h.apiary.setEnd("gone", endStopped)
	stoppedMachine(h)
	h.tick()
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

// TestAStrayWithAKeyOfAnotherShapeGetsOneCleanupAndNoPost keeps today's one delete,
// with no post, for a key that is not a dispatch id.
func TestAStrayWithAKeyOfAnotherShapeGetsOneCleanupAndNoPost(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.machines.add("i-s", "not-a-dispatch", contracts.MachineRunning, epoch)
	h.machines.takeCalls()
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-log not-a-dispatch false"})
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"log i-s", "delete i-s"})
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAStrayWhoseDeleteFailsGetsOneLogRead reads the log of a stray whose key is not a
// dispatch id once, while each watch sends its delete again and the delete fails.
func TestAStrayWhoseDeleteFailsGetsOneLogRead(t *testing.T) {
	h := newHarness(t)
	h.machines.setDeleteErr(deleteFailure, deleteFailure, deleteFailure)
	h.start()
	h.machines.add("i-s", "not-a-dispatch", contracts.MachineRunning, epoch)
	h.machines.takeCalls()
	h.tick()
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"log i-s", "delete i-s"})
	for range 2 {
		h.tick()
		equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"delete i-s"})
	}
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-log not-a-dispatch false"})
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAStrayWhoseWriteFailsGetsOnePostAfterTheDelete deletes a stray with a dispatch
// key once, with no entry, when the write of the journal fails, and then posts the
// state that the delete returned, once.
func TestAStrayWhoseWriteFailsGetsOnePostAfterTheDelete(t *testing.T) {
	for _, c := range []struct {
		name      string
		lingering bool
		deleteErr error
		state     string
	}{
		{"gone", false, nil, "gone"},
		{"deleting", true, nil, "deleting"},
		{"a failed delete", false, deleteFailure, "delete_failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.machines.lingering = c.lingering
			h.machines.setDeleteErr(c.deleteErr)
			failing := &setFailure{}
			failing.setFail(true)
			h.wrap = func(j loop.Journal) loop.Journal { failing.Journal = j; return failing }
			h.start()
			h.machines.add("i-s", dispatchC, contracts.MachineRunning, epoch)
			h.machines.takeCalls()
			h.tick()
			equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-log " + dispatchC + " false", "machine-end " + dispatchC + " " + c.state})
			equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"log i-s", "delete i-s"})
			equal(t, "the journal", h.entries(), nil)
			if n := len(h.find("writing the journal failed")); n != 1 {
				t.Errorf("the log has %d lines for a failed write", n)
			}
			if err := h.stop(); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestAPendingStrayIsDeletedUntilGone keeps the machine of a stray whose journal write
// failed and whose delete did not return gone. Each watch deletes it again, with no read
// of its log, also after the list leaves it out, until the delete returns gone. Then
// gone is posted.
func TestAPendingStrayIsDeletedUntilGone(t *testing.T) {
	t.Run("deleting, then out of the list", func(t *testing.T) {
		h := newHarness(t)
		h.machines.lingering = true
		failing := &setFailure{}
		failing.setFail(true)
		h.wrap = func(j loop.Journal) loop.Journal { failing.Journal = j; return failing }
		h.start()
		h.machines.add("i-s", dispatchC, contracts.MachineRunning, epoch)
		h.machines.takeCalls()
		h.tick()
		equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-log " + dispatchC + " false", "machine-end " + dispatchC + " deleting"})
		equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"log i-s", "delete i-s"})
		// The vendor's list leaves out a machine once it is terminated.
		h.machines.drop("i-s")
		h.tick()
		equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchC + " gone"})
		equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"delete i-s"})
		h.tick()
		equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
		equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
		equal(t, "the journal", h.entries(), nil)
		if err := h.stop(); err != nil {
			t.Error(err)
		}
	})
	t.Run("a failed delete", func(t *testing.T) {
		h := newHarness(t)
		h.machines.setDeleteErr(deleteFailure, deleteFailure)
		failing := &setFailure{}
		failing.setFail(true)
		h.wrap = func(j loop.Journal) loop.Journal { failing.Journal = j; return failing }
		h.start()
		h.machines.add("i-s", dispatchC, contracts.MachineRunning, epoch)
		h.machines.takeCalls()
		h.tick()
		equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-log " + dispatchC + " false", "machine-end " + dispatchC + " delete_failed"})
		equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"log i-s", "delete i-s"})
		h.tick()
		equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
		equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"delete i-s"})
		h.tick()
		equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchC + " gone"})
		equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"delete i-s"})
		if err := h.stop(); err != nil {
			t.Error(err)
		}
	})
}

// TestADeletingStrayWithADispatchKeyGetsAnEntry writes a deleting entry for a listed
// machine that is deleting and whose dispatch key has no entry, with no read of its
// log. Each watch deletes it again until the delete returns gone, and then gone is
// posted.
func TestADeletingStrayWithADispatchKeyGetsAnEntry(t *testing.T) {
	h := newHarness(t)
	h.machines.lingering = true
	h.start()
	h.machines.add("i-s", dispatchC, contracts.MachineDeleting, epoch)
	h.machines.takeCalls()
	h.tick()
	equal(t, "the journal", h.entries(), []journal.Entry{{DispatchID: dispatchC, Phase: journal.PhaseDeleting, MachineID: "i-s", MaxLifetimeMinutes: journal.MinLifetime}})
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchC + " deleting"})
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"delete i-s"})
	h.tick()
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"delete i-s"})
	h.machines.setLingering(false)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchC + " gone"})
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// failingStrayGone starts h with a journal whose writes fail and a stray of dispatchC
// whose delete returns gone, while the post of gone returns err. The first watch
// deletes the stray and posts gone once. The calls of that watch are taken.
func failingStrayGone(h *harness, err error) {
	h.t.Helper()
	failing := &setFailure{}
	failing.setFail(true)
	h.wrap = func(j loop.Journal) loop.Journal { failing.Journal = j; return failing }
	h.apiary.setEnd("gone", err)
	h.start()
	h.machines.add("i-s", dispatchC, contracts.MachineRunning, epoch)
	h.machines.takeCalls()
	h.tick()
	equal(h.t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-log " + dispatchC + " false", "machine-end " + dispatchC + " gone"})
	equal(h.t, "the adapter's calls", h.machines.takeCalls(), []string{"log i-s", "delete i-s"})
	equal(h.t, "the journal", h.entries(), nil)
}

// TestAStrayGoneIsSentAgainUntilAccepted keeps the gone of a stray with no entry after
// a failed post, and sends it again with the waits of an ended entry until Qory Apiary
// accepts it. The stray is no longer listed, so it gets no more deletes.
func TestAStrayGoneIsSentAgainUntilAccepted(t *testing.T) {
	h := newHarness(t)
	failingStrayGone(h, endFailed)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchC + " gone"})
	h.apiary.setEnd("gone", nil)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchC + " gone"})
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAStrayGoneEndsOnASigned403 sends the pending gone of a stray no more after a
// signed 403 to it.
func TestAStrayGoneEndsOnASigned403(t *testing.T) {
	h := newHarness(t)
	failingStrayGone(h, endFailed)
	h.apiary.setEnd("gone", endStopped)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchC + " gone"})
	for range 3 {
		h.tick()
	}
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
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

// TestAFailedGoneKeepsAnEndedEntry moves the entry to the phase ended when the first
// post of gone fails. The next machine starts while the ended entry is in the journal.
func TestAFailedGoneKeepsAnEndedEntry(t *testing.T) {
	h := newHarness(t)
	h.apiary.setEnd("gone", endFailed)
	stoppedMachine(h)
	h.tick()
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a")})
	h.claim(newClaim(dispatchB))
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a"), typed(dispatched(dispatchB, machineID(1)))})
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAnEndedEntryGetsOneTryAndNoDelete sends gone right after the delete, then once
// after each wait, with no delete, until the post is accepted. The first wait is one
// watch, and the second two.
func TestAnEndedEntryGetsOneTryAndNoDelete(t *testing.T) {
	h := newHarness(t)
	h.apiary.setEnd("gone", endFailed)
	stoppedMachine(h)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-log " + dispatchA + " false", "machine-end " + dispatchA + " gone"})
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"log i-a", "delete i-a"})
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " gone"})
	equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a")})
	h.apiary.setEnd("gone", nil)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a")})
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " gone"})
	equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAnEndedEntryLeavesOnASigned403 removes the entry.
func TestAnEndedEntryLeavesOnASigned403(t *testing.T) {
	h := newHarness(t)
	h.setEntries(ended(dispatchA, "i-a"))
	h.apiary.setEnd("gone", endStopped)
	h.start()
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " gone"})
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAFailedRemovalAfterGoneMovesTheEntryToEnded keeps an accepted gone as done when
// the journal cannot drop the entry. The entry gets no delete, no dispatched and no
// second post. Each watch tries the removal again, also after 24 hours.
func TestAFailedRemovalAfterGoneMovesTheEntryToEnded(t *testing.T) {
	h := newHarness(t)
	failing := &setFailure{}
	failing.setFailRemove(true)
	h.wrap = func(j loop.Journal) loop.Journal { failing.Journal = j; return failing }
	stoppedMachine(h)
	h.tick()
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a")})
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-log " + dispatchA + " false", "machine-end " + dispatchA + " gone"})
	h.machines.takeCalls()
	h.apiary.setEnd("gone", endFailed)
	h.tick()
	h.clock.Advance(loop.MachineEndLimit)
	h.clock.waitPending(t, loop.WatchInterval)
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a")})
	failing.setFailRemove(false)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAStoppedDispatchGetsNoPostInThePhaseEnded posts nothing for an entry that stays in
// the journal after a stop, when its removal fails, and removes it at the next watch.
func TestAStoppedDispatchGetsNoPostInThePhaseEnded(t *testing.T) {
	h := newHarness(t)
	failing := &setFailure{}
	failing.setFailRemove(true)
	h.wrap = func(j loop.Journal) loop.Journal { failing.Journal = j; return failing }
	h.apiary.setEnd("gone", endStopped)
	stoppedMachine(h)
	h.tick()
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a")})
	failing.setFailRemove(false)
	h.apiary.takeCalls()
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
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

// deletingMachines adds a listed machine for each deleting entry of dispatchA to
// dispatchC, with a live machine for dispatchD, starts h, and takes the calls.
func deletingMachines(h *harness) {
	h.t.Helper()
	h.machines.lingering = true
	h.setEntries(deleting(dispatchA, "i-a"), deleting(dispatchB, "i-b"), deleting(dispatchC, "i-c"), dispatched(dispatchD, "i-d"))
	h.machines.add("i-a", dispatchA, contracts.MachineDeleting, epoch)
	h.machines.add("i-b", dispatchB, contracts.MachineDeleting, epoch)
	h.machines.add("i-c", dispatchC, contracts.MachineDeleting, epoch)
	h.machines.add("i-d", dispatchD, contracts.MachineRunning, epoch)
	h.start()
	h.apiary.takeCalls()
	h.machines.takeCalls()
}

// withoutLogs is calls with no machine-log.
func withoutLogs(calls []string) []string {
	return slices.DeleteFunc(calls, func(c string) bool { return strings.HasPrefix(c, "machine-log ") })
}

// TestOneFailedPostPerWatch fails the post of deleting. Each watch tries one post, sends
// each delete, and sends dispatched for the live machine. The pause ends with the watch.
func TestOneFailedPostPerWatch(t *testing.T) {
	h := newHarness(t)
	h.apiary.setEnd("deleting", endFailed)
	deletingMachines(h)
	for range 2 {
		h.tick()
		equal(t, "Qory Apiary's calls", withoutLogs(h.apiary.takeCalls()), []string{
			"machine-end " + dispatchA + " deleting",
			"dispatched " + dispatchD + " i-d -",
		})
		if n := h.apiary.takeEndTries(); n != 1 {
			t.Errorf("%d tries in one watch", n)
		}
		calls := h.machines.takeCalls()
		for _, want := range []string{"delete i-a", "delete i-b", "delete i-c"} {
			if !slices.Contains(calls, want) {
				t.Errorf("the adapter's calls are %q, with no %q", calls, want)
			}
		}
	}
	h.apiary.setEnd("deleting", nil)
	h.tick()
	equal(t, "Qory Apiary's calls", withoutLogs(h.apiary.takeCalls()), []string{
		"machine-end " + dispatchA + " deleting",
		"machine-end " + dispatchB + " deleting",
		"machine-end " + dispatchC + " deleting",
		"dispatched " + dispatchD + " i-d -",
	})
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestGoneWhilePausedEntersEnded sends no post for gone after a failed post in the same
// watch, and the entry moves to the phase ended, which frees its slot.
func TestGoneWhilePausedEntersEnded(t *testing.T) {
	h := newHarness(t)
	h.apiary.setEnd("deleting", endFailed)
	h.setEntries(deleting(dispatchA, "i-a"), deleting(dispatchB, "i-b"))
	h.machines.add("i-a", dispatchA, contracts.MachineDeleting, epoch)
	h.machines.lingering = true
	h.start()
	h.apiary.takeCalls()
	h.tick()
	equal(t, "Qory Apiary's calls", withoutLogs(h.apiary.takeCalls()), []string{"machine-end " + dispatchA + " deleting"})
	equal(t, "the journal", h.entries(), []journal.Entry{deleting(dispatchA, "i-a"), ended(dispatchB, "i-b")})
	h.apiary.setEnd("deleting", nil)
	h.tick()
	equal(t, "Qory Apiary's calls", withoutLogs(h.apiary.takeCalls()), []string{"machine-end " + dispatchA + " deleting", "machine-end " + dispatchB + " gone"})
	equal(t, "the journal", h.entries(), []journal.Entry{deleting(dispatchA, "i-a")})
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestThePauseCoversTheClaimPath cleans up a machine in the claim path while posts are
// paused: no post, and the entry enters the phase ended for the next watch.
func TestThePauseCoversTheClaimPath(t *testing.T) {
	h := newHarness(t)
	h.apiary.setEnd("deleting", endFailed)
	h.setEntries(deleting(dispatchA, "i-a"))
	h.machines.add("i-a", dispatchA, contracts.MachineDeleting, epoch)
	h.machines.lingering = true
	h.start()
	h.tick()
	h.apiary.takeCalls()
	h.machines.setLingering(false)
	h.apiary.setDispatched(dispatchB, loop.ErrGone)
	h.claim(newClaim(dispatchB))
	equal(t, "the journal", h.entries(), []journal.Entry{deleting(dispatchA, "i-a"), typed(ended(dispatchB, machineID(1)))})
	if calls := h.apiary.takeCalls(); slices.Contains(calls, "machine-end "+dispatchB+" gone") {
		t.Errorf("Qory Apiary's calls are %q", calls)
	}
	h.apiary.setEnd("deleting", nil)
	h.tick()
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// expectFailingDeleteFailed runs two watches with failing deletes and a failing post of
// delete_failed, and checks that no limit applies to it: each watch tries the post.
func expectFailingDeleteFailed(h *harness) {
	h.t.Helper()
	h.machines.setDeleteErr(deleteFailure, deleteFailure)
	h.apiary.setEnd("delete_failed", endFailed)
	h.apiary.takeCalls()
	for range 2 {
		h.tick()
		equal(h.t, "Qory Apiary's calls", withoutLogs(h.apiary.takeCalls()), []string{"machine-end " + dispatchA + " delete_failed"})
	}
	if n := len(h.find(stateLimitLine)); n != 0 {
		h.t.Errorf("the log has %d lines for a state that failed just now", n)
	}
}

// TestAStateThatAgainMatchesTheLastEndsItsCount fails delete_failed once after an
// accepted deleting. The next accepted delete leaves deleting as the last state, so no
// post goes out, and the failure's count ends. A failure 24 hours later counts anew.
func TestAStateThatAgainMatchesTheLastEndsItsCount(t *testing.T) {
	h := newHarness(t)
	h.machines.lingering = true
	h.machines.deleteErr = []error{nil, deleteFailure}
	h.apiary.setEnd("delete_failed", endFailed)
	stoppedMachine(h)
	h.tick()
	h.tick()
	h.tick()
	h.clock.Advance(loop.MachineEndLimit)
	h.clock.waitPending(t, loop.WatchInterval)
	equal(t, "Qory Apiary's calls", withoutLogs(h.apiary.takeCalls()), []string{
		"machine-end " + dispatchA + " deleting",
		"machine-end " + dispatchA + " delete_failed",
	})
	expectFailingDeleteFailed(h)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAnAcceptedPostEndsTheCount fails delete_failed once and then gets it accepted.
// The next post, 24 hours later, is an accepted deleting, so no matching state ends the
// count in between. A failure of delete_failed after it counts anew.
func TestAnAcceptedPostEndsTheCount(t *testing.T) {
	h := newHarness(t)
	h.machines.lingering = true
	h.machines.deleteErr = []error{deleteFailure, deleteFailure}
	h.apiary.setEnd("delete_failed", endFailed)
	stoppedMachine(h)
	h.tick()
	h.apiary.setEnd("delete_failed", nil)
	h.tick()
	h.clock.Advance(loop.MachineEndLimit)
	h.clock.waitPending(t, loop.WatchInterval)
	equal(t, "Qory Apiary's calls", withoutLogs(h.apiary.takeCalls()), []string{
		"machine-end " + dispatchA + " delete_failed",
		"machine-end " + dispatchA + " delete_failed",
		"machine-end " + dispatchA + " deleting",
	})
	expectFailingDeleteFailed(h)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestARemovalEndsTheCount fails delete_failed once, then gone for 24 hours, and the
// entry leaves once gone is accepted. A machine with the same key after that gets a new
// entry, whose failure counts anew.
func TestARemovalEndsTheCount(t *testing.T) {
	h := newHarness(t)
	h.machines.deleteErr = []error{deleteFailure}
	h.apiary.setEnd("delete_failed", endFailed)
	h.apiary.setEnd("gone", endFailed)
	stoppedMachine(h)
	h.tick()
	h.tick()
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a")})
	h.clock.Advance(loop.MachineEndLimit)
	h.clock.waitPending(t, loop.WatchInterval)
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a")})
	h.apiary.setEnd("gone", nil)
	// The second failure of gone set a wait of two watches.
	h.tick()
	h.tick()
	equal(t, "the journal", h.entries(), nil)
	// A machine with the key of an accepted gone counts as a stray only after the
	// list grace.
	h.clock.Advance(loop.ListGrace)
	h.clock.waitPending(t, loop.WatchInterval)
	h.machines.add("i-again", dispatchA, contracts.MachineRunning, epoch)
	expectFailingDeleteFailed(h)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAChangeOfStateStartsTheCount fails delete_failed for 24 hours less one watch, then
// deleting. The count of deleting starts at its first failure, so the state is not
// dropped, and each watch keeps trying it.
func TestAChangeOfStateStartsTheCount(t *testing.T) {
	h := newHarness(t)
	h.machines.lingering = true
	h.machines.deleteErr = []error{deleteFailure, deleteFailure}
	h.apiary.setEnd("delete_failed", endFailed)
	h.apiary.setEnd("deleting", endFailed)
	stoppedMachine(h)
	h.tick()
	h.clock.Advance(loop.MachineEndLimit - loop.WatchInterval)
	h.clock.waitPending(t, loop.WatchInterval)
	h.apiary.takeCalls()
	for range 2 {
		h.tick()
		equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " deleting"})
	}
	if n := len(h.find(stateLimitLine)); n != 0 {
		t.Errorf("the log has %d lines for a state that failed just now", n)
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestADroppedStateIsFollowedByTheNextOnes keeps a failing delete_failed for 24 hours
// from its first failure, logs one error, and sends it no more. The later deleting and
// gone go out as usual.
func TestADroppedStateIsFollowedByTheNextOnes(t *testing.T) {
	const msg = stateLimitLine
	h := newHarness(t)
	h.machines.lingering = true
	h.machines.deleteErr = []error{deleteFailure, deleteFailure, deleteFailure, deleteFailure}
	h.apiary.setEnd("delete_failed", endFailed)
	stoppedMachine(h)
	h.tick()
	h.clock.Advance(loop.MachineEndLimit - loop.WatchInterval)
	h.clock.waitPending(t, loop.WatchInterval)
	equal(t, "Qory Apiary's calls", withoutLogs(h.apiary.takeCalls()), []string{"machine-end " + dispatchA + " delete_failed", "machine-end " + dispatchA + " delete_failed"})
	if len(h.find(msg)) != 0 {
		t.Error("the state was dropped before the limit")
	}
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " delete_failed"})
	lines := h.find(msg)
	if len(lines) != 1 || lines[0]["level"] != "ERROR" || lines[0]["state"] != "delete_failed" || lines[0]["dispatch_id"] != dispatchA {
		t.Errorf("the log lines are %v", lines)
	}
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	if len(h.find(msg)) != 1 {
		t.Error("the error is logged twice")
	}
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " deleting"})
	h.machines.drop("i-a")
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " gone"})
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestASkippedStateIsDroppedAtTheLimit drops the state of each entry at the limit, also
// of the entries that the pause skipped, so that no post repeats after it.
func TestASkippedStateIsDroppedAtTheLimit(t *testing.T) {
	h := newHarness(t)
	h.apiary.setEnd("deleting", endFailed)
	deletingMachines(h)
	h.tick()
	h.clock.Advance(loop.MachineEndLimit)
	h.clock.waitPending(t, loop.WatchInterval)
	if n := len(h.find(stateLimitLine)); n != 3 {
		t.Errorf("the log has %d lines for 3 dropped states", n)
	}
	h.apiary.takeCalls()
	h.tick()
	// The live machine ran past its lifetime in those 24 hours, so only its state is new.
	equal(t, "Qory Apiary's calls", withoutLogs(h.apiary.takeCalls()), []string{"machine-end " + dispatchD + " deleting"})
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAFailedSyncStillEndsTheEntry fails the sync of the folder after the write of the
// phase ended. The entry counts as ended, so the failed post counts: the wait after the
// second failure is two watches, not one.
func TestAFailedSyncStillEndsTheEntry(t *testing.T) {
	h := newHarness(t)
	h.apiary.setEnd("gone", endFailed)
	failing := &setFailure{}
	h.wrap = func(j loop.Journal) loop.Journal { failing.Journal = j; return failing }
	stoppedMachine(h)
	failing.setFailSync(true)
	h.tick()
	failing.setFailSync(false)
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a")})
	h.apiary.takeCalls()
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " gone"})
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	h.apiary.setEnd("gone", nil)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " gone"})
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAFailedRemovalOfAnEndedEntryIsTriedAgain accepts gone for an ended entry while
// the removal fails. The log has the failed removal, and the next watch removes the
// entry with no second post.
func TestAFailedRemovalOfAnEndedEntryIsTriedAgain(t *testing.T) {
	h := newHarness(t)
	failing := &setFailure{}
	failing.setFailRemove(true)
	h.wrap = func(j loop.Journal) loop.Journal { failing.Journal = j; return failing }
	h.setEntries(ended(dispatchA, "i-a"))
	h.start()
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " gone"})
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a")})
	if len(h.find("removing a dispatch from the journal failed")) != 1 {
		t.Error("the log has no line for the failed removal")
	}
	failing.setFailRemove(false)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	equal(t, "the journal", h.entries(), nil)
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
		"machine-end " + dispatchA + " gone",
		"dispatched " + dispatchC + " i-c -",
	})
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a"), ended(dispatchB, "i-b"), dispatched(dispatchC, "i-c")})
	h.apiary.setEnd("gone", nil)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{
		"machine-end " + dispatchA + " gone",
		"machine-end " + dispatchB + " gone",
		"dispatched " + dispatchC + " i-c -",
	})
	equal(t, "the journal", h.entries(), []journal.Entry{dispatched(dispatchC, "i-c")})
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestGoneIsSentPastTheLimitUntilAccepted keeps a failing post of gone for more than
// 24 hours, with waits that double from one watch up to one hour, and removes the entry
// once Qory Apiary accepts gone. No line of the log is an error.
func TestGoneIsSentPastTheLimitUntilAccepted(t *testing.T) {
	h := newHarness(t)
	h.apiary.setEnd("gone", endFailed)
	stoppedMachine(h)
	h.tick()
	equal(t, "Qory Apiary's calls", withoutLogs(h.apiary.takeCalls()), []string{"machine-end " + dispatchA + " gone"})
	var posted []int
	for i := 1; i <= 183; i++ {
		h.tick()
		if calls := h.apiary.takeCalls(); len(calls) > 0 {
			equal(t, "Qory Apiary's calls", calls, []string{"machine-end " + dispatchA + " gone"})
			posted = append(posted, i)
		}
	}
	// The watches after the first failure that send gone: waits of 1, 2, 4, 8, 16 and
	// 32 watches, and then 60.
	equal(t, "the watches that send gone", posted, []int{1, 3, 7, 15, 31, 63, 123, 183})
	h.clock.Advance(loop.MachineEndLimit)
	h.clock.waitPending(t, loop.WatchInterval)
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " gone"})
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a")})
	h.apiary.setEnd("gone", nil)
	for range 59 {
		h.tick()
	}
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " gone"})
	equal(t, "the journal", h.entries(), nil)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	for _, l := range h.lines() {
		if l["level"] == "ERROR" {
			t.Errorf("the log has an error line: %v", l)
		}
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAWaitingEndedEntryLetsTheNextOneGo keeps two ended entries past 24 hours while
// gone fails. The pause skips the second entry while the first is tried, and the
// second gets its post while the first waits.
func TestAWaitingEndedEntryLetsTheNextOneGo(t *testing.T) {
	h := newHarness(t)
	h.setEntries(ended(dispatchA, "i-a"), ended(dispatchB, "i-b"))
	h.apiary.setEnd("gone", endFailed)
	h.start()
	h.clock.Advance(loop.MachineEndLimit)
	h.clock.waitPending(t, loop.WatchInterval)
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " gone"})
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " gone"})
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchB + " gone"})
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a"), ended(dispatchB, "i-b")})
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestRestartKeepsAnEndedEntry resumes an ended entry with no dispatched and no delete,
// also when the list has a machine with its key. A restart starts the waits again, so
// the first watch after it sends gone.
func TestRestartKeepsAnEndedEntry(t *testing.T) {
	h := newHarness(t)
	h.apiary.setEnd("gone", endFailed)
	stoppedMachine(h)
	h.tick()
	h.tick()
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
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " gone"})
	equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
	equal(t, "the journal", h.entries(), []journal.Entry{ended(dispatchA, "i-a")})
	h.apiary.setEnd("gone", nil)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchA + " gone"})
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestALaggingListMakesNoStrayOfAGoneMachine skips a machine that the list still shows
// as deleting after its delete returned gone and gone was accepted. After the list
// grace, it is a stray.
func TestALaggingListMakesNoStrayOfAGoneMachine(t *testing.T) {
	h := newHarness(t)
	stoppedMachine(h)
	h.tick()
	equal(t, "Qory Apiary's calls", withoutLogs(h.apiary.takeCalls()), []string{"machine-end " + dispatchA + " gone"})
	equal(t, "the journal", h.entries(), nil)
	h.machines.add("i-a", dispatchA, contracts.MachineDeleting, epoch)
	h.machines.takeCalls()
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
	if n := len(h.find("found a machine with no journal entry")); n != 0 {
		t.Errorf("the log has %d lines for a stray", n)
	}
	h.clock.Advance(loop.ListGrace)
	h.clock.waitPending(t, loop.WatchInterval)
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"delete i-a"})
	if n := len(h.find("found a machine with no journal entry")); n != 1 {
		t.Errorf("the log has %d lines for a stray", n)
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAnAcceptedPendingGoneSkipsItsMachineInTheSameWatch posts the pending gone of a
// stray while the list still shows its machine as deleting. The stray loop of that
// watch skips the machine.
func TestAnAcceptedPendingGoneSkipsItsMachineInTheSameWatch(t *testing.T) {
	h := newHarness(t)
	failingStrayGone(h, endFailed)
	h.machines.add("i-s", dispatchC, contracts.MachineDeleting, epoch)
	h.apiary.setEnd("gone", nil)
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"machine-end " + dispatchC + " gone"})
	equal(t, "the adapter's calls", h.machines.takeCalls(), nil)
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}
