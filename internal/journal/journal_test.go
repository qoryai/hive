package journal_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/qoryai/hive/internal/journal"
)

const (
	dispatchA = "dp_0123456789abcdef"
	dispatchB = "dp_ghjkmnpqrstvwxyz"
	// machine is a machine id. The journal treats a machine id as opaque.
	machine = "m-0123456789abcdef"
	// oddID is a machine id with punctuation, in the shape of the adapter protocol.
	oddID = "pool/a:01~x"
	// lifetime is a machine's lifetime in minutes.
	lifetime = 60
)

// longID is a machine id of 128 characters, the longest that the shape allows.
var longID = strings.Repeat("z", 128)

// open opens the journal in dir and fails the test on an error.
func open(t *testing.T, dir string) *journal.Journal {
	t.Helper()
	j, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// lines is the journal file's lines in dir.
func lines(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, journal.File))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// TestAJournalThatDoesNotExistIsEmpty opens a folder with no journal and writes no
// file.
func TestAJournalThatDoesNotExistIsEmpty(t *testing.T) {
	dir := t.TempDir()
	if got := open(t, dir).List(); len(got) != 0 {
		t.Errorf("the entries are %v", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, journal.File)); !os.IsNotExist(err) {
		t.Errorf("the journal file: %v", err)
	}
}

// TestEachPhaseIsWrittenAndReadBack moves one dispatch through every phase. After each
// change the file has the line, mode 0600, no new file is left beside it, and a journal
// opened again lists the same entry.
func TestEachPhaseIsWrittenAndReadBack(t *testing.T) {
	dir := t.TempDir()
	j := open(t, dir)
	for _, c := range []struct {
		phase, machine string
		lifetime       int
		line           string
	}{
		{journal.PhaseClaimed, "", lifetime, dispatchA + " claimed - 60"},
		{journal.PhaseMade, machine, lifetime, dispatchA + " made " + machine + " 60"},
		{journal.PhaseDispatched, machine, lifetime, dispatchA + " dispatched " + machine + " 60"},
		{journal.PhaseDispatched, oddID, 15, dispatchA + " dispatched " + oddID + " 15"},
		{journal.PhaseMade, longID, 1470, dispatchA + " made " + longID + " 1470"},
		{journal.PhaseMade, "a", 999, dispatchA + " made a 999"},
		{journal.PhaseDeleting, oddID, journal.MinLifetime, dispatchA + " deleting " + oddID + " 15"},
	} {
		e := journal.Entry{DispatchID: dispatchA, Phase: c.phase, MachineID: c.machine, MaxLifetimeMinutes: c.lifetime}
		if err := j.Set(e); err != nil {
			t.Fatalf("%s: %v", c.phase, err)
		}
		if got := lines(t, dir); !slices.Equal(got, []string{c.line}) {
			t.Errorf("%s: the journal is %q, want %q", c.phase, got, c.line)
		}
		info, err := os.Stat(filepath.Join(dir, journal.File))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: the journal file: %v, %v", c.phase, info, err)
		}
		if _, err := os.Lstat(filepath.Join(dir, journal.File+".new")); !os.IsNotExist(err) {
			t.Errorf("%s: the new file is left: %v", c.phase, err)
		}
		if got := open(t, dir).List(); !slices.Equal(got, []journal.Entry{e}) {
			t.Errorf("%s: the journal opened again lists %v, want %v", c.phase, got, e)
		}
		if got := j.List(); !slices.Equal(got, []journal.Entry{e}) {
			t.Errorf("%s: the journal lists %v, want %v", c.phase, got, e)
		}
	}
}

// TestIsDispatchID accepts dp_ and 16 Crockford base32 characters and nothing else.
func TestIsDispatchID(t *testing.T) {
	for id, want := range map[string]bool{
		dispatchA:              true,
		"":                     false,
		"i-0123456789abcdef":   false,
		"dp_0123456789abcde":   false,
		"dp_0123456789ABCDEF":  false,
		"dp_0123456789abcdefg": false,
	} {
		if got := journal.IsDispatchID(id); got != want {
			t.Errorf("IsDispatchID(%q) is %t", id, got)
		}
	}
}

