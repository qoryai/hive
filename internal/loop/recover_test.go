package loop_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/qoryai/hive/internal/adapter"
	"github.com/qoryai/hive/internal/journal"
	"github.com/qoryai/hive/internal/loop"
	"github.com/qoryai/integrations/contracts"
)

// TestRestartRecovery starts the loop with a journal from before a restart. It lists
// the machines once and acts on each entry: a claimed entry with no machine is
// released and removed; a claimed entry with a machine becomes made with the
// machine's id and keeps the claim's lifetime, and gets dispatched; a made entry gets
// dispatched, also when its machine is not listed; an entry whose machine is deleting
// moves to the phase deleting; a dispatched entry with a machine stays for the watch.
func TestRestartRecovery(t *testing.T) {
	h := newHarness(t)
	h.config.MaxMachines = 6
	h.setEntries(
		journal.Entry{DispatchID: dispatchA, Phase: journal.PhaseClaimed, MaxLifetimeMinutes: lifetime},
		journal.Entry{DispatchID: dispatchB, Phase: journal.PhaseClaimed, MaxLifetimeMinutes: lifetime},
		journal.Entry{DispatchID: dispatchC, Phase: journal.PhaseMade, MachineID: "i-c", MaxLifetimeMinutes: lifetime},
		dispatched(dispatchD, "i-d"),
		journal.Entry{DispatchID: dispatchE, Phase: journal.PhaseMade, MachineID: "i-e", MaxLifetimeMinutes: lifetime},
		dispatched(dispatchF, "i-f"),
	)
	h.machines.add("i-b", dispatchB, contracts.MachineStarting, epoch)
	h.machines.add("i-d", dispatchD, contracts.MachineDeleting, epoch)
	h.machines.add("i-e", dispatchE, contracts.MachineRunning, epoch)
	h.machines.add("i-f", dispatchF, contracts.MachineRunning, epoch)
	h.start()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{
		"release " + dispatchA + " " + journal.ReasonRestarted,
		"dispatched " + dispatchB + " i-b",
		"dispatched " + dispatchC + " i-c",
		"dispatched " + dispatchE + " i-e",
	})
	h.machines.mu.Lock()
	equal(t, "the adapter's calls", h.machines.calls, []string{"check", "list " + pool})
	h.machines.mu.Unlock()
	want := []journal.Entry{
		dispatched(dispatchB, "i-b"),
		dispatched(dispatchC, "i-c"),
		deleting(dispatchD, "i-d"),
		dispatched(dispatchE, "i-e"),
		dispatched(dispatchF, "i-f"),
	}
	equal(t, "the journal", h.entries(), want)
	h.openJournal()
	equal(t, "the journal read again", h.entries(), want)
	if n := len(h.find("the machine of a recovered dispatch is deleting")); n != 1 {
		t.Errorf("the log has %d lines for entries that moved to the phase deleting", n)
	}
	if n := len(h.find("resumed a dispatch")); n != 4 {
		t.Errorf("the log has %d lines for resumed entries", n)
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestARecoveredDispatchThatEndedIsCleanedUp cleans up the machine of a recovered made
// entry whose dispatched returns 410 gone.
func TestARecoveredDispatchThatEndedIsCleanedUp(t *testing.T) {
	h := newHarness(t)
	h.setEntries(journal.Entry{DispatchID: dispatchA, Phase: journal.PhaseClaimed, MaxLifetimeMinutes: lifetime})
	h.machines.add("i-a", dispatchA, contracts.MachineRunning, epoch)
	h.apiary.setDispatched(dispatchA, loop.ErrGone)
	h.start()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{
		"dispatched " + dispatchA + " i-a",
		"machine-log " + dispatchA + " false",
		"machine-end " + dispatchA + " gone",
	})
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"check", "log i-a", "delete i-a"})
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAFailedCheckStopsTheLoop returns an error that starts with the fix and lists
// nothing.
func TestAFailedCheckStopsTheLoop(t *testing.T) {
	h := newHarness(t)
	h.machines.checkErr = &adapter.Error{Command: "check", Kind: contracts.FailureDefect, Message: "Give the role the right to list machines."}
	h.startNoWait()
	err := h.result()
	want := "Fix the machine adapter's settings or the rights it uses, then start the provisioner again: the machine adapter's check failed: Give the role the right to list machines. (machine adapter command check, kind defect)"
	if err == nil || err.Error() != want {
		t.Errorf("the error is %v, want %q", err, want)
	}
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"check"})
	if n := h.apiary.claimCount(); n != 0 {
		t.Errorf("the loop asked for %d claims", n)
	}
}

// TestAFailedListAtTheStartStopsTheLoop returns an error that starts with the fix and
// leaves the journal as it is.
func TestAFailedListAtTheStartStopsTheLoop(t *testing.T) {
	h := newHarness(t)
	h.setEntries(journal.Entry{DispatchID: dispatchA, Phase: journal.PhaseClaimed, MaxLifetimeMinutes: lifetime})
	h.machines.listErr = errors.New("the provisioner cancelled the machine adapter's list")
	h.startNoWait()
	err := h.result()
	if err == nil || !strings.HasPrefix(err.Error(), "Check the machine adapter, then start the provisioner again: listing the pool's machines to recover the journal of live dispatches failed: ") {
		t.Errorf("the error is %v", err)
	}
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	equal(t, "the journal", h.entries(), []journal.Entry{{DispatchID: dispatchA, Phase: journal.PhaseClaimed, MaxLifetimeMinutes: lifetime}})
}
