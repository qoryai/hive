package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qoryai/hive/internal/loop"
	"github.com/qoryai/hive/internal/provisioner"
	"github.com/qoryai/hive/internal/provisioner/provisionertest"
	"github.com/qoryai/hive/internal/state"
)

// The markers of the secrets. No log line, error, argument, file outside the state
// folder's key file, or test output may contain MARKER.
const (
	// markerRunCode is a run code of the contract's form.
	markerRunCode = "qecr_MARKERC0FFEE7A1B0000000000.MARKERMARKERMARKERMARK"
	// markerCode is an enrolment code in QORY_ENROLMENT_CODE, which a start with an
	// access key does not use.
	markerCode = "qecp_MARKERC0FFEE7A1B0000000000.MARKERMARKERMARKERMARK"
)

// leaks is whether s contains a marker, the access key's secret, or the start of a
// secret.
func leaks(s, secret string) bool {
	return strings.Contains(s, "MARKER") || strings.Contains(s, secret) || strings.Contains(s, "qecr_") || strings.Contains(s, "qak_") || strings.Contains(s, "qecp_")
}

// provisionerFixture is a state folder enrolled with the fake Qory Apiary, a settings
// file whose adapter is the fake adapter, and the fake adapter's folder.
type provisionerFixture struct {
	srv                      *provisionertest.Server
	settings, state, adapter string
}

func newProvisionerFixture(t *testing.T) *provisionerFixture {
	t.Helper()
	srv := provisionertest.New(t)
	dir := t.TempDir()
	f := &provisionerFixture{srv: srv, state: filepath.Join(dir, "state"), adapter: filepath.Join(dir, "adapter")}
	if err := os.Mkdir(f.adapter, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := state.CheckDir(f.state); err != nil {
		t.Fatal(err)
	}
	e := state.Enrolment{ApiaryURL: srv.URL, AccessKeyID: provisionertest.KeyID, PoolID: provisionertest.PoolID, Pin: srv.Pin()}
	if err := state.Save(f.state, e, srv.Pool); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(fakeSettings{Dir: f.adapter})
	adapterSettings := writeFile(t, dir, "adapter.json", string(b))
	b, _ = json.Marshal(map[string]any{
		"apiary_url":      srv.URL,
		"gateway_address": "::ffff:10.0.1.5",
		"max_machines":    1,
		"adapter":         map[string]string{"command": exe, "settings": adapterSettings},
	})
	f.settings = writeFile(t, dir, "settings.json", string(b))
	return f
}

// serving is one run of serve in the background.
type serving struct {
	stop   chan os.Signal
	stderr *buffer
	code   chan int
}

// start runs serve with f's files, and QORY_ENROLMENT_CODE set to markerCode.
func (f *provisionerFixture) start() *serving {
	s := &serving{stop: make(chan os.Signal, 1), stderr: &buffer{}, code: make(chan int, 1)}
	getenv := func(name string) string {
		if name == "QORY_ENROLMENT_CODE" {
			return markerCode
		}
		return ""
	}
	go func() {
		s.code <- run([]string{"serve", "--settings", f.settings, "--state-dir", f.state}, env{stderr: s.stderr, getenv: getenv, stop: s.stop})
	}()
	return s
}

// end sends SIGTERM and returns serve's exit status.
func (s *serving) end(t *testing.T) int {
	t.Helper()
	s.stop <- syscall.SIGTERM
	select {
	case code := <-s.code:
		return code
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not return within 30 s of SIGTERM")
		return 0
	}
}

// waitFor polls ok every 20 ms for up to 20 s.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 1000 {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("waited 20 s for %s", what)
}

// commands are the commands the fake adapter ran, by name.
func (f *provisionerFixture) commands(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.adapter, fakeCommands))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for line := range strings.Lines(string(b)) {
		names = append(names, strings.Fields(line)[0])
	}
	return names
}

// checkNoLeak fails the test when stderr, a file of the fake adapter or a file of the
// state folder other than the key file contains a secret. It never prints the text
// that leaks.
func (f *provisionerFixture) checkNoLeak(t *testing.T, stderr string) {
	t.Helper()
	secret := f.srv.Pool.Secret()
	if leaks(stderr, secret) {
		t.Error("serve's standard error contains a secret")
	}
	for _, dir := range []string{f.adapter, f.state} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if dir == f.state && e.Name() == state.KeyFile {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if leaks(string(b), secret) {
				t.Errorf("the file %s contains a secret", e.Name())
			}
		}
	}
}