// TestSetKeepsTheOrderOfClaims replaces an entry in its place, adds a new one after the
// others, and removes one, leaving the rest in order. Removing a dispatch that has no
// entry leaves the file as it is.
func TestSetKeepsTheOrderOfClaims(t *testing.T) {
	dir := t.TempDir()
	j := open(t, dir)
	a := journal.Entry{DispatchID: dispatchA, Phase: journal.PhaseClaimed, MaxLifetimeMinutes: lifetime}
	b := journal.Entry{DispatchID: dispatchB, Phase: journal.PhaseClaimed, MaxLifetimeMinutes: lifetime}
	for _, e := range []journal.Entry{a, b} {
		if err := j.Set(e); err != nil {
			t.Fatal(err)
		}
	}
	a.Phase, a.MachineID, a.MaxLifetimeMinutes = journal.PhaseMade, machine, lifetime
	if err := j.Set(a); err != nil {
		t.Fatal(err)
	}
	want := []string{
		dispatchA + " made " + machine + " 60",
		dispatchB + " claimed - 60",
	}
	if got := lines(t, dir); !slices.Equal(got, want) {
		t.Errorf("the journal is %q, want %q", got, want)
	}
	if err := j.Remove(dispatchA); err != nil {
		t.Fatal(err)
	}
	if got := lines(t, dir); !slices.Equal(got, want[1:]) {
		t.Errorf("after the removal the journal is %q, want %q", got, want[1:])
	}
	path := filepath.Join(dir, journal.File)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Remove(dispatchA); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Errorf("removing a dispatch with no entry wrote the journal: %v", err)
	}
	if err := j.Remove(dispatchB); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || len(b) != 0 {
		t.Errorf("the journal with no dispatch is %q, %v", b, err)
	}
	if got := open(t, dir).List(); len(got) != 0 {
		t.Errorf("the empty journal opened again lists %v", got)
	}
}

// TestALeftNewFileIsReplaced writes the journal over a new file that an earlier write
// left beside it.
func TestALeftNewFileIsReplaced(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, journal.File+".new"), []byte("left\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := open(t, dir)
	if err := j.Set(journal.Entry{DispatchID: dispatchA, Phase: journal.PhaseClaimed, MaxLifetimeMinutes: lifetime}); err != nil {
		t.Fatal(err)
	}
	if got, want := lines(t, dir), []string{dispatchA + " claimed - 60"}; !slices.Equal(got, want) {
		t.Errorf("the journal is %q, want %q", got, want)
	}
}

