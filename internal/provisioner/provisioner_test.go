package provisioner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/qoryai/forager/accesskey"
	"github.com/qoryai/integrations/contracts"

	"github.com/qoryai/hive/internal/provisioner"
	"github.com/qoryai/hive/internal/provisioner/provisionertest"
)

// testTime is when the tests' clock starts: the time of the claim fixture.
var testTime = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// clock is the client's and the fake's clock: it reads testTime until a wait moves it
// on, and records the waits.
type clock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

func (c *clock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func (c *clock) sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, d)
	c.now = c.now.Add(d)
	return ctx.Err()
}

func (c *clock) slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.waits)
}

// setup is a fake and its client, both on one clock at testTime, the client logging to
// the buffer returned.
func setup(t *testing.T) (*provisionertest.Server, *provisioner.Client, *clock, *bytes.Buffer) {
	t.Helper()
	clk := &clock{now: testTime}
	srv := provisionertest.New(t)
	srv.Now = clk.read
	c := srv.Client()
	c.Now, c.Sleep = clk.read, clk.sleep
	var log bytes.Buffer
	c.Log = slog.New(slog.NewJSONHandler(&log, nil))
	return srv, c, clk, &log
}

// claimed claims the work srv offers, which must be there.
func claimed(t *testing.T, srv *provisionertest.Server, c *provisioner.Client, w provisionertest.Work) provisioner.Claimed {
	t.Helper()
	srv.Offer(w)
	cl, ok, err := c.Claim(context.Background(), provisioner.MaxWaitSeconds, nil)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	return cl
}

// external is the job a dispatched sends in the tests.
var external = &contracts.External{Type: "example-ci", Ref: "i-0123456789abcdef0", URL: "https://ci.example.com/acme/shop/runs/8812"}

// dispatched claims the work srv offers and posts its first dispatched.
func dispatched(t *testing.T, srv *provisionertest.Server, c *provisioner.Client, w provisionertest.Work) provisioner.Claimed {
	t.Helper()
	cl := claimed(t, srv, c, w)
	if err := c.Dispatched(context.Background(), cl.DispatchID, external, ""); err != nil {
		t.Fatal(err)
	}
	return cl
}

// fixture reads one provisioner/v1 fixture.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := fs.ReadFile(contracts.ProvisionerFS, "fixtures/"+name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sameJSON reports whether a and b are the same JSON value.
func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}

// last is the last call srv got.
func last(t *testing.T, srv *provisionertest.Server) provisionertest.Call {
	t.Helper()
	calls := srv.Calls()
	if len(calls) == 0 {
		t.Fatal("no call")
	}
	return calls[len(calls)-1]
}

// written is what h writes.
func written(t *testing.T, h provisioner.Hidden) string {
	t.Helper()
	var b bytes.Buffer
	if _, err := h.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestNewChecksTheConfig makes a client of the fake's config, then of the config
// changed in one place each: every change is an error that contains no secret, and a
// client of the fake's config is made.
func TestNewChecksTheConfig(t *testing.T) {
	srv := provisionertest.New(t)
	if _, err := provisioner.New(srv.Config()); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*provisioner.Config){
		"an empty URL":          func(c *provisioner.Config) { c.URL = "" },
		"a path":                func(c *provisioner.Config) { c.URL += "/v1" },
		"a trailing slash":      func(c *provisioner.Config) { c.URL += "/" },
		"http to a host":        func(c *provisioner.Config) { c.URL = "http://apiary.example.com" },
		"a query":               func(c *provisioner.Config) { c.URL = "https://apiary.example.com?a=b" },
		"a secret in the URL":   func(c *provisioner.Config) { c.URL = "https://qak_abc.example.com" },
		"a run code in the URL": func(c *provisioner.Config) { c.URL = "https://qecr_abc.example.com" },
		"no key":                func(c *provisioner.Config) { c.Key = nil },
		"a key id":              func(c *provisioner.Config) { c.KeyID = "ak_SHORT" },
		"an empty pin":          func(c *provisioner.Config) { c.Pin = nil },
		"a pin's key":           func(c *provisioner.Config) { c.Pin = accesskey.Pin{{Alg: "ed25519", PublicKey: "AAAA"}} },
		"an empty instance id":  func(c *provisioner.Config) { c.InstanceID = "" },
		"an instance id secret": func(c *provisioner.Config) { c.InstanceID = "qrk_abc.def" },
	} {
		cfg := srv.Config()
		edit(&cfg)
		c, err := provisioner.New(cfg)
		if err == nil || c != nil || accesskey.ContainsSecret(err.Error()) || strings.Contains(err.Error(), "qecr_") || !strings.HasPrefix(err.Error(), "set ") {
			t.Errorf("%s: %v", name, err)
		}
	}
	cfg := srv.Config()
	cfg.URL = "https://apiary.example.com:8443"
	if _, err := provisioner.New(cfg); err != nil {
		t.Errorf("an https origin with a port: %v", err)
	}
}

// TestAMachineGoesThroughEveryRequest claims work, posts dispatched three times, uploads
// the machine log, gets 410 gone to dispatched once the dispatch ended, and releases a
// second dispatch before dispatched. Each request is signed with the pool's key, with
// the headers the contract lists, a body that passes its schema and the clock's time,
// and each answer's signature is verified under the pin.
func TestAMachineGoesThroughEveryRequest(t *testing.T) {
	srv, c, clk, _ := setup(t)
	ctx := context.Background()
	spec := contracts.MachineSpec{VCPUs: 2, MemoryMiB: 4096, DiskGiB: 30, Arch: "amd64"}
	offers := []contracts.MachineSpec{spec, {VCPUs: 4, MemoryMiB: 16384, DiskGiB: 16384, Arch: "arm64", Virtualization: true}}
	offered := srv.Offer(provisionertest.Work{Machine: "small", MachineSpec: &spec, MaxLifetimeMinutes: 120})[0]
	cl, ok, err := c.Claim(ctx, provisioner.MaxWaitSeconds, offers)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if cl.PoolID != provisionertest.PoolID || !cl.LeaseUntil.Equal(testTime.Add(provisionertest.Lease)) || cl.Machine != "small" ||
		cl.MachineSpec == nil || *cl.MachineSpec != spec ||
		cl.GatewayURL != provisionertest.GatewayURL || cl.MaxLifetimeMinutes != 120 || written(t, cl.RunCode) != offered.RunCode {
		t.Errorf("claimed %+v", cl)
	}
	if d, ok := srv.Dispatch(cl.DispatchID); !ok || d.State != provisionertest.StateClaimed || !reflect.DeepEqual(d.Offers, offers) {
		t.Errorf("the dispatch %v, offers %+v", d, d.Offers)
	}
	cl.RunCode.Clear()
	for range 3 {
		if err := c.Dispatched(ctx, cl.DispatchID, external, "t3.medium"); err != nil {
			t.Fatal(err)
		}
	}
	if d, _ := srv.Dispatch(cl.DispatchID); d.State != provisionertest.StateDispatched || d.Dispatched != 3 || d.External == nil || *d.External != *external || d.MachineType != "t3.medium" {
		t.Errorf("after dispatched: %+v", d)
	}
	if err := c.MachineLog(ctx, cl.DispatchID, "machine started\n", false); err != nil {
		t.Fatal(err)
	}
	srv.End(cl.DispatchID)
	if err := c.Dispatched(ctx, cl.DispatchID, external, ""); !errors.Is(err, provisioner.ErrGone) {
		t.Errorf("dispatched after the end: %v", err)
	}
	if d, _ := srv.Dispatch(cl.DispatchID); d.MachineLog == nil || d.MachineLog.Log != "machine started\n" || d.MachineLog.Truncated {
		t.Errorf("the machine log %+v", d.MachineLog)
	}

	other := claimed(t, srv, c, provisionertest.Work{})
	other.RunCode.Clear()
	if err := c.Release(ctx, other.DispatchID, "The machine could not be created."); err != nil {
		t.Fatal(err)
	}
	if d, _ := srv.Dispatch(other.DispatchID); d.State != provisionertest.StateEnded || d.Release == nil || d.Release.Reason != "The machine could not be created." {
		t.Errorf("after the release: %+v", d)
	}

	for _, call := range srv.Calls() {
		h := call.Header
		switch {
		case !call.Verified || call.Replayed || !call.Signed:
			t.Errorf("%s: verified %v, replayed %v, answer signed %v", call.Route, call.Verified, call.Replayed, call.Signed)
		case h.Get(accesskey.HeaderAccessKeyID) != provisionertest.KeyID || h.Get(accesskey.HeaderInstanceID) != provisionertest.InstanceID:
			t.Errorf("%s: the key's headers %v", call.Route, h)
		case h.Get("X-Qory-Contract-Version") != strconv.Itoa(provisioner.Revision) || h.Get("Content-Type") != "application/json" || h.Get("User-Agent") != "qory-provisioner":
			t.Errorf("%s: the headers %v", call.Route, h)
		case call.Time.Before(testTime) || call.Time.After(clk.read()) || call.Time.Nanosecond() != 0:
			t.Errorf("%s: the time %s", call.Route, call.Time)
		}
	}
	want := []provisionertest.Route{"claim", "dispatched", "dispatched", "dispatched", "machine-log", "dispatched", "claim", "release"}
	if got := srv.Routes(); !slices.Equal(got, want) {
		t.Errorf("routes %v, want %v", got, want)
	}
}

