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
	runA      = "0192f0c1-7d4e-7a2b-8c3d-00000000000a"
	runB      = "0192f0c1-7d4e-7a2b-8c3d-00000000000b"
	instance  = "i-0123456789abcdef0"
	shortID   = "i-01234567"
)

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
		phase, instance, line string
	}{
		{journal.PhaseClaimed, "", dispatchA + " " + runA + " explore claimed -"},
		{journal.PhaseDispatched, "", dispatchA + " " + runA + " explore dispatched -"},
		{journal.PhaseHandover, "", dispatchA + " " + runA + " explore handover -"},
		{journal.PhaseHandedOver, "", dispatchA + " " + runA + " explore handed_over -"},
		{journal.PhaseCreated, instance, dispatchA + " " + runA + " explore created " + instance},
		{journal.PhaseStopped, instance, dispatchA + " " + runA + " explore stopped " + instance},
		{journal.PhaseStopped, shortID, dispatchA + " " + runA + " explore stopped " + shortID},
	} {
		e := journal.Entry{DispatchID: dispatchA, RunID: runA, Step: "explore", Phase: c.phase, InstanceID: c.instance}
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

// TestSetKeepsTheOrderOfClaims replaces an entry in its place, adds a new one after the
// others, and removes one, leaving the rest in order. Removing a dispatch that has no
// entry leaves the file as it is.
func TestSetKeepsTheOrderOfClaims(t *testing.T) {
	dir := t.TempDir()
	j := open(t, dir)
	a := journal.Entry{DispatchID: dispatchA, RunID: runA, Step: "explore", Phase: journal.PhaseClaimed}
	b := journal.Entry{DispatchID: dispatchB, RunID: runB, Step: "prebuild", Phase: journal.PhaseClaimed}
	for _, e := range []journal.Entry{a, b} {
		if err := j.Set(e); err != nil {
			t.Fatal(err)
		}
	}
	a.Phase, a.InstanceID = journal.PhaseCreated, instance
	if err := j.Set(a); err != nil {
		t.Fatal(err)
	}
	want := []string{
		dispatchA + " " + runA + " explore created " + instance,
		dispatchB + " " + runB + " prebuild claimed -",
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
	if err := j.Set(journal.Entry{DispatchID: dispatchA, RunID: runA, Step: "explore", Phase: journal.PhaseClaimed}); err != nil {
		t.Fatal(err)
	}
	if got, want := lines(t, dir), []string{dispatchA + " " + runA + " explore claimed -"}; !slices.Equal(got, want) {
		t.Errorf("the journal is %q, want %q", got, want)
	}
}

// TestAFailedWriteLeavesTheJournalAsItWas makes the new file impossible to write and
// checks that the file and the entries are as they were.
func TestAFailedWriteLeavesTheJournalAsItWas(t *testing.T) {
	dir := t.TempDir()
	j := open(t, dir)
	a := journal.Entry{DispatchID: dispatchA, RunID: runA, Step: "explore", Phase: journal.PhaseClaimed}
	if err := j.Set(a); err != nil {
		t.Fatal(err)
	}
	// A folder that is not empty at the new file's path cannot be removed or opened
	// for writing.
	blocker := filepath.Join(dir, journal.File+".new")
	if err := os.MkdirAll(filepath.Join(blocker, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	b := a
	b.Phase = journal.PhaseDispatched
	err := j.Set(b)
	if err == nil || !strings.HasPrefix(err.Error(), "check the state folder: writing the journal of live dispatches failed; the journal is as it was: ") {
		t.Errorf("the error is %v", err)
	}
	if got := j.List(); !slices.Equal(got, []journal.Entry{a}) {
		t.Errorf("the journal lists %v, want %v", got, a)
	}
	if got, want := lines(t, dir), []string{dispatchA + " " + runA + " explore claimed -"}; !slices.Equal(got, want) {
		t.Errorf("the journal is %q, want %q", got, want)
	}
}

// TestAnEntryOutOfShapeIsNotWritten sets each entry with a field out of shape. Set
// returns an error that quotes no value and leaves the journal as it was.
func TestAnEntryOutOfShapeIsNotWritten(t *testing.T) {
	ok := journal.Entry{DispatchID: dispatchA, RunID: runA, Step: "explore", Phase: journal.PhaseClaimed}
	for _, c := range []struct {
		name  string
		edit  func(*journal.Entry)
		cause string
	}{
		{"a dispatch id with an i", func(e *journal.Entry) { e.DispatchID = "dp_0123456789abcdei" }, "the dispatch id is not dp_ and 16 Crockford base32 characters"},
		{"a dispatch id in upper case", func(e *journal.Entry) { e.DispatchID = "dp_0123456789ABCDEF" }, "the dispatch id is not dp_ and 16 Crockford base32 characters"},
		{"a short dispatch id", func(e *journal.Entry) { e.DispatchID = "dp_0123" }, "the dispatch id is not dp_ and 16 Crockford base32 characters"},
		{"a run id in upper case", func(e *journal.Entry) { e.RunID = strings.ToUpper(runA) }, "the run id is not a lower-case UUID"},
		{"a run id with a space", func(e *journal.Entry) { e.RunID = "run a" }, "the run id is not a lower-case UUID"},
		{"an empty step", func(e *journal.Entry) { e.Step = "" }, "the step is not 1 to 40 lower-case letters and underscores that start with a letter"},
		{"a step of 41", func(e *journal.Entry) { e.Step = strings.Repeat("a", 41) }, "the step is not 1 to 40 lower-case letters and underscores that start with a letter"},
		{"a step with a line feed", func(e *journal.Entry) { e.Step = "explore\nx" }, "the step is not 1 to 40 lower-case letters and underscores that start with a letter"},
		{"a step with qrk_", func(e *journal.Entry) { e.Step = "qrk_abc" }, "the step contains qak_, qrk_, qecr_ or qecp_, the start of a secret"},
		{"a step with qak_", func(e *journal.Entry) { e.Step = "x_qak_abc" }, "the step contains qak_, qrk_, qecr_ or qecp_, the start of a secret"},
		{"a step with qecr_", func(e *journal.Entry) { e.Step = "qecr_abc" }, "the step contains qak_, qrk_, qecr_ or qecp_, the start of a secret"},
		{"a step with qecp_", func(e *journal.Entry) { e.Step = "x_qecp_abc" }, "the step contains qak_, qrk_, qecr_ or qecp_, the start of a secret"},
		{"an unknown phase", func(e *journal.Entry) { e.Phase = "started" }, "the phase is not claimed, dispatched, handover, handed_over, created or stopped"},
		{"created with no instance id", func(e *journal.Entry) { e.Phase = journal.PhaseCreated }, "the instance id is not i- and 8 or 17 hex digits"},
		{"stopped with -", func(e *journal.Entry) { e.Phase, e.InstanceID = journal.PhaseStopped, "-" }, "the instance id is not i- and 8 or 17 hex digits"},
		{"an instance id of 9 hex", func(e *journal.Entry) { e.Phase, e.InstanceID = journal.PhaseCreated, "i-012345678" }, "the instance id is not i- and 8 or 17 hex digits"},
		{"an instance id in upper case", func(e *journal.Entry) { e.Phase, e.InstanceID = journal.PhaseCreated, "i-0123456789ABCDEF0" }, "the instance id is not i- and 8 or 17 hex digits"},
		{"handed_over with an instance id", func(e *journal.Entry) { e.Phase, e.InstanceID = journal.PhaseHandedOver, instance }, "the instance id is set before the phase created"},
		{"claimed with -", func(e *journal.Entry) { e.InstanceID = "-" }, "the instance id is set before the phase created"},
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
			if got, want := lines(t, dir), []string{dispatchA + " " + runA + " explore claimed -"}; !slices.Equal(got, want) {
				t.Errorf("the journal is %q, want %q", got, want)
			}
		})
	}
}

// TestAJournalOutOfShapeIsNotRead opens a journal file that is not a regular file of
// at most 1 MiB, or whose lines are out of shape or repeat a dispatch. Open returns an
// error that gives the line's number and quotes no value.
func TestAJournalOutOfShapeIsNotRead(t *testing.T) {
	const entry = dispatchA + " " + runA + " explore claimed -"
	const other = dispatchB + " " + runB + " explore created " + instance
	const fields = "the line is not five fields separated by single spaces and ended by a line feed"
	for _, c := range []struct {
		name, content string
		link, folder  bool
		large         bool
		line          int
		cause         string
	}{
		{name: "a line of four", content: entry + "\n" + dispatchB + " " + runB + " explore claimed\n", line: 2, cause: fields},
		{name: "a line of six", content: entry + " -\n", line: 1, cause: fields},
		{name: "no line feed", content: entry, line: 1, cause: fields},
		{name: "two spaces", content: dispatchA + "  " + runA + " explore claimed -\n", line: 1, cause: fields},
		{name: "a space at the end", content: entry + " \n", line: 1, cause: fields},
		{name: "a carriage return", content: entry + "\r\n", line: 1, cause: "the instance id is set before the phase created"},
		{name: "an empty line", content: entry + "\n\n", line: 2, cause: fields},
		{name: "the old four fields", content: dispatchA + " " + runA + " explore started\n", line: 1, cause: fields},
		{name: "an unknown phase", content: dispatchA + " " + runA + " explore running -\n", line: 1, cause: "the phase is not claimed, dispatched, handover, handed_over, created or stopped"},
		{name: "a dispatch id out of shape", content: "dp_0 " + runA + " explore claimed -\n", line: 1, cause: "the dispatch id is not dp_ and 16 Crockford base32 characters"},
		{name: "a run id out of shape", content: dispatchA + " run-a explore claimed -\n", line: 1, cause: "the run id is not a lower-case UUID"},
		{name: "a step out of shape", content: dispatchA + " " + runA + " Explore claimed -\n", line: 1, cause: "the step is not 1 to 40 lower-case letters and underscores that start with a letter"},
		{name: "created with -", content: dispatchA + " " + runA + " explore created -\n", line: 1, cause: "the instance id is not i- and 8 or 17 hex digits"},
		{name: "claimed with an instance id", content: dispatchA + " " + runA + " explore claimed " + instance + "\n", line: 1, cause: "the instance id is set before the phase created"},
		{name: "an instance id out of shape", content: other[:len(other)-1] + "\n", line: 1, cause: "the instance id is not i- and 8 or 17 hex digits"},
		{name: "a dispatch twice", content: entry + "\n" + other + "\n" + dispatchA + " " + runA + " explore dispatched -\n", line: 3},
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
	// line is a line of n bytes, from 69 to 108, for the dispatch k.
	line := func(k, n int) string {
		return fmt.Sprintf("dp_%016d %s %s claimed -\n", k, runA, strings.Repeat("a", n-68))
	}
	var b strings.Builder
	k := 0
	for ; 1<<20-b.Len() > 324; k++ {
		b.WriteString(line(k, 108))
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

// TestAnEntryHasOnlyItsFiveFields pins the fields of an entry: none is for a run key,
// a run code or inputs.
func TestAnEntryHasOnlyItsFiveFields(t *testing.T) {
	typ := reflect.TypeFor[journal.Entry]()
	var got []string
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.String {
			t.Errorf("the field %s is a %s", f.Name, f.Type)
		}
		got = append(got, f.Name)
	}
	if want := []string{"DispatchID", "RunID", "Step", "Phase", "InstanceID"}; !slices.Equal(got, want) {
		t.Errorf("the fields are %q, want %q", got, want)
	}
}

// TestNoSecretReachesTheJournal writes a dispatch through every phase while a run
// key, a run code and inputs with marker values are in hand, tries to write each
// marker in each field, and opens journals that contain the markers. No marker is in
// the journal file, in the folder or in any error.
func TestNoSecretReachesTheJournal(t *testing.T) {
	const (
		runKey = "qrk_MARKERrunkey000000000000"
		// runCode has the shape of a run code: qecr_, 26 Crockford base32 characters
		// and one or two parts of 22 base64url characters.
		runCode = "qecr_MARKER00000000000000000000.MARKERruncode000000000"
		inputs  = `{"MARKERinput":"MARKERvalue"}`
	)
	if !regexp.MustCompile(`^qecr_[0-9A-HJKMNP-TV-Z]{26}(\.[A-Za-z0-9_-]{22}){1,2}$`).MatchString(runCode) {
		t.Fatalf("the run code marker %q is not in the shape of a run code", runCode)
	}
	markers := []string{"MARKER", runKey, runCode, inputs}
	dir := t.TempDir()
	j := open(t, dir)
	var errs []string
	for _, phase := range []string{journal.PhaseClaimed, journal.PhaseDispatched, journal.PhaseHandover, journal.PhaseHandedOver, journal.PhaseCreated, journal.PhaseStopped} {
		e := journal.Entry{DispatchID: dispatchA, RunID: runA, Step: "explore", Phase: phase}
		if phase == journal.PhaseCreated || phase == journal.PhaseStopped {
			e.InstanceID = instance
		}
		if err := j.Set(e); err != nil {
			t.Fatal(err)
		}
		for _, m := range markers[1:] {
			for _, edit := range []func(*journal.Entry){
				func(x *journal.Entry) { x.DispatchID = m },
				func(x *journal.Entry) { x.DispatchID = "dp_" + m },
				func(x *journal.Entry) { x.RunID = m },
				func(x *journal.Entry) { x.Step = m },
				func(x *journal.Entry) { x.Step = "qrk_marker" },
				func(x *journal.Entry) { x.Step = "qecr_marker" },
				func(x *journal.Entry) { x.Step = "qecp_marker" },
				func(x *journal.Entry) { x.Phase = m },
				func(x *journal.Entry) { x.Phase, x.InstanceID = journal.PhaseCreated, m },
				func(x *journal.Entry) { x.Phase, x.InstanceID = journal.PhaseCreated, "i-"+m },
			} {
				bad := e
				edit(&bad)
				err := j.Set(bad)
				if err == nil {
					t.Fatalf("an entry with a marker was written: %v", bad)
				}
				errs = append(errs, err.Error())
			}
		}
	}
	if err := j.Remove(dispatchA); err != nil {
		t.Fatal(err)
	}
	// Open returns an error for a journal that contains a marker on a line, and the
	// error quotes no line.
	for _, m := range markers[1:] {
		for _, line := range []string{
			dispatchA + " " + runA + " explore claimed " + m + "\n",
			dispatchA + " " + runA + " " + m + " claimed -\n",
			dispatchA + " " + m + " explore claimed -\n",
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