// TestAFailedWriteLeavesTheJournalAsItWas makes the new file impossible to write and
// checks that the file and the entries are as they were.
func TestAFailedWriteLeavesTheJournalAsItWas(t *testing.T) {
	dir := t.TempDir()
	j := open(t, dir)
	a := journal.Entry{DispatchID: dispatchA, Phase: journal.PhaseClaimed, MaxLifetimeMinutes: lifetime}
	if err := j.Set(a); err != nil {
		t.Fatal(err)
	}
	// A folder that is not empty at the new file's path cannot be removed or opened
	// for writing.
	blocker := filepath.Join(dir, journal.File+".new")
	if err := os.MkdirAll(filepath.Join(blocker, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	b := journal.Entry{DispatchID: dispatchA, Phase: journal.PhaseMade, MachineID: machine, MaxLifetimeMinutes: lifetime}
	err := j.Set(b)
	if err == nil || !strings.HasPrefix(err.Error(), "check the state folder: writing the journal of live dispatches failed; the journal is as it was: ") {
		t.Errorf("the error is %v", err)
	}
	if got := j.List(); !slices.Equal(got, []journal.Entry{a}) {
		t.Errorf("the journal lists %v, want %v", got, a)
	}
	if got, want := lines(t, dir), []string{dispatchA + " claimed - 60"}; !slices.Equal(got, want) {
		t.Errorf("the journal is %q, want %q", got, want)
	}
}

// TestAnEntryOutOfShapeIsNotWritten sets each entry with a field out of shape. Set
// returns an error that quotes no value and leaves the journal as it was.
func TestAnEntryOutOfShapeIsNotWritten(t *testing.T) {
	const (
		dispatchShape = "the dispatch id is not dp_ and 16 Crockford base32 characters"
		machineShape  = "the machine id is not 1 to 128 printable ASCII characters other than a space"
		secret        = "the machine id contains qak_, qrk_, qecr_ or qecp_, the start of a secret"
		lifetimeRange = "the lifetime is not 15 to 1470 minutes"
	)
	ok := journal.Entry{DispatchID: dispatchA, Phase: journal.PhaseClaimed, MaxLifetimeMinutes: lifetime}
	for _, c := range []struct {
		name  string
		edit  func(*journal.Entry)
		cause string
	}{
		{"a dispatch id with an i", func(e *journal.Entry) { e.DispatchID = "dp_0123456789abcdei" }, dispatchShape},
		{"a dispatch id in upper case", func(e *journal.Entry) { e.DispatchID = "dp_0123456789ABCDEF" }, dispatchShape},
		{"a short dispatch id", func(e *journal.Entry) { e.DispatchID = "dp_0123" }, dispatchShape},
		{"an unknown phase", func(e *journal.Entry) { e.Phase = "started" }, "the phase is not claimed, made, dispatched or deleting"},
		{"an old phase", func(e *journal.Entry) { e.Phase = "handed_over" }, "the phase is not claimed, made, dispatched or deleting"},
		{"claimed with a machine id", func(e *journal.Entry) { e.MachineID = machine }, "the machine id is set in the phase claimed"},
		{"claimed with -", func(e *journal.Entry) { e.MachineID = "-" }, "the machine id is set in the phase claimed"},
		{"made with no machine id", func(e *journal.Entry) { e.Phase = journal.PhaseMade }, "the phase made, dispatched or deleting has no machine id"},
		{"dispatched with no machine id", func(e *journal.Entry) { e.Phase = journal.PhaseDispatched }, "the phase made, dispatched or deleting has no machine id"},
		{"deleting with no machine id", func(e *journal.Entry) { e.Phase = journal.PhaseDeleting }, "the phase made, dispatched or deleting has no machine id"},
		{"made with -", func(e *journal.Entry) { e.Phase, e.MachineID = journal.PhaseMade, "-" }, "the machine id is -, which the journal writes for no machine id"},
		{"a machine id of 129", func(e *journal.Entry) { e.Phase, e.MachineID = journal.PhaseMade, longID+"z" }, machineShape},
		{"a machine id with a space", func(e *journal.Entry) { e.Phase, e.MachineID = journal.PhaseMade, "m 1" }, machineShape},
		{"a machine id with a line feed", func(e *journal.Entry) { e.Phase, e.MachineID = journal.PhaseMade, "m\n1" }, machineShape},
		{"a machine id with a tab", func(e *journal.Entry) { e.Phase, e.MachineID = journal.PhaseDispatched, "m\t1" }, machineShape},
		{"a machine id that is not ASCII", func(e *journal.Entry) { e.Phase, e.MachineID = journal.PhaseMade, "m-é" }, machineShape},
		{"a machine id with qak_", func(e *journal.Entry) { e.Phase, e.MachineID = journal.PhaseMade, "x-qak_abc" }, secret},
		{"a machine id with qrk_", func(e *journal.Entry) { e.Phase, e.MachineID = journal.PhaseMade, "qrk_abc" }, secret},
		{"a machine id with qecr_", func(e *journal.Entry) { e.Phase, e.MachineID = journal.PhaseDispatched, "qecr_abc" }, secret},
		{"a machine id with qecp_", func(e *journal.Entry) { e.Phase, e.MachineID = journal.PhaseMade, "x/qecp_abc" }, secret},
		{"claimed with no lifetime", func(e *journal.Entry) { e.MaxLifetimeMinutes = 0 }, lifetimeRange},
		{"made with no lifetime", func(e *journal.Entry) { e.Phase, e.MachineID, e.MaxLifetimeMinutes = journal.PhaseMade, machine, 0 }, lifetimeRange},
		{"dispatched with no lifetime", func(e *journal.Entry) {
			e.Phase, e.MachineID, e.MaxLifetimeMinutes = journal.PhaseDispatched, machine, 0
		}, lifetimeRange},
		{"claimed with a lifetime of 1471", func(e *journal.Entry) { e.MaxLifetimeMinutes = 1471 }, lifetimeRange},
		{"a lifetime of 14", func(e *journal.Entry) { e.Phase, e.MachineID, e.MaxLifetimeMinutes = journal.PhaseMade, machine, 14 }, lifetimeRange},
		{"a lifetime of 1471", func(e *journal.Entry) {
			e.Phase, e.MachineID, e.MaxLifetimeMinutes = journal.PhaseDispatched, machine, 1471
		}, lifetimeRange},
		{"a lifetime below 0", func(e *journal.Entry) { e.Phase, e.MachineID, e.MaxLifetimeMinutes = journal.PhaseMade, machine, -60 }, lifetimeRange},
		{"claimed with qrk_", func(e *journal.Entry) { e.MachineID = "qrk_abc" }, secret},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			j := open(t, dir)
			if err := j.Set(ok); err != nil {
				t.Fatal(err)
			}
			e := ok
			c.edit(&e)
			err := j.Set(e)
			want := "fix the journal entry: " + c.cause + "; the journal of live dispatches is as it was"
			if err == nil || err.Error() != want {
				t.Errorf("%v, want %q", err, want)
			}
			if got := j.List(); !slices.Equal(got, []journal.Entry{ok}) {
				t.Errorf("the journal lists %v", got)
			}
			if got, want := lines(t, dir), []string{dispatchA + " claimed - 60"}; !slices.Equal(got, want) {
				t.Errorf("the journal is %q, want %q", got, want)
			}
		})
	}
}