// TestDispatchedIsBuiltAnewEachTime posts dispatched three times within one second of
// the clock, with the same external: each has a later time, other bytes and a new
// signature, the fake's replay check passes each, and the fake answers each with the
// 204.
func TestDispatchedIsBuiltAnewEachTime(t *testing.T) {
	srv, c, _, _ := setup(t)
	cl := dispatched(t, srv, c, provisionertest.Work{})
	cl.RunCode.Clear()
	for range 2 {
		if err := c.Dispatched(context.Background(), cl.DispatchID, external, ""); err != nil {
			t.Fatal(err)
		}
	}
	calls := srv.Calls()[1:]
	for i := 1; i < len(calls); i++ {
		if !calls[i].Time.After(calls[i-1].Time) || calls[i].Header.Get(accesskey.HeaderSignature) == calls[i-1].Header.Get(accesskey.HeaderSignature) ||
			bytes.Equal(calls[i].Body, calls[i-1].Body) || calls[i].Replayed || calls[i].Status != 204 {
			t.Errorf("dispatched %d: %s after %s, status %d", i+1, calls[i].Time, calls[i-1].Time, calls[i].Status)
		}
	}
}

// at sets clk to the RFC 3339 time s.
func at(t *testing.T, clk *clock, s string) {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	clk.set(tm)
}

// TestTheBodiesAreTheFixtures builds each request whose fixture the contract has, at
// the fixture's time, and checks that the body sent is the fixture: the claim waits 25
// seconds and has no node pool, dispatched has no external and then one, and the
// machine log is whole and then truncated.
func TestTheBodiesAreTheFixtures(t *testing.T) {
	srv, c, clk, _ := setup(t)
	ctx := context.Background()
	check := func(name string) {
		t.Helper()
		if b := fixture(t, name); !sameJSON(t, last(t, srv).Body, b) {
			t.Errorf("%s: sent\n%s\nwant\n%s", name, last(t, srv).Body, b)
		}
	}
	cl := claimed(t, srv, c, provisionertest.Work{})
	cl.RunCode.Clear()
	check("claim/without-pool-id.json")

	at(t, clk, "2026-10-15T12:00:20Z")
	if err := c.Dispatched(ctx, cl.DispatchID, nil, ""); err != nil {
		t.Fatal(err)
	}
	check("dispatched/without-external.json")
	var df contracts.Dispatched
	if err := json.Unmarshal(fixture(t, "dispatched/with-external.json"), &df); err != nil {
		t.Fatal(err)
	}
	if err := c.Dispatched(ctx, cl.DispatchID, df.External, ""); err != nil {
		t.Fatal(err)
	}
	check("dispatched/with-external.json")

	var mf contracts.MachineLog
	if err := json.Unmarshal(fixture(t, "machine-log/whole.json"), &mf); err != nil {
		t.Fatal(err)
	}
	at(t, clk, mf.Time)
	if err := c.MachineLog(ctx, cl.DispatchID, mf.Log, false); err != nil {
		t.Fatal(err)
	}
	check("machine-log/whole.json")

	at(t, clk, "2026-10-15T12:00:01Z")
	cl = claimed(t, srv, c, provisionertest.Work{})
	cl.RunCode.Clear()
	var rf contracts.Release
	if err := json.Unmarshal(fixture(t, "release/no-capacity.json"), &rf); err != nil {
		t.Fatal(err)
	}
	at(t, clk, rf.Time)
	if err := c.Release(ctx, cl.DispatchID, rf.Reason); err != nil {
		t.Fatal(err)
	}
	check("release/no-capacity.json")
	if err := json.Unmarshal(fixture(t, "machine-log/truncated.json"), &mf); err != nil {
		t.Fatal(err)
	}
	at(t, clk, mf.Time)
	if err := c.MachineLog(ctx, cl.DispatchID, mf.Log, true); err != nil {
		t.Fatal(err)
	}
	check("machine-log/truncated.json")
}

// TestAClaimSendsItsWait claims with each wait from 0 to 25 seconds: the body has the
// wait. A wait outside that range is an error, and nothing is sent.
func TestAClaimSendsItsWait(t *testing.T) {
	srv, c, _, _ := setup(t)
	srv.Hold = 0
	for _, wait := range []int{0, 1, 25} {
		if _, ok, err := c.Claim(context.Background(), wait, nil); err != nil || ok {
			t.Fatalf("wait %d: %v %v", wait, ok, err)
		}
		var body contracts.Claim
		if err := json.Unmarshal(last(t, srv).Body, &body); err != nil || body.WaitSeconds != wait {
			t.Errorf("wait %d: the body %s", wait, last(t, srv).Body)
		}
	}
	before := len(srv.Calls())
	for _, wait := range []int{-1, 26} {
		if _, _, err := c.Claim(context.Background(), wait, nil); err == nil || !strings.Contains(err.Error(), "set wait_seconds from 0 to 25") {
			t.Errorf("wait %d: %v", wait, err)
		}
	}
	if n := len(srv.Calls()) - before; n != 0 {
		t.Errorf("%d requests sent", n)
	}
}