// TestServeRuns runs serve against the fake Qory Apiary and the fake adapter: it
// claims the one work, makes its machine with the start data, sends dispatched, and
// on SIGTERM stops with exit status 0 and leaves the machine. A restart then resumes
// the dispatch from the journal and makes no machine.
func TestServeRuns(t *testing.T) {
	f := newProvisionerFixture(t)
	work := f.srv.Offer(provisionertest.Work{RunCode: markerRunCode, GatewayURL: "https://gateway.example.com:8443", MaxLifetimeMinutes: 60})[0]

	s := f.start()
	waitFor(t, "the dispatched", func() bool {
		d := f.srv.Dispatches()
		return len(d) == 1 && d[0].State == provisionertest.StateDispatched
	})
	code := s.end(t)
	stderr := s.stderr.String()
	f.checkNoLeak(t, stderr)
	if code != 0 {
		t.Fatalf("exit %d after SIGTERM, want 0", code)
	}
	d := f.srv.Dispatches()[0]
	if d.External != nil {
		t.Errorf("dispatched sent the external %+v, want none", *d.External)
	}
	if got, want := f.commands(t), []string{"check", "list", "make"}; !slices.Equal(got, want) {
		t.Errorf("the adapter ran %q, want %q", got, want)
	}
	b, err := os.ReadFile(filepath.Join(f.adapter, fakeMade))
	if err != nil {
		t.Fatal(err)
	}
	var m made
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(work.RunCode))
	want := made{
		Key: d.ID, Pool: provisionertest.PoolID, Size: loop.DefaultSize, RunCodeSHA256: hex.EncodeToString(sum[:]),
		GatewayURL: "https://gateway.example.com:8443", GatewayAddress: "10.0.1.5", MaxLifetimeMinutes: 60,
	}
	if m != want {
		t.Errorf("the make request was %+v, want %+v", m, want)
	}
	for _, line := range []string{
		`level=INFO msg="Remove QORY_ENROLMENT_CODE from the environment: the state folder has an access key, so the code is not used"`,
		`level=INFO msg="started the provisioner" version=`,
		`apiary_url=` + f.srv.URL + ` pool_id=` + provisionertest.PoolID + ` access_key_id=` + provisionertest.KeyID,
		`max_machines=1 gateway_address=10.0.1.5 state_dir=` + f.state,
		`level=INFO msg="sent dispatched" dispatch_id=` + d.ID + ` machine_id=i-00000000000000001`,
		`level=INFO msg="received a stop signal: the provisioner stops claiming" signal=terminated`,
		`level=INFO msg="stopped claiming"`,
		`level=INFO msg="the provisioner stopped"`,
	} {
		if !strings.Contains(stderr, line) && !leaks(stderr, f.srv.Pool.Secret()) {
			t.Errorf("stderr has no %s:\n%s", line, stderr)
		}
	}
	journal, err := os.ReadFile(filepath.Join(f.state, "dispatches"))
	if err != nil {
		t.Fatal(err)
	}
	if want := d.ID + " dispatched i-00000000000000001 60\n"; string(journal) != want {
		t.Errorf("the journal is %q, want %q", journal, want)
	}

	calls := len(f.srv.Calls())
	s = f.start()
	waitFor(t, "the resumed dispatch", func() bool { return strings.Contains(s.stderr.String(), `msg="resumed a dispatch"`) })
	if code := s.end(t); code != 0 {
		t.Errorf("exit %d after SIGTERM on the restart, want 0", code)
	}
	f.checkNoLeak(t, s.stderr.String())
	if got, want := f.commands(t), []string{"check", "list", "make", "check", "list"}; !slices.Equal(got, want) {
		t.Errorf("the adapter ran %q, want %q", got, want)
	}
	if n := len(f.srv.Calls()); n != calls {
		t.Errorf("the restart sent %d requests to Qory Apiary, want none: the journal is full", n-calls)
	}
}

// TestServeStopsEarly checks that SIGTERM right after the start, whichever step runs,
// stops serve with exit status 0.
func TestServeStopsEarly(t *testing.T) {
	f := newProvisionerFixture(t)
	s := f.start()
	code := s.end(t)
	f.checkNoLeak(t, s.stderr.String())
	if code != 0 {
		t.Errorf("exit %d, want 0; stderr\n%s", code, s.stderr.String())
	}
	if !strings.Contains(s.stderr.String(), `msg="the provisioner stopped"`) {
		t.Errorf("stderr has no line that the provisioner stopped:\n%s", s.stderr.String())
	}
}

