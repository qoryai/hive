// Package journal is the provisioner's journal of live dispatches: the file
// <state folder>/dispatches, with one line per live dispatch, and [Recover], which
// returns what to do with each line after a restart.
//
// An entry has four fields, a dispatch id, a phase, a machine id and the machine's
// lifetime in minutes, and no field for a secret or for inputs. Each field is checked
// against a fixed shape before it is written and after it is read. A machine id that contains qak_, qrk_, qecr_ or qecp_,
// the start of a secret, is out of shape.
package journal

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// File is the journal's name in the state folder.
const File = "dispatches"

// maxSize is the most bytes the journal may contain: far more than the lines of the
// machines one provisioner runs at once.
const maxSize = 1 << 20

// noMachine is the machine id field of a line whose dispatch has no machine id.
const noMachine = "-"

// The bounds of a machine's lifetime in minutes, as the provisioner contract states
// max_lifetime_minutes of a claim answer.
const (
	// MinLifetime is the shortest lifetime of a machine, in minutes.
	MinLifetime = 15
	// MaxLifetime is the longest lifetime of a machine, in minutes.
	MaxLifetime = 1470
)

// The phases of a dispatch, in order. In claimed, no make has returned, so a machine
// may exist without a machine id in the journal: a restart looks the machine up in
// the adapter's list by its key, the dispatch id. From made on, the entry has the
// machine id that the adapter's make returned.
const (
	// PhaseClaimed is a claimed dispatch for which no make has returned.
	PhaseClaimed = "claimed"
	// PhaseMade is a dispatch whose make returned a machine id, before dispatched
	// is posted.
	PhaseMade = "made"
	// PhaseDispatched is a dispatch for which dispatched is posted.
	PhaseDispatched = "dispatched"
)

// secretPrefixes are the starts of an access key secret, a run key, a run code and an
// enrolment code. A machine id that contains one is out of shape.
var secretPrefixes = []string{"qak_", "qrk_", "qecr_", "qecp_"}

// phases is every phase, in order.
var phases = []string{PhaseClaimed, PhaseMade, PhaseDispatched}

var (
	// dispatchIDShape is dp_ and 16 lower-case Crockford base32 characters.
	dispatchIDShape = regexp.MustCompile(`^dp_[0-9a-hjkmnp-tv-z]{16}$`)
	// machineIDShape is the machine id of the adapter protocol: 1 to 128 printable
	// ASCII characters other than a space.
	machineIDShape = regexp.MustCompile(`^[!-~]{1,128}$`)
	// lifetimeShape is a whole number of 1 to 4 digits with no leading zero.
	lifetimeShape = regexp.MustCompile(`^[1-9][0-9]{0,3}$`)
)

// Entry is one live dispatch. MachineID is empty in [PhaseClaimed] and is the machine
// id that the adapter's make returned from [PhaseMade] on. The journal treats a
// machine id as opaque. MaxLifetimeMinutes is the machine's lifetime from the claim
// answer, [MinLifetime] to [MaxLifetime], in every phase. An entry has no field for a
// secret or for inputs.
type Entry struct {
	DispatchID, Phase, MachineID string
	MaxLifetimeMinutes           int
}

// line is e as the journal writes it: the four fields, with - for no machine id.
func (e Entry) line() string {
	machine := e.MachineID
	if machine == "" {
		machine = noMachine
	}
	return e.DispatchID + " " + e.Phase + " " + machine + " " + strconv.Itoa(e.MaxLifetimeMinutes) + "\n"
}

// check returns the first field of e that is out of shape, as a phrase that quotes no
// value, or "" when every field has its shape.
func (e Entry) check() string {
	switch {
	case !dispatchIDShape.MatchString(e.DispatchID):
		return "the dispatch id is not dp_ and 16 Crockford base32 characters"
	case !slices.Contains(phases, e.Phase):
		return "the phase is not claimed, made or dispatched"
	case slices.ContainsFunc(secretPrefixes, func(p string) bool { return strings.Contains(e.MachineID, p) }):
		return "the machine id contains qak_, qrk_, qecr_ or qecp_, the start of a secret"
	case e.MaxLifetimeMinutes < MinLifetime || e.MaxLifetimeMinutes > MaxLifetime:
		return "the lifetime is not 15 to 1470 minutes"
	case e.Phase == PhaseClaimed && e.MachineID != "":
		return "the machine id is set in the phase claimed"
	case e.Phase == PhaseClaimed:
		return ""
	case e.MachineID == "":
		return "the phase made or dispatched has no machine id"
	case !machineIDShape.MatchString(e.MachineID):
		return "the machine id is not 1 to 128 printable ASCII characters other than a space"
	case e.MachineID == noMachine:
		return "the machine id is -, which the journal writes for no machine id"
	}
	return ""
}

// Journal is the file <state folder>/dispatches, mode 0600, of at most 1 MiB: one line
// per live dispatch, "dispatch_id phase machine_id max_lifetime_minutes", in the order
// of their claims. Each change writes the whole file to a new file, synced, renames it
// over the journal and syncs the folder, so a crash leaves the journal before the
// change or after it. A Journal is safe for use from several goroutines.
type Journal struct {
	path    string
	mu      sync.Mutex
	entries []Entry
}