// TestTheAnswersAreTheFixtures sends each claim answer fixture, signed, to a claim, and
// checks what the client read.
func TestTheAnswersAreTheFixtures(t *testing.T) {
	names, err := fs.Glob(contracts.ProvisionerFS, "fixtures/claim-answer/*.json")
	if err != nil || len(names) < 3 {
		t.Fatalf("the fixtures %v: %v", names, err)
	}
	for _, name := range names {
		name = strings.TrimPrefix(name, "fixtures/")
		t.Run(name, func(t *testing.T) {
			srv, c, _, _ := setup(t)
			body := fixture(t, name)
			srv.Script(provisionertest.RouteClaim, provisionertest.Reply{Status: http.StatusOK, Body: string(body)})
			cl, ok, err := c.Claim(context.Background(), provisioner.MaxWaitSeconds, nil)
			if err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			var ca contracts.ClaimAnswer
			if err := json.Unmarshal(body, &ca); err != nil {
				t.Fatal(err)
			}
			if cl.DispatchID != ca.DispatchID || cl.PoolID != ca.PoolID || contracts.FormatTime(cl.LeaseUntil) != ca.LeaseUntil || cl.Machine != ca.Machine ||
				written(t, cl.RunCode) != ca.RunCode || cl.GatewayURL != ca.GatewayURL || cl.MaxLifetimeMinutes != ca.MaxLifetimeMinutes {
				t.Errorf("claimed %+v, want %+v", cl, ca)
			}
		})
	}
}

// TestEveryInvalidClaimAnswerIsRefused sends each of the contract's invalid claim
// answers, signed, to a claim: each is ErrAnswerUnusable after one try, with no run
// code, and no error or log line contains a run code.
func TestEveryInvalidClaimAnswerIsRefused(t *testing.T) {
	names, err := fs.Glob(contracts.ProvisionerFS, "fixtures/invalid/claim-answer-*.json")
	if err != nil || len(names) < 10 {
		t.Fatalf("the fixtures %v: %v", names, err)
	}
	for _, name := range names {
		name = strings.TrimPrefix(name, "fixtures/")
		srv, c, _, log := setup(t)
		srv.Script(provisionertest.RouteClaim, provisionertest.Reply{Status: http.StatusOK, Body: string(fixture(t, name))})
		cl, ok, err := c.Claim(context.Background(), provisioner.MaxWaitSeconds, nil)
		if !errors.Is(err, provisioner.ErrAnswerUnusable) || ok || cl.RunCode.Len() != 0 || len(srv.Calls()) != 1 {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if text := strings.ToLower(err.Error() + log.String()); strings.Contains(text, "qecr_") || strings.Contains(text, "qecn_") {
			t.Errorf("%s: the error or the log contains a run code: %s", name, text)
		}
	}
}

// failing is each way a try fails, so it is built anew and sent again: no answer, an
// unsigned answer of each status, one signed under another key, one whose signature
// header is sent twice, an answer over its bound, a signed server error, and an
// unsigned 429, whose Retry-After the client does not wait.
var failing = []struct {
	name  string
	reply provisionertest.Reply
	is    error
}{
	{"no answer", provisionertest.Reply{Status: 204, Drop: true}, provisioner.ErrUnreachable},
	{"an unsigned 401", provisionertest.Reply{Status: 401, Code: "unauthorized", Unsigned: true}, provisioner.ErrUnsigned},
	{"an unsigned 413", provisionertest.Reply{Status: 413, Code: "payload_too_large", Unsigned: true}, provisioner.ErrUnsigned},
	{"an unsigned 400", provisionertest.Reply{Status: 400, Code: "bad_request", Unsigned: true}, provisioner.ErrUnsigned},
	{"an unsigned 503", provisionertest.Reply{Status: 503, Unsigned: true}, provisioner.ErrUnsigned},
	{"an unsigned 410", provisionertest.Reply{Status: 410, Code: "gone", Unsigned: true}, provisioner.ErrUnsigned},
	{"an unsigned 204", provisionertest.Reply{Status: 204, Unsigned: true}, provisioner.ErrUnsigned},
	{"an unsigned 429", provisionertest.Reply{Status: 429, Code: "rate_limited", Unsigned: true, RetryAfter: "3000"}, provisioner.ErrUnsigned},
	{"another key", provisionertest.Reply{Status: 204, WrongKey: true}, provisioner.ErrUnsigned},
	// The signature and then an empty one, and two empty configuration headers: each
	// joined is what was signed, so only the count fails them.
	{"the signature twice", provisionertest.Reply{Status: 204, Header: http.Header{accesskey.HeaderSignature: {""}}}, provisioner.ErrUnsigned},
	{"a configuration header twice", provisionertest.Reply{Status: 204, Header: http.Header{accesskey.HeaderConfiguration: {"", ""}}}, provisioner.ErrUnsigned},
	{"over its bound", provisionertest.Reply{Status: 409, Code: "gone", Pad: provisioner.MaxAnswer}, provisioner.ErrTooLarge},
	{"a signed 500", provisionertest.Reply{Status: 500}, nil},
	{"a signed 503", provisionertest.Reply{Status: 503}, nil},
}

// TestAFailedTryIsBuiltAnewAndSentAgain sends each failure to every try of a release:
// the client sends five, each with a later time and a new signature after a wait
// doubling from a second, logs a line with fixed text before each new one, and returns
// the failure. A failure then a signed 204 is the release done.
func TestAFailedTryIsBuiltAnewAndSentAgain(t *testing.T) {
	for _, f := range failing {
		t.Run(f.name, func(t *testing.T) {
			srv, c, clk, log := setup(t)
			cl := claimed(t, srv, c, provisionertest.Work{})
			cl.RunCode.Clear()
			for range 5 {
				srv.Script(provisionertest.RouteRelease, f.reply)
			}
			err := c.Release(context.Background(), cl.DispatchID, "The provisioner stopped before the machine was created.")
			if f.is == nil {
				var r *provisioner.Refusal
				if !errors.As(err, &r) || !r.Signed || r.Status != f.reply.Status || !r.Resent {
					t.Errorf("err %v", err)
				}
			} else if !errors.Is(err, f.is) {
				t.Errorf("err %v, want %v", err, f.is)
			}
			calls := srv.Calls()[1:]
			if len(calls) != 5 {
				t.Fatalf("%d tries", len(calls))
			}
			for i := 1; i < len(calls); i++ {
				if !calls[i].Time.After(calls[i-1].Time) || calls[i].Header.Get(accesskey.HeaderSignature) == calls[i-1].Header.Get(accesskey.HeaderSignature) || calls[i].Replayed {
					t.Errorf("try %d is not built anew: %s after %s", i+1, calls[i].Time, calls[i-1].Time)
				}
			}
			if w := clk.slept(); !slices.Equal(w, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}) {
				t.Errorf("waits %v", w)
			}
			if n := strings.Count(log.String(), `"msg":"a try failed, so the client sends the request again after the wait"`); n != 4 {
				t.Errorf("%d lines logged:\n%s", n, log)
			}

			srv2, c2, _, _ := setup(t)
			cl2 := claimed(t, srv2, c2, provisionertest.Work{})
			cl2.RunCode.Clear()
			srv2.Script(provisionertest.RouteRelease, f.reply)
			if err := c2.Release(context.Background(), cl2.DispatchID, "The provisioner stopped before the machine was created."); err != nil {
				t.Errorf("a failure then a 204: %v", err)
			}
		})
	}
}