// TestToLoop checks that each client error the loop acts on also matches the loop's
// sentinel and keeps its text, and that every other error is returned as it is.
func TestToLoop(t *testing.T) {
	for _, c := range []struct{ client, loop error }{
		{provisioner.ErrGone, loop.ErrGone},
		{provisioner.ErrLeaseExpired, loop.ErrLeaseExpired},
		{provisioner.ErrMachineExists, loop.ErrMachineExists},
	} {
		err := fmt.Errorf("the release of dp_0123456789abcdef: %w", c.client)
		got := toLoop(err)
		if !errors.Is(got, c.loop) || !errors.Is(got, c.client) || got.Error() != err.Error() {
			t.Errorf("%v: got %q, want it to match %v and %v with the same text", c.client, got, c.loop, c.client)
		}
	}
	for _, err := range []error{
		provisioner.ErrForbidden, provisioner.ErrNotFound, provisioner.ErrMachineLogExists, provisioner.ErrNoGateway,
		provisioner.ErrRateLimited, provisioner.ErrUnsigned, provisioner.ErrTooLarge, provisioner.ErrUnreachable,
		provisioner.ErrAnswerUnusable, context.Canceled,
	} {
		if got := toLoop(err); got != err {
			t.Errorf("%v: got %v, want it as it is", err, got)
		}
	}
	if toLoop(nil) != nil {
		t.Error("nil did not stay nil")
	}
}

// TestApiaryRefusals sends real refusals through the wrapper and the fake Qory Apiary.
func TestApiaryRefusals(t *testing.T) {
	srv := provisionertest.New(t)
	srv.Hold = 10 * time.Millisecond
	a := apiary{client: srv.Client()}
	ctx := context.Background()
	if _, ok, err := a.Claim(ctx); ok || err != nil {
		t.Fatalf("a claim with no work: %v, %v, want false and no error", ok, err)
	}
	srv.Offer(provisionertest.Work{RunCode: markerRunCode}, provisionertest.Work{}, provisionertest.Work{}, provisionertest.Work{})
	var ids []string
	for range 4 {
		c, ok, err := a.Claim(ctx)
		if !ok || err != nil {
			t.Fatalf("claim: %v, %v", ok, err)
		}
		if c.PoolID != provisionertest.PoolID || c.GatewayURL != provisionertest.GatewayURL || c.MaxLifetimeMinutes != provisionertest.MaxLifetimeMinutes || c.LeaseUntil.IsZero() {
			t.Errorf("the claim is %+v", c)
		}
		if p := fmt.Sprintf("%v %+v %#v", c, c, c); strings.Contains(p, "MARKER") || !strings.Contains(p, "[run code]") {
			t.Error("a printed claim shows the run code")
		}
		ids = append(ids, c.DispatchID)
	}
	const machine = "i-0123456789abcdef0"
	srv.End(ids[0])
	srv.Expire(ids[1])
	if err := a.Dispatched(ctx, ids[2], machine); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		err  error
		want error
	}{
		"dispatched of an ended dispatch":   {a.Dispatched(ctx, ids[0], machine), loop.ErrGone},
		"dispatched of a stale dispatch":    {a.Dispatched(ctx, ids[1], machine), loop.ErrLeaseExpired},
		"release after dispatched":          {a.Release(ctx, ids[2], "the machine was not made"), loop.ErrMachineExists},
		"release of an ended dispatch":      {a.Release(ctx, ids[0], "the machine was not made"), loop.ErrGone},
		"release of a stale dispatch":       {a.Release(ctx, ids[1], "the machine was not made"), loop.ErrLeaseExpired},
		"dispatched of an unknown dispatch": {a.Dispatched(ctx, "dp_0123456789abcdef", machine), provisioner.ErrForbidden},
	} {
		if !errors.Is(c.err, c.want) {
			t.Errorf("%s: %v, want %v", name, c.err, c.want)
		}
	}
	if err := a.MachineLog(ctx, ids[3], "boot\n", false); err != nil {
		t.Fatal(err)
	}
	err := a.MachineLog(ctx, ids[3], "boot\n", false)
	if !errors.Is(err, provisioner.ErrMachineLogExists) || errors.Is(err, loop.ErrGone) || errors.Is(err, loop.ErrLeaseExpired) || errors.Is(err, loop.ErrMachineExists) {
		t.Errorf("a second machine log: %v, want machine_log_exists and no loop sentinel", err)
	}
	if d, _ := srv.Dispatch(ids[2]); d.External != nil {
		t.Errorf("dispatched sent the external %+v, want none", *d.External)
	}
}