// Open reads the journal in the state folder dir. A journal that does not exist is
// empty. Open returns an error for a journal that is not a regular file, is larger
// than 1 MiB, or has a line out of shape or a dispatch on two lines. No error quotes a
// line.
func Open(dir string) (*Journal, error) {
	j := &Journal{path: filepath.Join(dir, File)}
	info, err := os.Lstat(j.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return j, nil
	case err != nil:
		return nil, fmt.Errorf("check the state folder: reading the journal of live dispatches failed: %w", err)
	case !info.Mode().IsRegular():
		return nil, fmt.Errorf("repair or remove %s: the journal of live dispatches is not a regular file", j.path)
	case info.Size() > maxSize:
		return nil, fmt.Errorf("repair or remove %s: the journal of live dispatches is larger than 1 MiB", j.path)
	}
	f, err := os.Open(j.path)
	if err != nil {
		return nil, fmt.Errorf("check the state folder: reading the journal of live dispatches failed: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("check the state folder: reading the journal of live dispatches failed: %w", err)
	}
	if len(b) > maxSize {
		return nil, fmt.Errorf("repair or remove %s: the journal of live dispatches is larger than 1 MiB", j.path)
	}
	for n := 1; len(b) > 0; n++ {
		e, cause := parse(&b)
		if cause == "" {
			cause = e.check()
		}
		if cause != "" {
			return nil, fmt.Errorf("repair or remove %s: line %d of the journal of live dispatches is out of shape: %s", j.path, n, cause)
		}
		if i := slices.IndexFunc(j.entries, func(x Entry) bool { return x.DispatchID == e.DispatchID }); i >= 0 {
			return nil, fmt.Errorf("repair or remove %s: line %d of the journal of live dispatches repeats the dispatch of line %d", j.path, n, i+1)
		}
		j.entries = append(j.entries, e)
	}
	return j, nil
}

// parse takes the first line off b and splits it into an entry. The cause is not ""
// when the line has no line feed, is not four fields that are not empty, separated by
// single spaces, or has a lifetime that is not a whole number.
func parse(b *[]byte) (Entry, string) {
	const cause = "the line is not four fields separated by single spaces and ended by a line feed"
	i := bytes.IndexByte(*b, '\n')
	if i < 0 {
		*b = nil
		return Entry{}, cause
	}
	fields := strings.Split(string((*b)[:i]), " ")
	*b = (*b)[i+1:]
	if len(fields) != 4 || slices.Contains(fields, "") {
		return Entry{}, cause
	}
	e := Entry{DispatchID: fields[0], Phase: fields[1], MachineID: fields[2]}
	if e.MachineID == noMachine {
		e.MachineID = ""
	}
	if !lifetimeShape.MatchString(fields[3]) {
		return Entry{}, "the lifetime is not a whole number of minutes"
	}
	e.MaxLifetimeMinutes, _ = strconv.Atoi(fields[3])
	return e, ""
}

// List returns the journal's entries, in the order of their claims.
func (j *Journal) List() []Entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.entries)
}

// Set writes e into the journal, in place of the entry of its dispatch, or after the
// others. On an error before the rename, the journal is as it was.
func (j *Journal) Set(e Entry) error {
	if cause := e.check(); cause != "" {
		return fmt.Errorf("fix the journal entry: %s; the journal of live dispatches is as it was", cause)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	next := slices.Clone(j.entries)
	if i := slices.IndexFunc(next, func(x Entry) bool { return x.DispatchID == e.DispatchID }); i >= 0 {
		next[i] = e
	} else {
		next = append(next, e)
	}
	return j.write(next)
}

// Remove takes the entry of the dispatch id out of the journal. A dispatch id that
// has no entry writes nothing.
func (j *Journal) Remove(dispatchID string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	next := slices.DeleteFunc(slices.Clone(j.entries), func(x Entry) bool { return x.DispatchID == dispatchID })
	if len(next) == len(j.entries) {
		return nil
	}
	return j.write(next)
}

// write writes entries to <journal>.new with O_EXCL, mode 0600, syncs it, renames it
// over the journal and syncs the folder. Once the rename is done, j holds entries,
// even when the folder's sync fails.
func (j *Journal) write(entries []Entry) error {
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.line())
	}
	if b.Len() > maxSize {
		return errors.New("remove a dispatch from the journal first: the journal of live dispatches would be larger than 1 MiB; the journal is as it was")
	}
	tmp := j.path + ".new"
	err := writeSynced(tmp, b.String())
	if err == nil {
		if err = os.Rename(tmp, j.path); err != nil {
			_ = os.Remove(tmp)
		}
	}
	if err != nil {
		return fmt.Errorf("check the state folder: writing the journal of live dispatches failed; the journal is as it was: %w", err)
	}
	j.entries = entries
	dir, err := os.Open(filepath.Dir(j.path))
	if err == nil {
		err = dir.Sync()
		if cerr := dir.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		return fmt.Errorf("check the state folder: the journal of live dispatches is written, but syncing its folder failed: %w", err)
	}
	return nil
}

// writeSynced writes s to the new file path, mode 0600, and syncs it. A file left at
// path by an earlier write is removed first. On an error after the file is created,
// the file is removed.
func writeSynced(path, s string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(s)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}