// TestASignedAnswerIsNotSentAgain sends each signed refusal below 500 to a release: one
// try each, returned as it is.
func TestASignedAnswerIsNotSentAgain(t *testing.T) {
	srv, c, _, _ := setup(t)
	cl := claimed(t, srv, c, provisionertest.Work{})
	cl.RunCode.Clear()
	for _, rep := range []provisionertest.Reply{
		{Status: 403, Code: "forbidden"}, {Status: 400, Code: "invalid_request"}, {Status: 429, Code: "rate_limited"},
		{Status: 413, Code: "payload_too_large"}, {Status: 409, Code: "machine_exists"},
	} {
		srv.Script(provisionertest.RouteRelease, rep)
		before := len(srv.Calls())
		var r *provisioner.Refusal
		if err := c.Release(context.Background(), cl.DispatchID, "a reason"); !errors.As(err, &r) || !r.Signed || r.Status != rep.Status || r.Code != rep.Code || r.Resent {
			t.Errorf("%d: %v", rep.Status, err)
		}
		if n := len(srv.Calls()) - before; n != 1 {
			t.Errorf("%d: %d tries", rep.Status, n)
		}
	}
}

// TestAReleaseAfterALostAnswerIsResent drops the 204 of a release after the fake ended
// the dispatch: the try sent again gets 410 gone, with Resent set.
func TestAReleaseAfterALostAnswerIsResent(t *testing.T) {
	srv, c, _, _ := setup(t)
	cl := claimed(t, srv, c, provisionertest.Work{})
	cl.RunCode.Clear()
	srv.Script(provisionertest.RouteRelease, provisionertest.Reply{Drop: true})
	err := c.Release(context.Background(), cl.DispatchID, "a reason")
	var r *provisioner.Refusal
	if !errors.Is(err, provisioner.ErrGone) || !errors.As(err, &r) || !r.Resent {
		t.Errorf("err %v", err)
	}
	if d, _ := srv.Dispatch(cl.DispatchID); d.State != provisionertest.StateEnded || d.Release == nil {
		t.Error("the fake did not end the dispatch")
	}
}

// TestTheSameBytesAreNeverSentTwice sends one release twice within one second of the
// clock: the second waits for the next second, so its time and its bytes differ. A
// distinct request in the same second does not wait.
func TestTheSameBytesAreNeverSentTwice(t *testing.T) {
	srv, c, clk, _ := setup(t)
	clk.set(testTime.Add(300 * time.Millisecond))
	cl := claimed(t, srv, c, provisionertest.Work{})
	cl.RunCode.Clear()
	srv.Script(provisionertest.RouteRelease, provisionertest.Reply{Status: 204}, provisionertest.Reply{Status: 204}, provisionertest.Reply{Status: 204})
	ctx := context.Background()
	if err := c.Release(ctx, cl.DispatchID, "a reason"); err != nil {
		t.Fatal(err)
	}
	if err := c.Release(ctx, cl.DispatchID, "another reason"); err != nil {
		t.Fatal(err)
	}
	if w := clk.slept(); len(w) != 0 {
		t.Errorf("a distinct request waited %v", w)
	}
	if err := c.Release(ctx, cl.DispatchID, "a reason"); err != nil {
		t.Fatal(err)
	}
	if w := clk.slept(); !slices.Equal(w, []time.Duration{700 * time.Millisecond}) {
		t.Errorf("waits %v", w)
	}
	calls := srv.Calls()
	if bytes.Equal(calls[1].Body, calls[3].Body) || !calls[3].Time.Equal(testTime.Add(time.Second)) {
		t.Errorf("the same bytes twice: %s %s", calls[1].Body, calls[3].Body)
	}
}

// TestRefusalsMatchTheirRequest sends each signed refusal code to each request:
// errors.Is matches the code's error only for a request whose schema lists it, and
// never an unsigned answer of the same status and body.
func TestRefusalsMatchTheirRequest(t *testing.T) {
	all := []provisionertest.Route{"claim", "dispatched", "release", "machine-log", "machine-end"}
	codes := []struct {
		err    error
		status int
		code   string
		on     []provisionertest.Route
	}{
		{provisioner.ErrForbidden, 403, "forbidden", all},
		{provisioner.ErrNotFound, 404, "not_found", all},
		{provisioner.ErrGone, 410, "gone", []provisionertest.Route{"dispatched", "release"}},
		{provisioner.ErrLeaseExpired, 409, "lease_expired", []provisionertest.Route{"dispatched", "release"}},
		{provisioner.ErrMachineExists, 409, "machine_exists", []provisionertest.Route{"release"}},
		{provisioner.ErrMachineLogExists, 409, "machine_log_exists", []provisionertest.Route{"machine-log"}},
		{provisioner.ErrNoGateway, 409, "no_gateway", []provisionertest.Route{"claim"}},
		{provisioner.ErrRateLimited, 429, "rate_limited", all},
		{provisioner.ErrUnavailable, 503, "unavailable", []provisionertest.Route{"machine-end"}},
	}
	for _, code := range codes {
		for _, route := range all {
			srv, c, _, _ := setup(t)
			cl := claimed(t, srv, c, provisionertest.Work{})
			cl.RunCode.Clear()
			srv.Script(route, slices.Repeat([]provisionertest.Reply{{Status: code.status, Code: code.code}}, triesOf(route, code.status))...)
			err := send(c, route, cl)
			if want := slices.Contains(code.on, route); errors.Is(err, code.err) != want {
				t.Errorf("%s to %s: %v, matches %v, want %v", code.code, route, err, !want, want)
			}
			unsigned := provisionertest.Reply{Status: code.status, Code: code.code, Unsigned: true}
			srv.Script(route, slices.Repeat([]provisionertest.Reply{unsigned}, triesOf(route, 500))...)
			if err := send(c, route, cl); errors.Is(err, code.err) || !errors.Is(err, provisioner.ErrUnsigned) {
				t.Errorf("an unsigned %s to %s: %v", code.code, route, err)
			}
		}
	}
}

// triesOf is the tries of route's request whose every answer is status. A machine end
// has one try. Any other request sends a try with a server error or with no valid
// signature again; pass 500 for the latter.
func triesOf(route provisionertest.Route, status int) int {
	if route == "machine-end" || status < 500 {
		return 1
	}
	return 5
}

// send sends route's request for the claimed dispatch.
func send(c *provisioner.Client, route provisionertest.Route, cl provisioner.Claimed) error {
	ctx := context.Background()
	switch route {
	case "claim":
		got, _, err := c.Claim(ctx, 0, nil)
		got.RunCode.Clear()
		return err
	case "dispatched":
		return c.Dispatched(ctx, cl.DispatchID, external, "")
	case "machine-log":
		return c.MachineLog(ctx, cl.DispatchID, "a line\n", false)
	case "machine-end":
		return c.MachineEnd(ctx, cl.DispatchID, contracts.MachineEndGone)
	}
	return c.Release(ctx, cl.DispatchID, "a reason")
}

// TestAMachineEndIsSent sends each state of the contract, any number of times, after
// the dispatch ended, and the fake receives each one in order. The body passes the
// fake's check against machine-end.schema.json.
func TestAMachineEndIsSent(t *testing.T) {
	srv, c, _, _ := setup(t)
	ctx := context.Background()
	cl := claimed(t, srv, c, provisionertest.Work{})
	cl.RunCode.Clear()
	srv.End(cl.DispatchID)
	states := []string{contracts.MachineEndDeleteFailed, contracts.MachineEndDeleting, contracts.MachineEndGone, contracts.MachineEndGone}
	for _, state := range states {
		if err := c.MachineEnd(ctx, cl.DispatchID, state); err != nil {
			t.Fatalf("%s: %v", state, err)
		}
	}
	if d, _ := srv.Dispatch(cl.DispatchID); !slices.Equal(d.MachineEnds, states) {
		t.Errorf("the machine ends are %q, want %q", d.MachineEnds, states)
	}
	if err := c.MachineEnd(ctx, "dp_0000000000000000", contracts.MachineEndGone); !errors.Is(err, provisioner.ErrForbidden) {
		t.Errorf("an unknown dispatch: %v", err)
	}
	if err := c.MachineEnd(ctx, cl.DispatchID, "stopped"); err == nil || !strings.HasPrefix(err.Error(), "report this defect in the provisioner: ") {
		t.Errorf("a state outside the schema: %v", err)
	}
}

