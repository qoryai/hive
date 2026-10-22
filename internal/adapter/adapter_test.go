package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/qoryai/hive/machines"
)

const (
	testKey  = "dp_0123456789abcdef"
	testPool = "np_0123456789abcdef"
	// marker stands for the run code. No error, argument, stderr line or test output
	// may contain it.
	marker = "qecr_MARKERc0ffee7a1b"
)

// startData is start data that contains the marker.
var startData = []byte(`{"version":1,"run_code":"` + marker + `","gateway":{"url":"https://gateway.example.com:8443","address":"10.0.1.5"}}`)

// leaks is whether s contains the marker or the start data, raw or as base64.
func leaks(s string) bool {
	return strings.Contains(s, "MARKER") || strings.Contains(s, base64.StdEncoding.EncodeToString(startData))
}

// buffer is a bytes.Buffer safe for concurrent use.
type buffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// fixture is a runner of the fake adapter with cfg, and the adapter's stderr.
type fixture struct {
	r      *Runner
	stderr *buffer
	cfg    config
}

func newFixture(t *testing.T, cfg config) *fixture {
	t.Helper()
	dir := t.TempDir()
	cfg.Report = filepath.Join(dir, "report.json")
	cfg.PIDFile = filepath.Join(dir, "child.pid")
	sum := sha256.Sum256(startData)
	cfg.StartDataSHA256 = hex.EncodeToString(sum[:])
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(dir, "adapter.json")
	if err := os.WriteFile(settings, b, 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &buffer{}
	return &fixture{r: New(exe, settings, stderr), stderr: stderr, cfg: cfg}
}

// report reads what the fake adapter received.
func (f *fixture) report(t *testing.T) report {
	t.Helper()
	b, err := os.ReadFile(f.cfg.Report)
	if err != nil {
		t.Fatalf("the fake adapter wrote no report: %v", err)
	}
	var r report
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// gone waits until the process in the fake's pid file no longer exists.
func (f *fixture) gone(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile(f.cfg.PIDFile)
	if err != nil {
		t.Fatalf("the fake adapter's child wrote no pid: %v", err)
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatal(err)
	}
	for range 500 {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("the adapter's child process %d still runs", pid)
}

// defect checks that err is a defect of the command name whose message starts with
// prefix, and that err contains no secret.
func defect(t *testing.T, err error, name, prefix string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("the error is %v, want an *Error", err)
	}
	if leaks(err.Error()) {
		t.Fatal("the error contains the marker or the start data")
	}
	if e.Kind != machines.KindDefect || e.Command != name || !strings.HasPrefix(e.Message, prefix) {
		t.Errorf("the error is %q, kind %s, command %s; want a defect of %s starting with %q", e.Message, e.Kind, e.Command, name, prefix)
	}
}

func TestTimeouts(t *testing.T) {
	r := New("adapter", "adapter.json", os.Stderr)
	if r.timeout("make") != 2*time.Minute || MakeTimeout != 2*time.Minute {
		t.Errorf("make's time limit is %s", r.timeout("make"))
	}
	for _, name := range []string{"check", "list", "log", "delete"} {
		if r.timeout(name) != time.Minute {
			t.Errorf("%s's time limit is %s", name, r.timeout(name))
		}
	}
}

func TestCommands(t *testing.T) {
	t.Setenv("QORY_ENROLMENT_CODE", "qecp_"+marker)
	t.Setenv("QORY_OTHER", "1")
	t.Setenv("ADAPTER_TEST_KEEP", "1")
	ctx := context.Background()
	f := newFixture(t, config{Mode: "serve"})
	settings := f.r.settings

	check := func(name string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		rep := f.report(t)
		if !slices.Equal(rep.Args, []string{name, "--settings", settings}) {
			if leaks(strings.Join(rep.Args, " ")) {
				t.Fatalf("%s: an argument contains the marker or the start data", name)
			}
			t.Errorf("%s: the arguments are %q", name, rep.Args)
		}
		if len(rep.QoryEnv) != 0 || !rep.KeptEnv {
			t.Errorf("%s: the adapter's environment has %v of QORY_, and the other variable kept: %v", name, rep.QoryEnv, rep.KeptEnv)
		}
	}

	check("check", f.r.Check(ctx))

	made, err := f.r.Make(ctx, testKey, testPool, "standard", startData)
	check("make", err)
	if made.ID != "i-00000000000000001" || !f.report(t).StartDataMatch {
		t.Errorf("make: the answer is %+v, the start data arrived intact: %v", made, f.report(t).StartDataMatch)
	}

	list, err := f.r.List(ctx, testPool)
	check("list", err)
	if len(list.Machines) != 1 || list.Machines[0].Key != testKey || list.Machines[0].State != machines.StateRunning {
		t.Errorf("list: %+v", list)
	}

	log, err := f.r.Log(ctx, "i-0123456789abcdef0")
	check("log", err)
	if log.Log != "boot\n" {
		t.Errorf("log: %+v", log)
	}

	del, err := f.r.Delete(ctx, made.ID)
	check("delete", err)
	if del.State != machines.StateDeleting {
		t.Errorf("delete: %+v", del)
	}

	want := "fake adapter: check ran\nfake adapter: make ran\nfake adapter: list ran\nfake adapter: log ran\nfake adapter: delete ran\n"
	if got := f.stderr.String(); got != want {
		if leaks(got) {
			t.Fatal("the adapter's stderr contains the marker")
		}
		t.Errorf("the adapter's stderr is %q, want %q", got, want)
	}
}

func TestErrorDocument(t *testing.T) {
	f := newFixture(t, config{Mode: "fail", Kind: machines.KindLimit, Message: "Raise the instance quota of the account."})
	_, err := f.r.Make(context.Background(), testKey, testPool, "standard", startData)
	var e *Error
	if !errors.As(err, &e) || *e != (Error{Command: "make", Kind: machines.KindLimit, Message: "Raise the instance quota of the account."}) {
		t.Fatalf("the error is %v", err)
	}
	if err.Error() != "Raise the instance quota of the account. (machine adapter command make, kind limit)" {
		t.Errorf("the error text is %q", err.Error())
	}
}

func TestErrorMessageWithSecret(t *testing.T) {
	// The fake's Serve replaces such a message before the runner reads it.
	for _, msg := range []string{"echo", "echo64", "Check the code qecr_abc"} {
		f := newFixture(t, config{Mode: "fail", Kind: machines.KindOther, Message: msg})
		_, err := f.r.Make(context.Background(), testKey, testPool, "standard", startData)
		defect(t, err, "make", "Fix the machine adapter: its error message for make contains what looks like a run code or the start data, so Serve wrote this message in its place")
	}
	// The runner replaces it when the adapter writes it: the start data, raw (as a
	// JSON string) or in base64, the run code prefix in any case, and a cut part of
	// the start data in base64 from several start characters, a piece from the middle
	// of the base64, and the run code without its prefix.
	const want = "Fix the machine adapter: its error message for make contains what looks like a run code or the start data, so the provisioner does not show it"
	raw, _ := json.Marshal(string(startData))
	enc := base64.StdEncoding.EncodeToString(startData)
	for _, part := range []string{string(raw[1 : len(raw)-1]), enc, "QECR_abc", enc[:80], enc[1:60], enc[2:50], enc[3:45], enc[21:52], enc[100:116], "MARKERc0ffee7a1b"} {
		f := newFixture(t, config{Mode: "raw", Status: 1, Stdout: `{"error": {"kind": "other", "message": "Check the start data ` + part + `"}}`})
		_, err := f.r.Make(context.Background(), testKey, testPool, "standard", startData)
		defect(t, err, "make", want)
	}
}

func TestErrorIsOneLine(t *testing.T) {
	e := &Error{Command: "make", Kind: machines.KindOther, Message: "Raise\nthe\tquota\r\u2028now\u0085."}
	if got := e.Error(); got != "Raise the quota  now . (machine adapter command make, kind other)" {
		t.Errorf("the error text is %q", got)
	}
}

func TestDefects(t *testing.T) {
	cases := []struct {
		stdout string
		status int
		prefix string
	}{
		{"not json", 0, "Fix the machine adapter: its answer to make fails a rule of version 1: the document must be one JSON object"},
		{`{"id": "-"}`, 0, "Fix the machine adapter: its answer to make fails a rule of version 1: the member id must be"},
		{`{"id": "` + marker + ` x"}`, 0, "Fix the machine adapter: its answer to make fails a rule of version 1: the member id must be"},
		{`{"id": "i-1", "version": 1}`, 0, "Fix the machine adapter: its answer to make fails a rule of version 1: the document has a member"},
		{`{"error": {"kind": "retry", "message": "m"}}`, 0, "Fix the machine adapter: its answer to make fails a rule of version 1: the document has a member"},
		{"", 0, "Fix the machine adapter: its answer to make fails a rule of version 1"},
		{"oops " + marker, 1, "Fix the machine adapter: it exited with status 1 for make, and its standard output fails a rule of the error document: "},
		{`{"error": {"kind": "fatal", "message": "m"}}`, 1, "Fix the machine adapter: it exited with status 1 for make, and its standard output fails a rule of the error document: error: the member kind"},
		{`{"error": {"kind": "defect", "message": "Run the machine adapter as <command> --settings FILE"}}`, 2, "Fix the machine adapter: it exited with status 2 for make; only status 0 and status 1 are allowed"},
		{`{"id": "i-1"}`, 3, "Fix the machine adapter: it exited with status 3 for make; only status 0 and status 1 are allowed"},
	}
	for _, c := range cases {
		f := newFixture(t, config{Mode: "raw", Stdout: c.stdout, Status: c.status})
		_, err := f.r.Make(context.Background(), testKey, testPool, "standard", startData)
		defect(t, err, "make", c.prefix)
	}
}

func TestSignal(t *testing.T) {
	f := newFixture(t, config{Mode: "signal"})
	defect(t, f.r.Check(context.Background()), "check", "Fix the machine adapter: it ended on the signal killed during check")
}

func TestNotStarted(t *testing.T) {
	r := New(filepath.Join(t.TempDir(), "missing"), "adapter.json", &buffer{})
	defect(t, r.Check(context.Background()), "check", "Fix adapter.command in the provisioner's settings: the machine adapter did not start: ")
}

func TestOutputBound(t *testing.T) {
	f := newFixture(t, config{Mode: "flood"})
	start := time.Now()
	_, err := f.r.Make(context.Background(), testKey, testPool, "standard", startData)
	defect(t, err, "make", "Fix the machine adapter: it wrote more than 1048576 bytes on standard output for make, so the provisioner killed its process group")
	if time.Since(start) > 30*time.Second {
		t.Errorf("the runner took %s to stop the adapter", time.Since(start))
	}

	f = newFixture(t, config{Mode: "biglog"})
	log, err := f.r.Log(context.Background(), "i-1")
	if err != nil || len(log.Log) != machines.MaxLog {
		t.Errorf("a log of %d bytes, escaped: %d bytes, %v", machines.MaxLog, len(log.Log), err)
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	f := newFixture(t, config{Mode: "hang"})
	f.r.timeout = func(string) time.Duration { return 500 * time.Millisecond }
	_, err := f.r.Make(context.Background(), testKey, testPool, "standard", startData)
	defect(t, err, "make", "Fix the machine adapter: make ran longer than its time limit of 500ms, so the provisioner killed its process group")
	f.gone(t)
}

func TestCancelKillsProcessGroup(t *testing.T) {
	f := newFixture(t, config{Mode: "hang"})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for range 500 {
			if _, err := os.Stat(f.cfg.PIDFile); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	_, err := f.r.List(ctx, testPool)
	var e *Error
	if !errors.Is(err, context.Canceled) || errors.As(err, &e) {
		t.Fatalf("the error is %v, want context.Canceled and no *Error", err)
	}
	if err.Error() != "the provisioner stopped the machine adapter's list and killed its process group: context canceled" {
		t.Errorf("the error text is %q", err.Error())
	}
	f.gone(t)
}

func TestOutputLeftOpen(t *testing.T) {
	f := newFixture(t, config{Mode: "daemon", Stdout: ""})
	f.r.waitDelay = 300 * time.Millisecond
	defect(t, f.r.Check(context.Background()), "check", "Fix the machine adapter: a process that it started kept its standard output or standard error open after check exited")
	f.gone(t)
}

func TestChildOutlivesSuccess(t *testing.T) {
	f := newFixture(t, config{Mode: "orphan", Stdout: "{}\n"})
	if err := f.r.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.gone(t)
}

// TestAnswerWithOutputLeftOpen checks that an adapter that exits with status 0 keeps
// its valid answer when a child that it left holds standard output open.
func TestAnswerWithOutputLeftOpen(t *testing.T) {
	f := newFixture(t, config{Mode: "daemon", Stdout: `{"id": "i-0123456789abcdef0"}`})
	f.r.waitDelay = 300 * time.Millisecond
	made, err := f.r.Make(context.Background(), testKey, testPool, "standard", startData)
	if err != nil || made.ID != "i-0123456789abcdef0" {
		t.Errorf("make returned %+v, %v", made, err)
	}
	f.gone(t)
}

func TestInvalidRequest(t *testing.T) {
	f := newFixture(t, config{Mode: "serve"})
	_, err := f.r.Make(context.Background(), "d-1", testPool, "standard", startData)
	var e *Error
	if err == nil || errors.As(err, &e) || leaks(err.Error()) {
		t.Fatalf("the error is %v", err)
	}
	if err.Error() != "Report this defect in the provisioner: its make request to the machine adapter is not valid: the member key must be dp_ and 16 characters of 0-9 and a-z without i, l, o and u" {
		t.Errorf("the error text is %q", err.Error())
	}
	if _, err := os.Stat(f.cfg.Report); err == nil {
		t.Error("the runner started the adapter with an invalid request")
	}
	_, err = f.r.Make(context.Background(), testKey, testPool, "standard", make([]byte, machines.MaxStartData+1))
	if err == nil || !strings.HasSuffix(err.Error(), "the member start_data must be standard base64 of 1 to 16384 bytes") {
		t.Errorf("the error is %v", err)
	}
}

func TestCancelledBeforeStart(t *testing.T) {
	f := newFixture(t, config{Mode: "serve"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := f.r.Check(ctx)
	if !errors.Is(err, context.Canceled) || err.Error() != "the provisioner cancelled the machine adapter's check before it started: context canceled" {
		t.Errorf("the error is %v", err)
	}
}

// TestFakeRules checks the rules of the protocol that the fake adapter follows: one
// machine per key, gone for an unknown id, and defect for an unknown size.
func TestFakeRules(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, config{Mode: "serve"})
	first, err := f.r.Make(ctx, testKey, testPool, "standard", startData)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.r.Make(ctx, testKey, testPool, "standard", startData)
	if err != nil || again != first {
		t.Errorf("a second make with the same key returned %+v, %v; want %+v", again, err, first)
	}
	other, err := f.r.Make(ctx, "dp_0123456789abcdeg", testPool, "standard", startData)
	if err != nil || other == first {
		t.Errorf("a make with another key returned %+v, %v", other, err)
	}
	del, err := f.r.Delete(ctx, "i-ffffffffffffffff0")
	if err != nil || del.State != machines.StateGone {
		t.Errorf("the delete of an unknown id returned %+v, %v", del, err)
	}
	_, err = f.r.Make(ctx, testKey, testPool, "huge", startData)
	var e *Error
	if !errors.As(err, &e) || e.Kind != machines.KindDefect || e.Message != "Add the size to the adapter's settings: the adapter has no size of this name" {
		t.Errorf("a make with an unknown size returned %v", err)
	}
}
