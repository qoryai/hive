package enrol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qoryai/forager/accesskey"
	"github.com/qoryai/hive/internal/state"
)

// writeCodeFile writes content to the file code, mode 0600, in a new folder, and
// returns its path.
func writeCodeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "provisioner", "code")
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// withCodeFile sets the Config's code file to path, and its Sleep to fail the test:
// a start with a file that is there never reads it again.
func withCodeFile(t *testing.T, path string) func(*Config) {
	return func(c *Config) {
		c.CodeFile = path
		c.Sleep = func(context.Context, time.Duration) error {
			t.Error("Start read the code file again")
			return errors.New("the test stops here")
		}
	}
}

// fakeNames are the names of a system where uid 1000 is tester, with the primary group
// 1000 named tester, gid 0 is root, 10 wheel, 80 admin and 20 staff, and other ids have
// no name.
var fakeNames = names{
	User: func(uid uint32) string {
		if uid == 1000 {
			return "tester"
		}
		return ""
	},
	Group: func(gid uint32) string {
		return map[uint32]string{0: "root", 10: "wheel", 80: "admin", 20: "staff", 1000: "tester", 1001: "tester"}[gid]
	},
	Primary: func(uid uint32) (uint32, bool) { return 1000, uid == 1000 },
}

// TestTrustedIsTheOwnerRule pins the rule for the code file and each folder above it,
// for the user 1000: the owner is root or that user, others may not write it but in a
// sticky folder of root's, and a group may write it only when it is gid 0, wheel, admin
// or the owner's primary group with the owner's name. Each error starts with the fix
// and contains the path and the owner's uid.
func TestTrustedIsTheOwnerRule(t *testing.T) {
	const path = "/run/qory-enrolment/provisioner/code"
	for _, tc := range []struct {
		what string
		o    fileOwner
		want string
	}{
		{"", fileOwner{UID: 1000, GID: 20, Mode: 0o600}, ""},
		{"", fileOwner{UID: 0, GID: 0, Mode: 0o644}, ""},
		{"", fileOwner{UID: 0, GID: 0, Mode: os.ModeDir | os.ModeSticky | 0o777}, ""},
		{"", fileOwner{UID: 1000, GID: 0, Mode: 0o660}, ""},
		{"", fileOwner{UID: 1000, GID: 10, Mode: 0o660}, ""},
		{"", fileOwner{UID: 1000, GID: 80, Mode: 0o660}, ""},
		{"", fileOwner{UID: 1000, GID: 1000, Mode: 0o660}, ""},
		{"the link ", fileOwner{UID: 1000, GID: 20, Mode: os.ModeSymlink | 0o777}, ""},
		{"", fileOwner{UID: 1001, GID: 20, Mode: 0o600},
			"Make root or uid 1000 the owner of " + path + ", then start the provisioner again: uid 1001 owns it, so a user other than root and the provisioner could change the enrolment code."},
		{"", fileOwner{UID: 65534, GID: 65534, Mode: os.ModeDir | 0o755},
			"Make root or uid 1000 the owner of " + path + ", then start the provisioner again: uid 65534 owns it, so a user other than root and the provisioner could change the enrolment code."},
		{"the link ", fileOwner{UID: 1001, GID: 20, Mode: os.ModeSymlink | 0o777},
			"Make root or uid 1000 the owner of the link " + path + ", then start the provisioner again: uid 1001 owns it, so a user other than root and the provisioner could change the enrolment code."},
		{"", fileOwner{UID: 1000, GID: 20, Mode: 0o602},
			"Remove the write permission of other users from " + path + ", then start the provisioner again: every user may write it (-rw-----w-, owner uid 1000)."},
		{"", fileOwner{UID: 1000, GID: 20, Mode: os.ModeDir | os.ModeSticky | 0o777},
			"Remove the write permission of other users from " + path + ", then start the provisioner again: every user may write it (dtrwxrwxrwx, owner uid 1000)."},
		{"", fileOwner{UID: 0, GID: 0, Mode: os.ModeDir | 0o777},
			"Remove the write permission of other users from " + path + ", then start the provisioner again: every user may write it (drwxrwxrwx, owner uid 0)."},
		{"", fileOwner{UID: 1000, GID: 20, Mode: 0o620},
			"Remove the group's write permission from " + path + ", then start the provisioner again: the group staff (gid 20) may write it (-rw--w----, owner uid 1000). A group may write it only when it is gid 0, wheel, admin, or the owner's primary group with the owner's name."},
		{"", fileOwner{UID: 1000, GID: 1001, Mode: 0o620},
			"Remove the group's write permission from " + path + ", then start the provisioner again: the group tester (gid 1001) may write it (-rw--w----, owner uid 1000). A group may write it only when it is gid 0, wheel, admin, or the owner's primary group with the owner's name."},
		{"", fileOwner{UID: 0, GID: 1234, Mode: os.ModeDir | 0o775},
			"Remove the group's write permission from " + path + ", then start the provisioner again: the group gid 1234 may write it (drwxrwxr-x, owner uid 0). A group may write it only when it is gid 0, wheel, admin, or the owner's primary group with the owner's name."},
	} {
		err := trusted(path, tc.what, tc.o, 1000, fakeNames)
		if got := errText(err); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.o, got, tc.want)
		}
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestStartEnrolsFromTheCodeFile pins the first start from the code file: it reads the
// code without its newline, enrols, and logs only the enrolment. A second start with the
// file still set logs one line that the file is not used.
func TestStartEnrolsFromTheCodeFile(t *testing.T) {
	f := newFakeApiary(t, poolEnrolled)
	dir := stateDir(t)
	code := accesskey.CodePrefixPool + codeCore + "." + f.signer.Fingerprint()
	path := writeCodeFile(t, code+"\n")
	r := startWith(t, context.Background(), f, dir, "", withCodeFile(t, path))
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got := f.got.Load(); f.requests.Load() != 1 || got == nil || got.PublicKey != r.made.PublicKey().String() {
		t.Fatalf("%d requests: %+v", f.requests.Load(), got)
	}
	if !strings.Contains(r.log, `"msg":"enrolled the access key"`) || strings.Count(r.log, "\n") != 1 {
		t.Errorf("the log: %s", r.log)
	}
	noLeak(t, r, dir, code)

	again := startWith(t, context.Background(), f, dir, "", withCodeFile(t, path))
	var line map[string]any
	if again.err != nil || again.made != nil || f.requests.Load() != 1 || json.Unmarshal([]byte(again.log), &line) != nil || strings.Count(again.log, "\n") != 1 ||
		line["msg"] != "the state folder has an access key, so the enrolment code file is not used" || line["path"] != path || line["state_dir"] != dir || line["level"] != "INFO" {
		t.Errorf("the second start: %v, %d requests, log %s", again.err, f.requests.Load(), again.log)
	}
	noLeak(t, again, dir, code)
}