// TestASignedRefusalStopsTheMachineEnds matches ErrMachineEndStopped for a machine end's
// signed 400, 403 and 404, with any code, after one try. No unsigned answer matches it,
// an unsigned 404 among them, and no other status, and no other request.
func TestASignedRefusalStopsTheMachineEnds(t *testing.T) {
	for _, c := range []struct {
		reply provisionertest.Reply
		want  bool
	}{
		{provisionertest.Reply{Status: 400, Code: "invalid_request"}, true},
		{provisionertest.Reply{Status: 403, Code: "forbidden"}, true},
		{provisionertest.Reply{Status: 404, Code: "not_found"}, true},
		{provisionertest.Reply{Status: 403, Code: "other"}, true},
		{provisionertest.Reply{Status: 404, Code: "not_found", Unsigned: true}, false},
		{provisionertest.Reply{Status: 403, Code: "forbidden", Unsigned: true}, false},
		{provisionertest.Reply{Status: 401, Code: "unauthorized", Unsigned: true}, false},
		{provisionertest.Reply{Status: 429, Code: "rate_limited"}, false},
		{provisionertest.Reply{Status: 503, Code: "unavailable"}, false},
		{provisionertest.Reply{Status: 410, Code: "gone"}, false},
		{provisionertest.Reply{Status: 409, Code: "lease_expired"}, false},
	} {
		srv, cl, _, _ := setup(t)
		got := claimed(t, srv, cl, provisionertest.Work{})
		got.RunCode.Clear()
		srv.Script("machine-end", c.reply)
		err := cl.MachineEnd(context.Background(), got.DispatchID, contracts.MachineEndGone)
		if err == nil || errors.Is(err, provisioner.ErrMachineEndStopped) != c.want {
			t.Errorf("%d %s, unsigned %t: %v, want a match %t", c.reply.Status, c.reply.Code, c.reply.Unsigned, err, c.want)
		}
		srv.Script("release", provisionertest.Reply{Status: 403, Code: "forbidden"})
		if err := cl.Release(context.Background(), got.DispatchID, "a reason"); errors.Is(err, provisioner.ErrMachineEndStopped) {
			t.Errorf("a release's 403 matches: %v", err)
		}
	}
}

// TestAMachineEndHasOneTry sends one request for each failure that another request
// sends again.
func TestAMachineEndHasOneTry(t *testing.T) {
	for _, rep := range []provisionertest.Reply{
		{Status: 404, Code: "not_found", Unsigned: true},
		{Status: 401, Unsigned: true},
		{Status: 503, Code: "unavailable"},
		{Status: 204, Drop: true},
	} {
		srv, c, _, _ := setup(t)
		cl := claimed(t, srv, c, provisionertest.Work{})
		cl.RunCode.Clear()
		srv.Script("machine-end", rep)
		if err := c.MachineEnd(context.Background(), cl.DispatchID, contracts.MachineEndGone); err == nil {
			t.Errorf("%+v: no error", rep)
		}
		n := 0
		for _, call := range srv.Calls() {
			if call.Route == "machine-end" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%+v: %d tries", rep, n)
		}
	}
}

// TestTheModelsRefusals walks the fake's dispatches through the contract's refusals,
// each read as its error: a release after dispatched, dispatched and a release after
// the end, a second machine log, a stale dispatch, a redeemed run code and an unknown
// dispatch.
func TestTheModelsRefusals(t *testing.T) {
	srv, c, clk, _ := setup(t)
	ctx := context.Background()
	cl := dispatched(t, srv, c, provisionertest.Work{})
	cl.RunCode.Clear()
	if err := c.Release(ctx, cl.DispatchID, "a reason"); !errors.Is(err, provisioner.ErrMachineExists) {
		t.Errorf("a release after dispatched: %v", err)
	}
	if err := c.Dispatched(ctx, cl.DispatchID, external, ""); err != nil {
		t.Errorf("dispatched again: %v", err)
	}
	srv.End(cl.DispatchID)
	if err := c.Dispatched(ctx, cl.DispatchID, external, ""); !errors.Is(err, provisioner.ErrGone) {
		t.Errorf("dispatched after the end: %v", err)
	}
	if err := c.Release(ctx, cl.DispatchID, "a reason again"); !errors.Is(err, provisioner.ErrGone) {
		t.Errorf("a release after the end: %v", err)
	}
	if err := c.MachineLog(ctx, cl.DispatchID, "a line\n", false); err != nil {
		t.Errorf("a machine log after the end: %v", err)
	}
	if err := c.MachineLog(ctx, cl.DispatchID, "another line\n", false); !errors.Is(err, provisioner.ErrMachineLogExists) || errors.Is(err, provisioner.ErrGone) {
		t.Errorf("a second machine log: %v", err)
	}

	stale := claimed(t, srv, c, provisionertest.Work{Lease: 30 * time.Second})
	stale.RunCode.Clear()
	clk.set(clk.read().Add(30 * time.Second))
	if err := c.Dispatched(ctx, stale.DispatchID, external, ""); !errors.Is(err, provisioner.ErrLeaseExpired) {
		t.Errorf("a first dispatched at the lease's end: %v", err)
	}
	if err := c.Release(ctx, stale.DispatchID, "a reason"); !errors.Is(err, provisioner.ErrLeaseExpired) {
		t.Errorf("a release after a lapsed lease: %v", err)
	}
	if err := c.MachineLog(ctx, stale.DispatchID, "a line\n", false); err != nil {
		t.Errorf("a machine log after a lapsed lease: %v", err)
	}
	expired := claimed(t, srv, c, provisionertest.Work{})
	expired.RunCode.Clear()
	srv.Expire(expired.DispatchID)
	if err := c.Dispatched(ctx, expired.DispatchID, nil, ""); !errors.Is(err, provisioner.ErrLeaseExpired) {
		t.Errorf("dispatched of a stale dispatch: %v", err)
	}
	redeemed := claimed(t, srv, c, provisionertest.Work{})
	redeemed.RunCode.Clear()
	if !srv.Redeem(redeemed.DispatchID) {
		t.Fatal("the fake refused the redemption")
	}
	if err := c.Release(ctx, redeemed.DispatchID, "a reason"); !errors.Is(err, provisioner.ErrMachineExists) {
		t.Errorf("a release after the run code was redeemed: %v", err)
	}
	if err := c.Release(ctx, "dp_0000000000000000", "a reason"); !errors.Is(err, provisioner.ErrForbidden) {
		t.Errorf("an unknown dispatch: %v", err)
	}
	if err := c.MachineLog(ctx, "dp_0000000000000000", "a line\n", false); !errors.Is(err, provisioner.ErrForbidden) {
		t.Errorf("a machine log of an unknown dispatch: %v", err)
	}
}