// TestAJournalOutOfShapeIsNotRead opens a journal file that is not a regular file of
// at most 1 MiB, or whose lines are out of shape or repeat a dispatch. Open returns an
// error that gives the line's number and quotes no value.
func TestAJournalOutOfShapeIsNotRead(t *testing.T) {
	const entry = dispatchA + " claimed - 60"
	const other = dispatchB + " made " + machine + " 60"
	const fields = "the line is not four fields separated by single spaces and ended by a line feed"
	const lifetimeText = "the lifetime is not a whole number of minutes"
	const run = "0192f0c1-7d4e-7a2b-8c3d-00000000000a"
	for _, c := range []struct {
		name, content string
		link, folder  bool
		large         bool
		line          int
		cause         string
	}{
		{name: "a line of two", content: entry + "\n" + dispatchB + " claimed\n", line: 2, cause: fields},
		{name: "a line of three", content: dispatchA + " claimed -\n", line: 1, cause: fields},
		{name: "a line of five", content: entry + " -\n", line: 1, cause: fields},
		{name: "the old three fields with a machine", content: dispatchA + " made " + machine + "\n", line: 1, cause: fields},
		{name: "the old five fields", content: dispatchA + " " + run + " explore claimed -\n", line: 1, cause: fields},
		{name: "the old five fields with a machine", content: dispatchA + " " + run + " explore created i-0123456789abcdef0\n", line: 1, cause: fields},
		{name: "no line feed", content: entry, line: 1, cause: fields},
		{name: "two spaces", content: dispatchA + "  claimed - 60\n", line: 1, cause: fields},
		{name: "a space at the end", content: entry + " \n", line: 1, cause: fields},
		{name: "a carriage return", content: entry + "\r\n", line: 1, cause: lifetimeText},
		{name: "a carriage return after a lifetime", content: other + "\r\n", line: 1, cause: lifetimeText},
		{name: "a carriage return after a machine id", content: dispatchB + " made " + machine + "\r 60\n", line: 1, cause: "the machine id is not 1 to 128 printable ASCII characters other than a space"},
		{name: "an empty line", content: entry + "\n\n", line: 2, cause: fields},
		{name: "an unknown phase", content: dispatchA + " running - 60\n", line: 1, cause: "the phase is not claimed, made, dispatched or deleting"},
		{name: "a dispatch id out of shape", content: "dp_0 claimed - 60\n", line: 1, cause: "the dispatch id is not dp_ and 16 Crockford base32 characters"},
		{name: "made with -", content: dispatchA + " made - 60\n", line: 1, cause: "the phase made, dispatched or deleting has no machine id"},
		{name: "dispatched with -", content: dispatchA + " dispatched - 60\n", line: 1, cause: "the phase made, dispatched or deleting has no machine id"},
		{name: "deleting with -", content: dispatchA + " deleting - 60\n", line: 1, cause: "the phase made, dispatched or deleting has no machine id"},
		{name: "claimed with a machine id", content: dispatchA + " claimed " + machine + " 60\n", line: 1, cause: "the machine id is set in the phase claimed"},
		{name: "a machine id of 129", content: dispatchA + " made " + longID + "z 60\n", line: 1, cause: "the machine id is not 1 to 128 printable ASCII characters other than a space"},
		{name: "a machine id with qrk_", content: dispatchA + " made qrk_abc 60\n", line: 1, cause: "the machine id contains qak_, qrk_, qecr_ or qecp_, the start of a secret"},
		{name: "claimed with the lifetime -", content: dispatchA + " claimed - -\n", line: 1, cause: lifetimeText},
		{name: "made with the lifetime -", content: dispatchA + " made " + machine + " -\n", line: 1, cause: lifetimeText},
		{name: "claimed with a lifetime of 14", content: dispatchA + " claimed - 14\n", line: 1, cause: "the lifetime is not 15 to 1470 minutes"},
		{name: "a lifetime of 14", content: dispatchA + " made " + machine + " 14\n", line: 1, cause: "the lifetime is not 15 to 1470 minutes"},
		{name: "a lifetime of 1471", content: dispatchA + " dispatched " + machine + " 1471\n", line: 1, cause: "the lifetime is not 15 to 1470 minutes"},
		{name: "a lifetime of 5 digits", content: dispatchA + " made " + machine + " 10000\n", line: 1, cause: lifetimeText},
		{name: "a lifetime with a leading zero", content: dispatchA + " made " + machine + " 060\n", line: 1, cause: lifetimeText},
		{name: "a lifetime of 0", content: dispatchA + " made " + machine + " 0\n", line: 1, cause: lifetimeText},
		{name: "a lifetime with a sign", content: dispatchA + " made " + machine + " +60\n", line: 1, cause: lifetimeText},
		{name: "a lifetime with a fraction", content: dispatchA + " made " + machine + " 60.0\n", line: 1, cause: lifetimeText},
		{name: "a dispatch twice", content: entry + "\n" + other + "\n" + dispatchA + " made " + machine + " 60\n", line: 3},
		{name: "a link", link: true},
		{name: "a folder", folder: true},
		{name: "too large", large: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, journal.File)
			switch {
			case c.link:
				target := filepath.Join(t.TempDir(), "elsewhere")
				if err := os.WriteFile(target, []byte(entry+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case c.folder:
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case c.large:
				if err := os.WriteFile(path, []byte(strings.Repeat(entry+"\n", 1<<20/len(entry)+1)), 0o600); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := journal.Open(dir)
			var want string
			switch {
			case c.link || c.folder:
				want = "repair or remove " + path + ": the journal of live dispatches is not a regular file"
			case c.large:
				want = "repair or remove " + path + ": the journal of live dispatches is larger than 1 MiB"
			case c.cause == "":
				want = fmt.Sprintf("repair or remove %s: line %d of the journal of live dispatches repeats the dispatch of line 1", path, c.line)
			default:
				want = fmt.Sprintf("repair or remove %s: line %d of the journal of live dispatches is out of shape: %s", path, c.line, c.cause)
			}
			if err == nil || err.Error() != want {
				t.Errorf("%v, want %q", err, want)
			}
		})
	}
}

// TestAJournalOfExactly1MiBIsRead opens a journal of 1 MiB, the most it may contain.
func TestAJournalOfExactly1MiBIsRead(t *testing.T) {
	// line is a line of n bytes, from 30 to 157, for the dispatch k.
	line := func(k, n int) string {
		return fmt.Sprintf("dp_%016d made %s 60\n", k, strings.Repeat("a", n-29))
	}
	var b strings.Builder
	k := 0
	for ; 1<<20-b.Len() > 450; k++ {
		b.WriteString(line(k, 150))
	}
	r := 1<<20 - b.Len()
	b.WriteString(line(k, r/3) + line(k+1, r/3) + line(k+2, r-2*(r/3)))
	if b.Len() != 1<<20 {
		t.Fatalf("the journal is %d bytes", b.Len())
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, journal.File), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	j, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(j.List()); n != k+3 {
		t.Errorf("the journal lists %d entries, want %d", n, k+3)
	}
}

// TestAnEntryHasOnlyItsFourFields pins the fields of an entry: none is for a secret
// or for inputs.
func TestAnEntryHasOnlyItsFourFields(t *testing.T) {
	typ := reflect.TypeFor[journal.Entry]()
	kinds := []reflect.Kind{reflect.String, reflect.String, reflect.String, reflect.Int}
	var got []string
	for i := range typ.NumField() {
		f := typ.Field(i)
		if i >= len(kinds) || f.Type.Kind() != kinds[i] {
			t.Errorf("the field %s is a %s", f.Name, f.Type)
		}
		got = append(got, f.Name)
	}
	if want := []string{"DispatchID", "Phase", "MachineID", "MaxLifetimeMinutes"}; !slices.Equal(got, want) {
		t.Errorf("the fields are %q, want %q", got, want)
	}
}

// TestNoSecretReachesTheJournal writes a dispatch through every phase while an access
// key secret, a run key, a run code, an enrolment code and inputs with marker values
// are in hand, tries to write each marker in each field, and opens journals that
// contain the markers. No marker is in the journal file, in the folder or in any
// error.
//
// A machine id is opaque, so the shape check alone does not keep inputs out of the
// machine id field: inputs reach the journal only through a caller that passes them
// as a machine id. The test writes inputs in the fields with a fixed shape, and every
// secret in every field.
func TestNoSecretReachesTheJournal(t *testing.T) {
	const (
		accessKey = "qak_MARKERaccesskey0000000000"
		runKey    = "qrk_MARKERrunkey000000000000"
		// runCode has the shape of a run code: qecr_, 26 Crockford base32 characters
		// and one or two parts of 22 base64url characters.
		runCode   = "qecr_MARKER00000000000000000000.MARKERruncode000000000"
		enrolment = "qecp_MARKERenrolment000000000"
		inputs    = `{"MARKERinput":"MARKERvalue"}`
	)
	if !regexp.MustCompile(`^qecr_[0-9A-HJKMNP-TV-Z]{26}(\.[A-Za-z0-9_-]{22}){1,2}$`).MatchString(runCode) {
		t.Fatalf("the run code marker %q is not in the shape of a run code", runCode)
	}
	secrets := []string{accessKey, runKey, runCode, enrolment}
	markers := append([]string{"MARKER", inputs}, secrets...)
	dir := t.TempDir()
	j := open(t, dir)
	var errs []string
	for _, phase := range []string{journal.PhaseClaimed, journal.PhaseMade, journal.PhaseDispatched} {
		e := journal.Entry{DispatchID: dispatchA, Phase: phase, MaxLifetimeMinutes: lifetime}
		if phase != journal.PhaseClaimed {
			e.MachineID = machine
		}
		if err := j.Set(e); err != nil {
			t.Fatal(err)
		}
		var edits []func(*journal.Entry)
		for _, m := range markers[1:] {
			edits = append(edits,
				func(x *journal.Entry) { x.DispatchID = m },
				func(x *journal.Entry) { x.DispatchID = "dp_" + m },
				func(x *journal.Entry) { x.Phase = m },
			)
		}
		for _, m := range secrets {
			edits = append(edits,
				func(x *journal.Entry) { x.MachineID = m },
				func(x *journal.Entry) { x.MachineID = "m-" + m },
				func(x *journal.Entry) { x.Phase, x.MachineID, x.MaxLifetimeMinutes = journal.PhaseMade, m, lifetime },
				func(x *journal.Entry) {
					x.Phase, x.MachineID, x.MaxLifetimeMinutes = journal.PhaseDispatched, "m/"+m, lifetime
				},
			)
		}
		for _, edit := range edits {
			bad := e
			edit(&bad)
			err := j.Set(bad)
			if err == nil {
				t.Fatalf("an entry with a marker was written: %v", bad)
			}
			errs = append(errs, err.Error())
		}
	}
	if err := j.Remove(dispatchA); err != nil {
		t.Fatal(err)
	}
	// Open returns an error for a journal that contains a marker on a line, and the
	// error quotes no line.
	for _, m := range markers[1:] {
		for _, line := range []string{
			dispatchA + " claimed " + m + " 60\n",
			dispatchA + " " + m + " - 60\n",
			dispatchA + " made " + machine + " " + m + "\n",
			m + " claimed - 60\n",
			m + "\n",
		} {
			other := t.TempDir()
			if err := os.WriteFile(filepath.Join(other, journal.File), []byte(line), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := journal.Open(other)
			if err == nil {
				t.Fatalf("a journal with a marker was read: %q", line)
			}
			errs = append(errs, err.Error())
		}
	}
	for _, m := range secrets {
		for _, line := range []string{
			dispatchA + " made " + m + " 60\n",
			dispatchA + " dispatched m-" + m + " 60\n",
		} {
			other := t.TempDir()
			if err := os.WriteFile(filepath.Join(other, journal.File), []byte(line), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := journal.Open(other)
			if err == nil {
				t.Fatalf("a journal with a secret was read: %q", line)
			}
			errs = append(errs, err.Error())
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range entries {
		b, err := os.ReadFile(filepath.Join(dir, f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range markers {
			if strings.Contains(string(b), m) || strings.Contains(f.Name(), m) {
				t.Errorf("the file %s contains %q", f.Name(), m)
			}
		}
	}
	for _, e := range errs {
		for _, m := range markers {
			if strings.Contains(e, m) {
				t.Errorf("the error %q contains %q", e, m)
			}
		}
	}
}