// TestStartReadsAMissingCodeFileAgain pins that a code file that is missing, also with
// its folder, is read again after 10 seconds, with one log line for all the reads, and
// that the code read then enrols.
func TestStartReadsAMissingCodeFileAgain(t *testing.T) {
	f := newFakeApiary(t, poolEnrolled)
	dir := stateDir(t)
	code := poolCode(f)
	path := filepath.Join(t.TempDir(), "provisioner", "code")
	sleeps := 0
	r := startWith(t, context.Background(), f, dir, "", func(c *Config) {
		c.CodeFile = path
		c.Sleep = func(_ context.Context, d time.Duration) error {
			sleeps++
			if d != 10*time.Second {
				t.Errorf("Start slept %v", d)
			}
			switch sleeps {
			case 1:
				if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
			case 3:
				if err := os.WriteFile(path, []byte(code+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		}
	})
	if r.err != nil || sleeps != 3 || f.requests.Load() != 1 {
		t.Fatalf("%v, %d sleeps, %d requests", r.err, sleeps, f.requests.Load())
	}
	lines := strings.Split(strings.TrimSpace(r.log), "\n")
	var line map[string]any
	if len(lines) != 2 || json.Unmarshal([]byte(lines[0]), &line) != nil ||
		line["msg"] != "the enrolment code file is missing: the provisioner reads it again every 10 seconds until Qory Apiary writes it" || line["path"] != path || line["level"] != "INFO" ||
		!strings.Contains(lines[1], `"msg":"enrolled the access key"`) {
		t.Errorf("the log: %s", r.log)
	}
	noLeak(t, r, dir, code)
}

// TestStartStopsReadingTheCodeFileWhenTheContextEnds pins that the reads of a missing
// code file end with the context, with an error that wraps the context's.
func TestStartStopsReadingTheCodeFileWhenTheContextEnds(t *testing.T) {
	f := newFakeApiary(t, poolEnrolled)
	dir := stateDir(t)
	path := filepath.Join(t.TempDir(), "code")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := startWith(t, ctx, f, dir, "", func(c *Config) { c.CodeFile = path })
	want := "Start the provisioner again: it stopped before Qory Apiary wrote the enrolment code file " + path + ": context canceled"
	if r.err == nil || r.err.Error() != want || !errors.Is(r.err, context.Canceled) || r.made != nil || f.requests.Load() != 0 {
		t.Errorf("%v", r.err)
	}
	noKey(t, dir)
}

// TestStartRefusesTheCodeFileAndTheVariableTogether pins that a start with both
// enrolment_code_file and QORY_ENROLMENT_CODE set is refused before anything is read
// or sent, with and without a key in the state folder.
func TestStartRefusesTheCodeFileAndTheVariableTogether(t *testing.T) {
	f := newFakeApiary(t, poolEnrolled)
	dir := stateDir(t)
	code := poolCode(f)
	path := writeCodeFile(t, code+"\n")
	want := "Remove QORY_ENROLMENT_CODE from the environment or enrolment_code_file from the settings file, then start the provisioner again: the provisioner reads the enrolment code from one of them, and both are set."
	r := startWith(t, context.Background(), f, dir, code, withCodeFile(t, path))
	if r.err == nil || r.err.Error() != want || r.made != nil || f.requests.Load() != 0 {
		t.Errorf("%v", r.err)
	}
	noKey(t, dir)
	noLeak(t, r, dir, code)

	if r := start(t, f, dir, code); r.err != nil {
		t.Fatal(r.err)
	}
	r = startWith(t, context.Background(), f, dir, code, withCodeFile(t, path))
	if r.err == nil || r.err.Error() != want || f.requests.Load() != 1 {
		t.Errorf("with a key: %v", r.err)
	}
	noLeak(t, r, dir, code)
}

// TestStartRefusesACodeOfAnotherKindInTheCodeFile pins that a code file with a code for
// another kind of node, or with no code, is refused before a key is made or anything is
// sent, with an error that quotes nothing of the file.
func TestStartRefusesACodeOfAnotherKindInTheCodeFile(t *testing.T) {
	f := newFakeApiary(t, poolEnrolled)
	for _, tc := range []struct{ code, want string }{
		{codeOf(accesskey.CodePrefixNode, f.signer), "the code in %s is for a node."},
		{codeOf(accesskey.CodePrefixGateway, f.signer), "the code in %s is for a gateway."},
		{codeOf(accesskey.CodePrefixAdapter, f.signer), "the code in %s is for an integration."},
		{"qecp_" + codeCore, "%s contains no code of the form qecp_, 26 characters, a dot and a fingerprint."},
		{"", "%s contains no code of the form qecp_, 26 characters, a dot and a fingerprint."},
	} {
		dir := stateDir(t)
		path := writeCodeFile(t, tc.code+"\n")
		r := startWith(t, context.Background(), f, dir, "", withCodeFile(t, path))
		want := "Set enrolment_code_file in the settings file to the file in which Qory Apiary writes a node pool's enrolment code: " + strings.Replace(tc.want, "%s", path, 1)
		if r.err == nil || r.err.Error() != want || r.made != nil {
			t.Errorf("%v, want %q", r.err, want)
		}
		noKey(t, dir)
		noLeak(t, r, dir, tc.code)
	}
	if n := f.requests.Load(); n != 0 {
		t.Errorf("%d requests were sent", n)
	}
}

// TestStartSaysTheCodeFileInAnError pins that an enrolment from the code file that
// fails, or whose state cannot be written, says to start again for the code that Qory
// Apiary writes, never to set QORY_ENROLMENT_CODE.
func TestStartSaysTheCodeFileInAnError(t *testing.T) {
	for _, tc := range []struct {
		rp   reply
		want string
	}{
		{reply{status: http.StatusUnauthorized}, ": the server at http://127.0.0.1:"},
		{reply{status: http.StatusCreated, kind: "pool", nodeID: poolID, unsigned: true}, ": the response to the enrolment did not verify (HTTP 201), so no secret was written. Have a workspace admin of Qory Apiary revoke the key "},
		{reply{status: http.StatusConflict, refusal: "other"}, ": the enrolment did not complete: "},
		{reply{status: http.StatusCreated, kind: "gateway", nodeID: "ng_0123456789abcdef"}, ": Qory Apiary enrolled the key as ak_0123456789abcdef for the gateway ng_0123456789abcdef, not a node pool, so no secret was written. Have a workspace admin of Qory Apiary revoke the key "},
	} {
		f := newFakeApiary(t, tc.rp)
		dir := stateDir(t)
		code := poolCode(f)
		path := writeCodeFile(t, code+"\n")
		r := startWith(t, context.Background(), f, dir, "", withCodeFile(t, path))
		want := "Start the provisioner again, which reads the code that Qory Apiary writes to " + path + tc.want
		if r.err == nil || !strings.HasPrefix(r.err.Error(), want) || f.requests.Load() != 1 {
			t.Errorf("%d %s: %v, want %q", tc.rp.status, tc.rp.refusal, r.err, want)
		}
		noKey(t, dir)
		noLeak(t, r, dir, code)
	}

	f := newFakeApiary(t, poolEnrolled)
	dir := stateDir(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, state.EnrolmentFile), 0o700); err != nil {
		t.Fatal(err)
	}
	code := poolCode(f)
	path := writeCodeFile(t, code+"\n")
	r := startWith(t, context.Background(), f, dir, "", withCodeFile(t, path))
	want := "Fix the state folder, have a workspace admin of Qory Apiary revoke the key ak_0123456789abcdef, and start the provisioner again, which reads the code that Qory Apiary writes to " + path + ": the key is enrolled, and the state folder has no key file for it: "
	if r.err == nil || !strings.HasPrefix(r.err.Error(), want) {
		t.Errorf("%v", r.err)
	}
	noKey(t, dir)
	noLeak(t, r, dir, code)
}

// TestStartRefusesACodeFileOthersMayWrite pins that a code file, or a folder above it,
// that every user may write is refused before the code is read or anything is sent.
func TestStartRefusesACodeFileOthersMayWrite(t *testing.T) {
	f := newFakeApiary(t, poolEnrolled)
	code := poolCode(f)
	path := writeCodeFile(t, code+"\n")
	for _, tc := range []struct {
		chmod string
		mode  os.FileMode
		shown string
	}{
		{path, 0o606, "-rw----rw-"},
		{filepath.Dir(path), 0o707, "drwx---rwx"},
	} {
		if err := os.Chmod(tc.chmod, tc.mode); err != nil {
			t.Fatal(err)
		}
		dir := stateDir(t)
		r := startWith(t, context.Background(), f, dir, "", withCodeFile(t, path))
		resolved, err := filepath.EvalSymlinks(tc.chmod)
		if err != nil {
			t.Fatal(err)
		}
		want := "Remove the write permission of other users from " + resolved + ", then start the provisioner again: every user may write it (" + tc.shown + ", owner uid " + strconv.Itoa(os.Geteuid()) + ")."
		if r.err == nil || r.err.Error() != want || r.made != nil {
			t.Errorf("%v, want %q", r.err, want)
		}
		noKey(t, dir)
		noLeak(t, r, dir, code)
		if err := os.Chmod(tc.chmod, tc.mode&^0o007); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.requests.Load(); n != 0 {
		t.Errorf("%d requests were sent", n)
	}
}

// TestReadCodeFileChecksTheOwnerAndTheGroup pins, on files, that a code file in a
// folder another user owns is refused with the folder's owner, and that a group may write the file or its folder
// only as the names allow.
func TestReadCodeFileChecksTheOwnerAndTheGroup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns every file the test makes, and root is a trusted owner")
	}
	euid := uint32(os.Geteuid())
	path := writeCodeFile(t, "marker\n")
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = readCodeFile(path, euid+1, systemNames)
	want := "Make root or uid " + strconv.Itoa(int(euid)+1) + " the owner of " + filepath.Dir(resolved) + ", then start the provisioner again: uid " + strconv.Itoa(int(euid)) + " owns it, so a user other than root and the provisioner could change the enrolment code."
	if err == nil || err.Error() != want {
		t.Errorf("%v, want %q", err, want)
	}

	gid := uint32(os.Getegid())
	group := func(name string, primary bool) names {
		return names{
			User:    func(uint32) string { return "tester" },
			Group:   func(uint32) string { return name },
			Primary: func(uint32) (uint32, bool) { return gid, primary },
		}
	}
	for _, chmod := range []string{path, filepath.Dir(path)} {
		info, err := os.Stat(chmod)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(chmod, info.Mode().Perm()|0o020); err != nil {
			t.Fatal(err)
		}
		if got, err := readCodeFile(path, euid, group("tester", true)); err != nil || got != "marker" {
			t.Errorf("the owner's primary group with the owner's name: %q %v", got, err)
		}
		r, err := filepath.EvalSymlinks(chmod)
		if err != nil {
			t.Fatal(err)
		}
		_, err = readCodeFile(path, euid, group("staff", true))
		prefix := "Remove the group's write permission from " + r + ", then start the provisioner again: the group staff (gid " + strconv.Itoa(int(gid)) + ") may write it ("
		if err == nil || !strings.HasPrefix(err.Error(), prefix) {
			t.Errorf("%v, want %q", err, prefix)
		}
		if _, err := readCodeFile(path, euid, group("tester", false)); err == nil {
			t.Error("a group with the owner's name that is not the owner's primary group was taken")
		}
		if err := os.Chmod(chmod, info.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
}

// TestReadCodeFileChecksLinks pins that a link to a code file is followed, and that a
// link in a folder that every user may write is refused, as is a link to a file that
// every user may write.
func TestReadCodeFileChecksLinks(t *testing.T) {
	euid := uint32(os.Geteuid())
	path := writeCodeFile(t, "marker\n")
	open := t.TempDir()
	link := filepath.Join(open, "code")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if got, err := readCodeFile(link, euid, systemNames); err != nil || got != "marker" {
		t.Errorf("%q %v", got, err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	resolvedOpen, err := filepath.EvalSymlinks(open)
	if err != nil {
		t.Fatal(err)
	}
	_, err = readCodeFile(link, euid, systemNames)
	if err == nil || !strings.HasPrefix(err.Error(), "Remove the write permission of other users from "+resolvedOpen+",") {
		t.Errorf("%v", err)
	}
	if err := os.Chmod(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o606); err != nil {
		t.Fatal(err)
	}
	if _, err := readCodeFile(link, euid, systemNames); err == nil || !strings.HasPrefix(err.Error(), "Remove the write permission of other users from ") {
		t.Errorf("%v", err)
	}
}

// TestReadCodeFileRefusesWhatIsNoRegularFile pins that a folder or a named pipe at the
// code file's path is refused, and that the open of the pipe does not block.
func TestReadCodeFileRefusesWhatIsNoRegularFile(t *testing.T) {
	euid := uint32(os.Geteuid())
	folder := filepath.Join(t.TempDir(), "code")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	pipe := filepath.Join(t.TempDir(), "code")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]string{folder: "drwx------", pipe: "prw-------"} {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = readCodeFile(path, euid, systemNames)
		want := "Set enrolment_code_file in the settings file to a regular file: " + resolved + " is not one (" + mode + ")."
		if err == nil || err.Error() != want {
			t.Errorf("%v, want %q", err, want)
		}
	}
}

// TestReadCodeFileBoundsTheRead pins that the code file is read up to the longest code
// and a newline, that a longer file is refused without its content, and that one
// trailing newline is trimmed.
func TestReadCodeFileBoundsTheRead(t *testing.T) {
	euid := uint32(os.Geteuid())
	if maxCodeFile != 78 {
		t.Errorf("maxCodeFile is %d", maxCodeFile)
	}
	longest := "qecp_" + codeCore + "." + strings.Repeat("A", 22) + "." + strings.Repeat("B", 22)
	for content, want := range map[string]string{
		longest + "\n": longest,
		longest + "C":  longest + "C",
		"marker\n\n":   "marker\n",
		"marker\r\n":   "marker\r",
		"marker":       "marker",
	} {
		path := writeCodeFile(t, content)
		if got, err := readCodeFile(path, euid, systemNames); err != nil || got != want {
			t.Errorf("%q: %q %v", content, got, err)
		}
	}
	for _, content := range []string{longest + "\n\n", longest + "CC", strings.Repeat("qecp_"+codeCore, 1<<15)} {
		path := writeCodeFile(t, content)
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = readCodeFile(path, euid, systemNames)
		want := "Set enrolment_code_file in the settings file to the file in which Qory Apiary writes a node pool's enrolment code: " + resolved + " is longer than 78 bytes, the most that one code and a newline take."
		if err == nil || err.Error() != want {
			t.Errorf("%d bytes: %v", len(content), err)
		}
	}
}

// TestReadCodeFileChecksEveryLinkOnTheWay pins that a link to a link is followed one
// link at a time: the folder of the second link is checked too, so a folder that every
// user may write, which holds the second link, is refused. A relative target is read
// from the link's folder, and a loop of links is refused.
func TestReadCodeFileChecksEveryLinkOnTheWay(t *testing.T) {
	euid := uint32(os.Geteuid())
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(base, "real")
	open := filepath.Join(base, "open")
	for _, d := range []string{realDir, open} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(realDir, "code"), []byte("marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../real", filepath.Join(open, "l2")); err != nil {
		t.Fatal(err)
	}
	l1 := filepath.Join(base, "l1")
	if err := os.Symlink(filepath.Join(open, "l2"), l1); err != nil {
		t.Fatal(err)
	}
	if got, err := readCodeFile(filepath.Join(l1, "code"), euid, systemNames); err != nil || got != "marker" {
		t.Errorf("%q %v", got, err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	_, err = readCodeFile(filepath.Join(l1, "code"), euid, systemNames)
	want := "Remove the write permission of other users from " + open + ", then start the provisioner again: every user may write it (drwxrwxrwx, owner uid " + strconv.Itoa(int(euid)) + ")."
	if err == nil || err.Error() != want {
		t.Errorf("%v, want %q", err, want)
	}

	loop := filepath.Join(base, "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Fatal(err)
	}
	_, err = readCodeFile(loop, euid, systemNames)
	want = "Set enrolment_code_file in the settings file to a path that leads through at most 40 links: " + loop + " leads through more."
	if err == nil || err.Error() != want {
		t.Errorf("%v, want %q", err, want)
	}
	if _, err := readCodeFile(filepath.Join(base, "dangling"), euid, systemNames); !errors.Is(err, errMissing) {
		t.Errorf("a missing file: %v", err)
	}
	if err := os.Symlink(filepath.Join(base, "none"), filepath.Join(realDir, "gone")); err != nil {
		t.Fatal(err)
	}
	if _, err := readCodeFile(filepath.Join(realDir, "gone"), euid, systemNames); !errors.Is(err, errMissing) {
		t.Errorf("a link to a missing file: %v", err)
	}
}