// TestARedeemedDispatchNeverGoesStale redeems a dispatch's run code before its lease
// ends, then moves the clock past the lease: a release is 409 machine_exists, and the
// first dispatched and each one after it get the 204. Expire leaves the dispatch as it
// is, and a release then is 409 machine_exists again.
func TestARedeemedDispatchNeverGoesStale(t *testing.T) {
	srv, c, clk, _ := setup(t)
	ctx := context.Background()
	cl := claimed(t, srv, c, provisionertest.Work{Lease: 30 * time.Second})
	cl.RunCode.Clear()
	if !srv.Redeem(cl.DispatchID) {
		t.Fatal("the fake refused the redemption")
	}
	clk.set(clk.read().Add(31 * time.Second))
	if err := c.Release(ctx, cl.DispatchID, "a reason"); !errors.Is(err, provisioner.ErrMachineExists) || errors.Is(err, provisioner.ErrLeaseExpired) {
		t.Errorf("a release after the lease's end: %v", err)
	}
	for range 2 {
		if err := c.Dispatched(ctx, cl.DispatchID, external, ""); err != nil {
			t.Errorf("dispatched after the lease's end: %v", err)
		}
	}
	srv.Expire(cl.DispatchID)
	if err := c.Release(ctx, cl.DispatchID, "a reason again"); !errors.Is(err, provisioner.ErrMachineExists) {
		t.Errorf("a release after Expire: %v", err)
	}
	if d, _ := srv.Dispatch(cl.DispatchID); d.Stale || d.State != provisionertest.StateDispatched || d.Dispatched != 2 {
		t.Errorf("the dispatch %+v", d)
	}
}

// TestExpireLeavesARedeemedDispatch redeems a dispatch's run code, with no dispatched,
// then calls Expire: the dispatch is not stale, a release is 409 machine_exists, and
// the first dispatched gets the 204.
func TestExpireLeavesARedeemedDispatch(t *testing.T) {
	srv, c, _, _ := setup(t)
	ctx := context.Background()
	cl := claimed(t, srv, c, provisionertest.Work{})
	cl.RunCode.Clear()
	if !srv.Redeem(cl.DispatchID) {
		t.Fatal("the fake refused the redemption")
	}
	srv.Expire(cl.DispatchID)
	if d, _ := srv.Dispatch(cl.DispatchID); d.Stale || !d.Redeemed || d.State != provisionertest.StateClaimed {
		t.Errorf("after Expire: %+v", d)
	}
	if err := c.Release(ctx, cl.DispatchID, "a reason"); !errors.Is(err, provisioner.ErrMachineExists) {
		t.Errorf("a release: %v", err)
	}
	if err := c.Dispatched(ctx, cl.DispatchID, external, ""); err != nil {
		t.Errorf("the first dispatched: %v", err)
	}
	if !srv.Redeem(cl.DispatchID) {
		t.Error("the fake refused a repeat redemption")
	}
}

// TestARedemptionAfterTheLeaseIsRefused moves the clock to a dispatch's lease_until with
// no dispatched and no redemption: the fake refuses the redemption, and a release and
// a first dispatched are 409 lease_expired.
func TestARedemptionAfterTheLeaseIsRefused(t *testing.T) {
	srv, c, clk, _ := setup(t)
	ctx := context.Background()
	cl := claimed(t, srv, c, provisionertest.Work{Lease: 30 * time.Second})
	cl.RunCode.Clear()
	clk.set(clk.read().Add(30 * time.Second))
	if srv.Redeem(cl.DispatchID) {
		t.Error("the fake took a redemption at lease_until")
	}
	if err := c.Release(ctx, cl.DispatchID, "a reason"); !errors.Is(err, provisioner.ErrLeaseExpired) {
		t.Errorf("a release: %v", err)
	}
	if err := c.Dispatched(ctx, cl.DispatchID, external, ""); !errors.Is(err, provisioner.ErrLeaseExpired) {
		t.Errorf("a first dispatched: %v", err)
	}
}

