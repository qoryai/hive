package journal_test

import (
	"testing"

	"github.com/qoryai/hive/internal/journal"
)

// TestRecover returns one action for every phase and every result of the lookup of
// the machine by its client token.
func TestRecover(t *testing.T) {
	release := journal.Action{Kind: journal.Release, Retry: true, Reason: journal.ReasonNoMachine}
	finish := journal.Action{Kind: journal.Finish, Retry: false, Reason: journal.ReasonTerminated}
	resume := journal.Action{Kind: journal.Resume, InstanceID: instance}
	lookups := []struct {
		name   string
		lookup journal.Lookup
	}{
		{"not found", journal.Lookup{}},
		{"pending", journal.Lookup{Found: true, InstanceID: instance, State: "pending"}},
		{"running", journal.Lookup{Found: true, InstanceID: instance, State: "running"}},
		{"stopping", journal.Lookup{Found: true, InstanceID: instance, State: "stopping"}},
		{"stopped", journal.Lookup{Found: true, InstanceID: instance, State: "stopped"}},
		{"shutting-down", journal.Lookup{Found: true, InstanceID: instance, State: journal.StateShuttingDown}},
		{"terminated", journal.Lookup{Found: true, InstanceID: instance, State: journal.StateTerminated}},
	}
	// want is the action for each lookup, in the order of lookups.
	before := []journal.Action{release, release, release, release, release, release, release}
	after := []journal.Action{release, resume, resume, resume, resume, finish, finish}
	for _, c := range []struct {
		phase, instance string
		want            []journal.Action
	}{
		{journal.PhaseClaimed, "", before},
		{journal.PhaseDispatched, "", before},
		{journal.PhaseHandover, "", before},
		{journal.PhaseHandedOver, "", after},
		{journal.PhaseCreated, instance, after},
		{journal.PhaseStopped, instance, after},
	} {
		e := journal.Entry{DispatchID: dispatchA, RunID: runA, Step: "explore", Phase: c.phase, InstanceID: c.instance}
		for i, l := range lookups {
			if got := journal.Recover(e, l.lookup); got != c.want[i] {
				t.Errorf("%s, %s: %+v, want %+v", c.phase, l.name, got, c.want[i])
			}
		}
	}
}

// TestResumeWatchesTheMachineFound resumes the watch of the instance the lookup
// found.
func TestResumeWatchesTheMachineFound(t *testing.T) {
	e := journal.Entry{DispatchID: dispatchA, RunID: runA, Step: "explore", Phase: journal.PhaseHandedOver}
	got := journal.Recover(e, journal.Lookup{Found: true, InstanceID: shortID, State: "running"})
	if want := (journal.Action{Kind: journal.Resume, InstanceID: shortID}); got != want {
		t.Errorf("%+v, want %+v", got, want)
	}
}

// TestTheReasonsFitTheContract checks that each reason is 1 to 200 characters.
func TestTheReasonsFitTheContract(t *testing.T) {
	for _, r := range []string{journal.ReasonNoMachine, journal.ReasonTerminated} {
		if n := len([]rune(r)); n < 1 || n > 200 {
			t.Errorf("%q is %d characters", r, n)
		}
	}
}
