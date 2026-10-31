package journal_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/qoryai/hive/internal/journal"
)

// TestRecover returns one action for every phase and every result of the lookup of
// the machine by its key, the dispatch id.
func TestRecover(t *testing.T) {
	release := journal.Action{Kind: journal.Release, Reason: journal.ReasonRestarted}
	deleting := journal.Action{Kind: journal.Deleting, MachineID: oddID}
	resume := journal.Action{Kind: journal.Resume, MachineID: oddID}
	// keep resumes with the entry's machine id, when the list has no machine.
	keep := journal.Action{Kind: journal.Resume, MachineID: machine}
	lookups := []struct {
		name   string
		lookup journal.Lookup
	}{
		{"not found", journal.Lookup{}},
		{"starting", journal.Lookup{Found: true, MachineID: oddID, State: "starting"}},
		{"running", journal.Lookup{Found: true, MachineID: oddID, State: "running"}},
		{"stopped", journal.Lookup{Found: true, MachineID: oddID, State: "stopped"}},
		{"deleting", journal.Lookup{Found: true, MachineID: oddID, State: "deleting"}},
	}
	// want is the action for each lookup, in the order of lookups.
	for _, c := range []struct {
		phase, machine string
		want           []journal.Action
	}{
		{journal.PhaseClaimed, "", []journal.Action{release, resume, resume, resume, deleting}},
		{journal.PhaseMade, machine, []journal.Action{keep, resume, resume, resume, deleting}},
		{journal.PhaseDispatched, machine, []journal.Action{keep, resume, resume, resume, deleting}},
		{journal.PhaseDeleting, machine, []journal.Action{keep, keep, keep, keep, keep}},
	} {
		e := journal.Entry{DispatchID: dispatchA, Phase: c.phase, MachineID: c.machine, MaxLifetimeMinutes: lifetime}
		for i, l := range lookups {
			if got := journal.Recover(e, l.lookup); got != c.want[i] {
				t.Errorf("%s, %s: %+v, want %+v", c.phase, l.name, got, c.want[i])
			}
		}
	}
}

// TestResumeUsesTheMachineFound resumes with the machine id of the lookup. A claimed
// entry has no machine id, so the lookup is the only source of one.
func TestResumeUsesTheMachineFound(t *testing.T) {
	e := journal.Entry{DispatchID: dispatchA, Phase: journal.PhaseClaimed, MaxLifetimeMinutes: lifetime}
	got := journal.Recover(e, journal.Lookup{Found: true, MachineID: longID, State: "running"})
	if want := (journal.Action{Kind: journal.Resume, MachineID: longID}); got != want {
		t.Errorf("%+v, want %+v", got, want)
	}
}

// TestTheReasonFitsTheContract pins the release reason and checks that it is 1 to 200
// characters.
func TestTheReasonFitsTheContract(t *testing.T) {
	if want := "the provisioner restarted before a machine was made"; journal.ReasonRestarted != want {
		t.Errorf("the reason is %q, want %q", journal.ReasonRestarted, want)
	}
	if n := len([]rune(journal.ReasonRestarted)); n < 1 || n > 200 {
		t.Errorf("%q is %d characters", journal.ReasonRestarted, n)
	}
}

// TestAnActionHasNoRetry pins the fields of an action: a release has a reason and no
// retry.
func TestAnActionHasNoRetry(t *testing.T) {
	typ := reflect.TypeFor[journal.Action]()
	var got []string
	for i := range typ.NumField() {
		got = append(got, typ.Field(i).Name)
	}
	if want := []string{"Kind", "Reason", "MachineID"}; !slices.Equal(got, want) {
		t.Errorf("the fields are %q, want %q", got, want)
	}
}