// TestARetryAfterIsBounded reads a signed 429's Retry-After from a second to an hour,
// in seconds or as a date, on a claim and a release.
func TestARetryAfterIsBounded(t *testing.T) {
	for _, route := range []provisionertest.Route{"claim", "release"} {
		for _, rc := range []struct {
			header string
			want   time.Duration
		}{
			{"7", 7 * time.Second}, {"0", time.Second}, {"999999", time.Hour}, {"soon", time.Second},
			{testTime.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		} {
			srv, c, _, _ := setup(t)
			cl := claimed(t, srv, c, provisionertest.Work{})
			cl.RunCode.Clear()
			srv.Script(route, provisionertest.Reply{Status: 429, Code: "rate_limited", RetryAfter: rc.header})
			err := send(c, route, cl)
			var r *provisioner.Refusal
			if !errors.Is(err, provisioner.ErrRateLimited) || !errors.As(err, &r) || r.RetryAfter != rc.want || !strings.Contains(err.Error(), "with a wait of") {
				t.Errorf("%s, Retry-After %s: %v", route, rc.header, err)
			}
		}
	}
}

// TestAnAnswerThatBreaksTheContractIsNotUsed sends a claim signed 200s that break the
// contract: another revision, a member the schema does not define, a run code outside
// its form or missing, a gateway URL that contains a run code's prefix, a member twice;
// and dispatched, a release and a machine log a signed 200. Each is ErrAnswerUnusable
// after one try, and its text quotes no value.
func TestAnAnswerThatBreaksTheContractIsNotUsed(t *testing.T) {
	edits := map[string]func(map[string]any){
		"revision 0":                 func(d map[string]any) { d["revision"] = 0 },
		"the next revision":          func(d map[string]any) { d["revision"] = provisioner.Revision + 1 },
		"a revision after the next":  func(d map[string]any) { d["revision"] = provisioner.Revision + 2 },
		"a new member":               func(d map[string]any) { d["extra"] = "the title of the ticket" },
		"a run id":                   func(d map[string]any) { d["run_id"] = "0192f0c1-7d4e-7a2b-8c3d-4e5f6a7b8c9d" },
		"a run code's shape":         func(d map[string]any) { d["run_code"] = d["run_code"].(string) + "!" },
		"no run code":                func(d map[string]any) { delete(d, "run_code") },
		"a number as run code":       func(d map[string]any) { d["run_code"] = 7 },
		"no gateway URL":             func(d map[string]any) { delete(d, "gateway_url") },
		"a gateway URL with qecr_":   func(d map[string]any) { d["gateway_url"] = "https://qecr_ticket.example.com" },
		"a lifetime of 14 minutes":   func(d map[string]any) { d["max_lifetime_minutes"] = 14 },
		"a lifetime with a fraction": func(d map[string]any) { d["max_lifetime_minutes"] = 15.5 },
		"an empty machine":           func(d map[string]any) { d["machine"] = "" },
	}
	for name, edit := range edits {
		srv, c, _, log := setup(t)
		srv.Offer(provisionertest.Work{})
		srv.Script(provisionertest.RouteClaim, provisionertest.Reply{Edit: edit})
		cl, ok, err := c.Claim(context.Background(), provisioner.MaxWaitSeconds, nil)
		if !errors.Is(err, provisioner.ErrAnswerUnusable) || ok || cl.RunCode.Len() != 0 || len(srv.Calls()) != 1 {
			t.Errorf("a claim with %s: %v", name, err)
			continue
		}
		if text := strings.ToLower(err.Error() + log.String()); strings.Contains(text, "ticket") || strings.Contains(text, "qecr_") {
			t.Errorf("a claim with %s: the error or the log quotes a value: %v", name, err)
		}
	}
	twice := string(fixture(t, "claim-answer/machine.json"))
	twice = strings.Replace(twice, `"version": 1,`, `"version": 1, "version": 1,`, 1)
	srv, c, _, _ := setup(t)
	srv.Script(provisionertest.RouteClaim, provisionertest.Reply{Status: 200, Body: twice})
	if _, _, err := c.Claim(context.Background(), provisioner.MaxWaitSeconds, nil); !errors.Is(err, provisioner.ErrAnswerUnusable) {
		t.Errorf("a member twice: %v", err)
	}

	for _, route := range []provisionertest.Route{"dispatched", "release", "machine-log", "machine-end"} {
		srv, c, _, _ = setup(t)
		cl := claimed(t, srv, c, provisionertest.Work{})
		cl.RunCode.Clear()
		srv.Script(route, provisionertest.Reply{Status: 200, Body: `{}`})
		if err := send(c, route, cl); !errors.Is(err, provisioner.ErrAnswerUnusable) {
			t.Errorf("a 200 to %s: %v", route, err)
		}
	}
}

// TestAnswersAreReadUpToTheirBound sends a claim's 200 one byte over its bound, and
// just within it, and a refusal at its bound: over is ErrTooLarge, never read as
// signed; within is used.
func TestAnswersAreReadUpToTheirBound(t *testing.T) {
	pad := func(body string, limit int) int { return limit - len(body) }
	srv, c, _, _ := setup(t)
	claim := string(fixture(t, "claim-answer/machine.json"))
	for range 5 {
		srv.Script(provisionertest.RouteClaim, provisionertest.Reply{Status: 200, Body: claim, Pad: pad(claim, provisioner.MaxClaimAnswer) + 1})
	}
	if _, _, err := c.Claim(context.Background(), provisioner.MaxWaitSeconds, nil); !errors.Is(err, provisioner.ErrTooLarge) || errors.Is(err, provisioner.ErrUnsigned) {
		t.Errorf("a claim's 200 over its bound: %v", err)
	}
	srv.Script(provisionertest.RouteClaim, provisionertest.Reply{Status: 200, Body: claim, Pad: pad(claim, provisioner.MaxClaimAnswer)})
	cl, ok, err := c.Claim(context.Background(), provisioner.MaxWaitSeconds, nil)
	if err != nil || !ok || cl.RunCode.Len() == 0 {
		t.Errorf("a claim's 200 at its bound: %v", err)
	}
	cl.RunCode.Clear()
	refusal := `{"error":"gone"}`
	srv.Script(provisionertest.RouteRelease, provisionertest.Reply{Status: 410, Body: refusal, Pad: pad(refusal, provisioner.MaxAnswer)})
	if err := c.Release(context.Background(), cl.DispatchID, "a reason"); !errors.Is(err, provisioner.ErrGone) {
		t.Errorf("a refusal at its bound: %v", err)
	}
}

// TestARequestThatBreaksItsSchemaIsNotSent stops, before anything is sent, a release
// whose reason contains a run key, a run code or an access key secret, is empty or too
// long; a dispatched whose external breaks its schema or contains a secret; and a
// dispatch id outside its form. No error quotes the secret or the id.
func TestARequestThatBreaksItsSchemaIsNotSent(t *testing.T) {
	srv, c, _, _ := setup(t)
	ctx := context.Background()
	const dp = "dp_0123456789abcdef"
	ext := func(edit func(e *contracts.External)) *contracts.External {
		e := *external
		edit(&e)
		return &e
	}
	for name, err := range map[string]error{
		"a run key":                   c.Release(ctx, dp, "the key is qrk_abc.def"),
		"a run code":                  c.Release(ctx, dp, "the code is QECR_abc"),
		"a secret":                    c.Release(ctx, dp, "QAK_abcdef"),
		"an empty reason":             c.Release(ctx, dp, ""),
		"a long reason":               c.Release(ctx, dp, strings.Repeat("a", 201)),
		"a NUL":                       c.Release(ctx, dp, "a\x00b"),
		"a dispatch id":               c.Dispatched(ctx, "dp_../../v1/pools/claim", nil, ""),
		"a secret as id":              c.Release(ctx, "qrk_abc.def", "a reason"),
		"a machine log's id":          c.MachineLog(ctx, "dp_../x", "a line", false),
		"an empty ref":                c.Dispatched(ctx, dp, ext(func(e *contracts.External) { e.Ref = "" }), ""),
		"a ref of 201":                c.Dispatched(ctx, dp, ext(func(e *contracts.External) { e.Ref = strings.Repeat("i", 201) }), ""),
		"a type in upper case":        c.Dispatched(ctx, dp, ext(func(e *contracts.External) { e.Type = "Example-CI" }), ""),
		"an http URL":                 c.Dispatched(ctx, dp, ext(func(e *contracts.External) { e.URL = "http://ci.example.com/runs/1" }), ""),
		"a run code as ref":           c.Dispatched(ctx, dp, ext(func(e *contracts.External) { e.Ref = "qecr_abc" }), ""),
		"a machine type with a space": c.Dispatched(ctx, dp, nil, "t3 medium"),
		"a machine type of 65":        c.Dispatched(ctx, dp, nil, strings.Repeat("t", 65)),
		"a run code as type":          c.Dispatched(ctx, dp, nil, "qecr_abc"),
		"65 offers":                   claimErr(c.Claim(ctx, 0, make([]contracts.MachineSpec, 65))),
		"an offer of 0 vCPUs":         claimErr(c.Claim(ctx, 0, []contracts.MachineSpec{{MemoryMiB: 4096, DiskGiB: 30, Arch: "amd64"}})),
		"an offer of x86_64":          claimErr(c.Claim(ctx, 0, []contracts.MachineSpec{{VCPUs: 2, MemoryMiB: 4096, DiskGiB: 30, Arch: "x86_64"}})),
	} {
		if err == nil || accesskey.ContainsSecret(err.Error()) || strings.Contains(strings.ToLower(err.Error()), "qecr_") || strings.Contains(err.Error(), "../") {
			t.Errorf("%s: %v", name, err)
		}
	}
	srv.Script(provisionertest.RouteRelease, provisionertest.Reply{Status: http.StatusNoContent})
	if err := c.Release(ctx, dp, strings.Repeat("é", 200)); err != nil {
		t.Errorf("200 characters of two bytes each: %v", err)
	}
	if n := len(srv.Calls()); n != 1 {
		t.Errorf("%d requests sent", n)
	}
}

// claimErr is the error of a claim, without its answer.
func claimErr(_ provisioner.Claimed, _ bool, err error) error { return err }

// machineLogOf is the log the fake recorded for the dispatch, and the bytes sent.
func machineLogOf(t *testing.T, srv *provisionertest.Server, dispatchID string) (contracts.MachineLog, []byte) {
	t.Helper()
	d, _ := srv.Dispatch(dispatchID)
	if d.MachineLog == nil {
		t.Fatal("no machine log")
	}
	return *d.MachineLog, last(t, srv).Body
}

// TestAMachineLogIsCutToItsBounds uploads logs over each bound: 800 KiB of plain lines,
// over MaxLog; 700 KiB of characters that JSON writes as 6 bytes each, over the
// request's mebibyte once escaped; and one line with no line feed, whose cut at MaxLog
// bytes falls inside a 2-byte character. Each sent log is the end of the log, starts at
// a line when it has one and at a whole character otherwise, is valid UTF-8, is within
// both bounds, and is marked truncated. A log within its bounds is sent whole, truncated
// as the caller set it.
func TestAMachineLogIsCutToItsBounds(t *testing.T) {
	line := "2026-10-15T12:01:05Z machine step done\n"
	plain := strings.Repeat(line, 800<<10/len(line)+1)
	escaped := strings.Repeat("<<<<<<<<<\n", 70<<10)
	// The last character is 1 byte and every other 2, so a cut at MaxLog bytes from the
	// end, an even number, falls inside a character.
	oneLine := strings.Repeat(string(rune(0xe9)), 500<<10) + "x"
	for name, log := range map[string]string{"plain": plain, "escaped": escaped, "one line": oneLine} {
		srv, c, _, _ := setup(t)
		cl := claimed(t, srv, c, provisionertest.Work{})
		cl.RunCode.Clear()
		if err := c.MachineLog(context.Background(), cl.DispatchID, log, false); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, body := machineLogOf(t, srv, cl.DispatchID)
		switch {
		case !got.Truncated || !strings.HasSuffix(log, got.Log) || len(got.Log) > provisioner.MaxLog || len(body) > contracts.MaxRequest ||
			!utf8.ValidString(got.Log) || !utf8.Valid(body) || strings.ContainsRune(got.Log, utf8.RuneError):
			t.Errorf("%s: truncated %v, %d bytes of log, a body of %d bytes", name, got.Truncated, len(got.Log), len(body))
		case name != "one line" && !strings.HasSuffix(log[:len(log)-len(got.Log)], "\n"):
			t.Errorf("%s: the log does not start at a line", name)
		case len(got.Log) < provisioner.MaxLog/2 && name != "escaped":
			t.Errorf("%s: only %d bytes kept", name, len(got.Log))
		}
	}
	for _, truncated := range []bool{false, true} {
		srv, c, _, _ := setup(t)
		cl := claimed(t, srv, c, provisionertest.Work{})
		cl.RunCode.Clear()
		if err := c.MachineLog(context.Background(), cl.DispatchID, "a line\n", truncated); err != nil {
			t.Fatal(err)
		}
		got, body := machineLogOf(t, srv, cl.DispatchID)
		if got.Log != "a line\n" || got.Truncated != truncated || strings.Contains(string(body), "truncated") != truncated {
			t.Errorf("truncated %v: sent %s", truncated, body)
		}
	}
}

// TestAMachineLogIsCleaned uploads a log with lines that contain an access key secret,
// a run key and a run code in any case, a U+0000 and a run of 3 bytes that are not
// UTF-8: each such line is replaced with a fixed text, the U+0000 with U+FFFD, and the
// run of bytes with one U+FFFD.
func TestAMachineLogIsCleaned(t *testing.T) {
	srv, c, _, _ := setup(t)
	cl := claimed(t, srv, c, provisionertest.Work{})
	cl.RunCode.Clear()
	log := "start\nkey qak_abc\nrun key QRK_abc.def\ncode qecr_ABC\na\x00b\nc\xff\xfe\xfdd\nend"
	if err := c.MachineLog(context.Background(), cl.DispatchID, log, false); err != nil {
		t.Fatal(err)
	}
	got, _ := machineLogOf(t, srv, cl.DispatchID)
	hidden := "[hidden: the line contained what looks like an access key secret, a run key or a run code]\n"
	r := string(utf8.RuneError)
	if want := "start\n" + hidden + hidden + hidden + "a" + r + "b\nc" + r + "d\nend"; got.Log != want || got.Truncated {
		t.Errorf("sent %q, want %q", got.Log, want)
	}
}

// TestNoRedirectAndNoCookie sends a release a signed 307 to another server and a claim
// a cookie, through an HTTP client with a cookie jar and a redirect policy that
// follows: the 307 is a refusal, nothing reaches the other server, and the next
// request carries no cookie.
func TestNoRedirectAndNoCookie(t *testing.T) {
	var reached atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	t.Cleanup(elsewhere.Close)
	srv, c, _, _ := setup(t)
	jar, _ := cookiejar.New(nil)
	c.HTTP = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	srv.Offer(provisionertest.Work{})
	srv.Script(provisionertest.RouteClaim, provisionertest.Reply{Header: http.Header{"Set-Cookie": {"session=abc; Path=/"}}})
	cl, _, err := c.Claim(context.Background(), provisioner.MaxWaitSeconds, nil)
	if err != nil {
		t.Fatal(err)
	}
	cl.RunCode.Clear()
	srv.Script(provisionertest.RouteRelease, provisionertest.Reply{Status: 307, Header: http.Header{"Location": {elsewhere.URL + "/v1/dispatches/" + cl.DispatchID + "/release"}}})
	var r *provisioner.Refusal
	if err := c.Release(context.Background(), cl.DispatchID, "a reason"); !errors.As(err, &r) || r.Status != 307 {
		t.Errorf("a 307: %v", err)
	}
	if reached.Load() != 0 {
		t.Error("the client followed the redirect")
	}
	if h := last(t, srv).Header; h.Get("Cookie") != "" {
		t.Errorf("a cookie was sent: %s", h.Get("Cookie"))
	}
}

// TestAClaimIsHeldAndCancelled claims with nothing queued: the fake holds the claim,
// then sends the signed 204, no work; work offered while it is held is claimed at
// once; a cancelled context ends a held claim with its error and nothing logged.
func TestAClaimIsHeldAndCancelled(t *testing.T) {
	srv, c, _, log := setup(t)
	srv.Hold = 50 * time.Millisecond
	if _, ok, err := c.Claim(context.Background(), provisioner.MaxWaitSeconds, nil); err != nil || ok {
		t.Errorf("an empty claim: %v %v", ok, err)
	}
	srv.Hold = 10 * time.Second
	go func() {
		time.Sleep(50 * time.Millisecond)
		srv.Offer(provisionertest.Work{})
	}()
	if _, ok, err := c.Claim(context.Background(), provisioner.MaxWaitSeconds, nil); err != nil || !ok {
		t.Errorf("work offered while held: %v %v", ok, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if _, _, err := c.Claim(ctx, provisioner.MaxWaitSeconds, nil); !errors.Is(err, context.Canceled) || time.Since(start) > 5*time.Second {
		t.Errorf("a cancelled claim: %v after %s", err, time.Since(start))
	}
	if log.Len() != 0 {
		t.Errorf("logged:\n%s", log)
	}
}

// TestA401SaysWhatToCheck sends a 401 to every try of a claim: the error starts with
// what to check, and states that a 401 is not a revoked key.
func TestA401SaysWhatToCheck(t *testing.T) {
	srv, c, _, _ := setup(t)
	unauthorized := provisionertest.Reply{Status: 401, Code: "unauthorized", Unsigned: true}
	srv.Script(provisionertest.RouteClaim, unauthorized, unauthorized, unauthorized, unauthorized, unauthorized)
	_, _, err := c.Claim(context.Background(), provisioner.MaxWaitSeconds, nil)
	want := "check that the access key is approved in Qory Apiary and that this machine's clock is within 300 seconds of the correct time: " +
		"the claim returned 401 unauthorized, which the client does not take as a revoked key"
	if !errors.Is(err, provisioner.ErrUnsigned) || err.Error() != want {
		t.Errorf("err %v", err)
	}
}

// sprintf is fmt.Sprintf of one value.
func sprintf(format string, v any) string { return fmt.Sprintf(format, v) }
