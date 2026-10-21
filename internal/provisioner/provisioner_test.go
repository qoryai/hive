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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// claimed claims the run srv offers, which must be there.
func claimed(t *testing.T, srv *provisionertest.Server, c *provisioner.Client, run provisionertest.Run) provisioner.Claimed {
	t.Helper()
	srv.Offer(run)
	cl, ok, err := c.Claim(context.Background(), provisioner.MaxWaitSeconds)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	return cl
}

// handed claims the run srv offers, posts dispatched and takes its handover.
func handed(t *testing.T, srv *provisionertest.Server, c *provisioner.Client, run provisionertest.Run) (provisioner.Claimed, provisioner.Handed) {
	t.Helper()
	cl := claimed(t, srv, c, run)
	if err := c.Dispatched(context.Background(), cl.DispatchID); err != nil {
		t.Fatal(err)
	}
	h, err := c.Handover(context.Background(), cl.DispatchID, cl.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return cl, h
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
		if err == nil || c != nil || accesskey.ContainsSecret(err.Error()) || !strings.HasPrefix(err.Error(), "set ") {
			t.Errorf("%s: %v", name, err)
		}
	}
	cfg := srv.Config()
	cfg.URL = "https://apiary.example.com:8443"
	if _, err := provisioner.New(cfg); err != nil {
		t.Errorf("an https origin with a port: %v", err)
	}
}

// TestARunGoesThroughEveryRequest claims an explore run, posts dispatched, takes its
// handover, releases it, and releases a second run before its handover, each request signed with the pool's key, with the headers the contract
// lists, a body that passes its schema and the clock's time, and each answer's
// signature verified under the pin.
func TestARunGoesThroughEveryRequest(t *testing.T) {
	srv, c, clk, _ := setup(t)
	ctx := context.Background()
	cl := claimed(t, srv, c, provisionertest.Run{})
	if cl.Step != provisioner.StepExplore || cl.PoolID != provisionertest.PoolID || cl.WorkItem == nil || !cl.LeaseUntil.Equal(testTime.Add(provisionertest.Lease)) {
		t.Errorf("claimed %+v", cl)
	}
	d, _ := srv.Dispatch(cl.DispatchID)
	if cl.WorkItem.ID != d.Run.WorkItem.ID || cl.WorkItem.URL != d.Run.WorkItem.URL || written(t, cl.WorkItem.Title) != d.Run.WorkItem.Title {
		t.Errorf("the claim's work item %+v", cl.WorkItem)
	}
	if err := c.Dispatched(ctx, cl.DispatchID); err != nil {
		t.Fatal(err)
	}
	h, err := c.Handover(ctx, cl.DispatchID, cl.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if h.RunID != cl.RunID || h.Step != provisioner.StepExplore || h.Domain != d.Run.Domain || h.WorkItem == nil || !sameJSON(t, []byte(written(t, h.Inputs)), d.Run.Inputs) {
		t.Errorf("handed %+v", h)
	}
	if written(t, h.RunKey) != d.Run.RunKey || written(t, h.WorkItem.Title) != d.Run.WorkItem.Title {
		t.Error("the run key or the title is not the one handed over")
	}
	if err := c.Release(ctx, cl.DispatchID, "The machine stopped.", false); err != nil {
		t.Fatal(err)
	}
	if d, _ := srv.Dispatch(cl.DispatchID); d.State != provisionertest.StateEnded || d.Release == nil || d.Release.Retry {
		t.Errorf("after the release: %+v", d)
	}

	other := claimed(t, srv, c, provisionertest.Run{})
	if err := c.Release(ctx, other.DispatchID, "The machine could not be created.", true); err != nil {
		t.Fatal(err)
	}
	if d, _ := srv.Dispatch(other.DispatchID); d.State != provisionertest.StateEnded || d.Release == nil || !d.Release.Retry || d.Release.Reason != "The machine could not be created." {
		t.Errorf("after the release: %+v", d)
	}

	for _, call := range srv.Calls() {
		h := call.Header
		switch {
		case !call.Verified || call.Replayed || !call.Signed:
			t.Errorf("%s: verified %v, replayed %v, answer signed %v", call.Route, call.Verified, call.Replayed, call.Signed)
		case h.Get(accesskey.HeaderAccessKeyID) != provisionertest.KeyID || h.Get(accesskey.HeaderInstanceID) != provisionertest.InstanceID:
			t.Errorf("%s: the key's headers %v", call.Route, h)
		case h.Get("X-Qory-Contract-Version") != "3" || h.Get("Content-Type") != "application/json" || h.Get("User-Agent") != "qory-provisioner":
			t.Errorf("%s: the headers %v", call.Route, h)
		case call.Time.Before(testTime) || call.Time.After(clk.read()) || call.Time.Nanosecond() != 0:
			t.Errorf("%s: the time %s", call.Route, call.Time)
		}
	}
	want := []provisionertest.Route{"claim", "dispatched", "handover", "release", "claim", "release"}
	if got := srv.Routes(); !slices.Equal(got, want) {
		t.Errorf("routes %v, want %v", got, want)
	}
}

// TestTheBodiesAreTheFixtures builds each request whose fixture the contract has, at
// the fixture's time, and checks that the body sent is the fixture: the claim waits 25
// seconds and has no node pool, and dispatched has no job.
func TestTheBodiesAreTheFixtures(t *testing.T) {
	srv, c, clk, _ := setup(t)
	ctx := context.Background()
	at := func(s string) {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		clk.set(tm)
	}
	cl := claimed(t, srv, c, provisionertest.Run{})
	if b := fixture(t, "claim/without-pool-id.json"); !sameJSON(t, last(t, srv).Body, b) {
		t.Errorf("the claim\n%s\nwant\n%s", last(t, srv).Body, b)
	}
	at("2026-10-15T12:00:20Z")
	if err := c.Dispatched(ctx, cl.DispatchID); err != nil {
		t.Fatal(err)
	}
	if b := fixture(t, "dispatched/without-external.json"); !sameJSON(t, last(t, srv).Body, b) {
		t.Errorf("dispatched\n%s\nwant\n%s", last(t, srv).Body, b)
	}
	var hf contracts.Handover
	if err := json.Unmarshal(fixture(t, "handover/handover.json"), &hf); err != nil {
		t.Fatal(err)
	}
	at(hf.Time)
	h, err := c.Handover(ctx, cl.DispatchID, cl.RunID)
	if err != nil {
		t.Fatal(err)
	}
	h.RunKey.Clear()
	if b := fixture(t, "handover/handover.json"); !sameJSON(t, last(t, srv).Body, b) {
		t.Errorf("the handover\n%s\nwant\n%s", last(t, srv).Body, b)
	}
	var rf contracts.Release
	if err := json.Unmarshal(fixture(t, "release/retry.json"), &rf); err != nil {
		t.Fatal(err)
	}
	at(rf.Time)
	if err := c.Release(ctx, cl.DispatchID, rf.Reason, rf.Retry); err != nil {
		t.Fatal(err)
	}
	if b := fixture(t, "release/retry.json"); !sameJSON(t, last(t, srv).Body, b) {
		t.Errorf("the release\n%s\nwant\n%s", last(t, srv).Body, b)
	}
}

// TestAClaimSendsItsWait claims with each wait from 0 to 25 seconds: the body has the
// wait. A wait outside that range is an error, and nothing is sent.
func TestAClaimSendsItsWait(t *testing.T) {
	srv, c, _, _ := setup(t)
	srv.Hold = 0
	for _, wait := range []int{0, 1, 25} {
		if _, ok, err := c.Claim(context.Background(), wait); err != nil || ok {
			t.Fatalf("wait %d: %v %v", wait, ok, err)
		}
		var body contracts.Claim
		if err := json.Unmarshal(last(t, srv).Body, &body); err != nil || body.WaitSeconds != wait {
			t.Errorf("wait %d: the body %s", wait, last(t, srv).Body)
		}
	}
	before := len(srv.Calls())
	for _, wait := range []int{-1, 26} {
		if _, _, err := c.Claim(context.Background(), wait); err == nil || !strings.Contains(err.Error(), "set wait_seconds from 0 to 25") {
			t.Errorf("wait %d: %v", wait, err)
		}
	}
	if n := len(srv.Calls()) - before; n != 0 {
		t.Errorf("%d requests sent", n)
	}
}

// atRevision3 is an answer fixture, which has revision 3.
func atRevision3(t *testing.T, name string) string {
	t.Helper()
	doc := string(fixture(t, name))
	if !strings.Contains(doc, `"revision": 3,`) {
		t.Fatalf("the fixture %s has no revision 3", name)
	}
	return doc
}

// TestTheAnswersAreTheFixtures sends each answer fixture, signed, at revision 3, to a
// claim and a handover, and checks what the client read.
func TestTheAnswersAreTheFixtures(t *testing.T) {
	for _, step := range []string{provisioner.StepExplore, provisioner.StepPrebuild} {
		t.Run(step, func(t *testing.T) {
			srv, c, _, _ := setup(t)
			ctx := context.Background()
			srv.Script(provisionertest.RouteClaim, provisionertest.Reply{Status: http.StatusOK, Body: atRevision3(t, "claim-answer/"+step+".json")})
			cl, ok, err := c.Claim(ctx, provisioner.MaxWaitSeconds)
			if err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			var ca contracts.ClaimAnswer
			_ = json.Unmarshal(fixture(t, "claim-answer/"+step+".json"), &ca)
			if cl.DispatchID != ca.DispatchID || cl.RunID != ca.RunID || cl.PoolID != ca.PoolID || cl.Step != step ||
				cl.Access != ca.Access || cl.Target == nil || *cl.Target != *ca.Target || contracts.FormatTime(cl.LeaseUntil) != ca.LeaseUntil ||
				(cl.WorkItem == nil) != (ca.WorkItem == nil) {
				t.Errorf("claimed %+v, want %+v", cl, ca)
			}
			if cl.WorkItem != nil && (cl.WorkItem.ID != ca.WorkItem.ID || cl.WorkItem.URL != ca.WorkItem.URL || written(t, cl.WorkItem.Title) != ca.WorkItem.Title) {
				t.Errorf("the work item %+v, want %+v", cl.WorkItem, ca.WorkItem)
			}
			body := atRevision3(t, "handover-answer/"+step+".json")
			srv.Script(provisionertest.RouteHandover, provisionertest.Reply{Status: http.StatusOK, Body: body})
			h, err := c.Handover(ctx, cl.DispatchID, cl.RunID)
			if err != nil {
				t.Fatal(err)
			}
			var ha struct {
				contracts.HandoverAnswer
				Inputs json.RawMessage `json:"inputs"`
			}
			_ = json.Unmarshal([]byte(body), &ha)
			if h.RunID != ha.RunID || h.Step != step || h.Domain != ha.Domain || h.Access != ha.Access ||
				h.Target == nil || *h.Target != *ha.Target || written(t, h.Inputs) != string(ha.Inputs) || written(t, h.RunKey) != ha.RunKey {
				t.Errorf("handed %+v", h)
			}
			if !strings.Contains(body, written(t, h.Inputs)) {
				t.Error("the inputs are not the answer's bytes")
			}
		})
	}
}

// TestEveryHandoverFixtureIsRead sends each of the contract's handover answers, signed,
// to a handover: the client reads each, with its domain, its target or none, the access
// only with a target, its work item and its run settings as sent.
func TestEveryHandoverFixtureIsRead(t *testing.T) {
	names, err := fs.Glob(contracts.ProvisionerFS, "fixtures/handover-answer/*.json")
	if err != nil || len(names) < 6 {
		t.Fatalf("the fixtures %v: %v", names, err)
	}
	for _, name := range names {
		name = strings.TrimPrefix(name, "fixtures/")
		t.Run(name, func(t *testing.T) {
			body := atRevision3(t, name)
			var want contracts.HandoverAnswer
			if err := json.Unmarshal([]byte(body), &want); err != nil {
				t.Fatal(err)
			}
			srv, c, _, _ := setup(t)
			cl := claimed(t, srv, c, provisionertest.Run{RunID: want.RunID, Step: want.Step})
			_ = c.Dispatched(context.Background(), cl.DispatchID)
			srv.Script(provisionertest.RouteHandover, provisionertest.Reply{Status: http.StatusOK, Body: body})
			h, err := c.Handover(context.Background(), cl.DispatchID, cl.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if h.Domain != want.Domain || h.Access != want.Access || (h.Target == nil) != (want.Target == nil) ||
				h.Target != nil && *h.Target != *want.Target || (h.Target == nil) != (h.Access == "") || (h.WorkItem == nil) != (want.WorkItem == nil) {
				t.Errorf("handed %+v", h)
			}
			if h.WorkItem != nil && written(t, h.WorkItem.Title) != want.WorkItem.Title {
				t.Error("the title is not the answer's")
			}
			got, _ := json.Marshal(h.RunSettings)
			sent, _ := json.Marshal(want.RunSettings)
			if string(got) != string(sent) {
				t.Errorf("the run settings are %s, want %s", got, sent)
			}
		})
	}
}

// TestTheFakeOffersARunWithNoTarget claims and hands over a run the fake offers with no
// target and with run settings: neither answer has a target or an access, and the
// handover has the fixture's domain and the run settings offered.
func TestTheFakeOffersARunWithNoTarget(t *testing.T) {
	srv, c, _, _ := setup(t)
	rs := &contracts.RunSettings{
		Runtime: "claude", Model: "example-large", Effort: "high",
		ProviderModels: []contracts.ProviderModel{{ID: "example-large", Effort: []string{"high"}}},
		Tiers:          &contracts.Tiers{Light: &contracts.Tier{Model: "example-large"}},
	}
	cl, h := handed(t, srv, c, provisionertest.Run{NoTarget: true, RunSettings: rs})
	if cl.Target != nil || cl.Access != "" {
		t.Errorf("claimed %+v", cl)
	}
	got, _ := json.Marshal(h.RunSettings)
	sent, _ := json.Marshal(rs)
	if h.Target != nil || h.Access != "" || h.Domain != "marketing" || string(got) != string(sent) {
		t.Errorf("handed %+v, run settings %s", h, got)
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
	{"an unsigned 503", provisionertest.Reply{Status: 503, Code: "unavailable", Unsigned: true}, provisioner.ErrUnsigned},
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
}

// TestAFailedTryIsBuiltAnewAndSentAgain sends each failure to every try of a release:
// the client sends five, each with a later time and a new signature after a wait
// doubling from a second, logs a line with fixed text before each new one, and returns
// the failure. A failure then a signed 204 is the release done.
func TestAFailedTryIsBuiltAnewAndSentAgain(t *testing.T) {
	for _, f := range failing {
		t.Run(f.name, func(t *testing.T) {
			srv, c, clk, log := setup(t)
			cl := claimed(t, srv, c, provisionertest.Run{})
			for range 5 {
				srv.Script(provisionertest.RouteRelease, f.reply)
			}
			err := c.Release(context.Background(), cl.DispatchID, "The provisioner stopped before the run started.", true)
			if f.is == nil {
				var r *provisioner.Refusal
				if !errors.As(err, &r) || !r.Signed || r.Status != 500 || !r.Resent {
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
			cl2 := claimed(t, srv2, c2, provisionertest.Run{})
			srv2.Script(provisionertest.RouteRelease, f.reply)
			if err := c2.Release(context.Background(), cl2.DispatchID, "The provisioner stopped before the run started.", true); err != nil {
				t.Errorf("a failure then a 204: %v", err)
			}
		})
	}
}

// TestASignedAnswerIsNotSentAgain sends each signed refusal below 500 to a release and
// the signed 503 unavailable to a handover: one try each, returned as it is.
func TestASignedAnswerIsNotSentAgain(t *testing.T) {
	srv, c, _, _ := setup(t)
	cl := claimed(t, srv, c, provisionertest.Run{})
	for _, rep := range []provisionertest.Reply{{Status: 403, Code: "forbidden"}, {Status: 400, Code: "invalid_request"}, {Status: 429, Code: "rate_limited"}, {Status: 413, Code: "payload_too_large"}} {
		srv.Script(provisionertest.RouteRelease, rep)
		before := len(srv.Calls())
		var r *provisioner.Refusal
		if err := c.Release(context.Background(), cl.DispatchID, "a reason", false); !errors.As(err, &r) || !r.Signed || r.Status != rep.Status || r.Code != rep.Code || r.Resent {
			t.Errorf("%d: %v", rep.Status, err)
		}
		if n := len(srv.Calls()) - before; n != 1 {
			t.Errorf("%d: %d tries", rep.Status, n)
		}
	}
	if err := c.Dispatched(context.Background(), cl.DispatchID); err != nil {
		t.Fatal(err)
	}
	srv.Script(provisionertest.RouteHandover, provisionertest.Reply{Status: 503, Code: "unavailable"})
	before := len(srv.Calls())
	if _, err := c.Handover(context.Background(), cl.DispatchID, cl.RunID); !errors.Is(err, provisioner.ErrUnavailable) {
		t.Errorf("503: %v", err)
	}
	if n := len(srv.Calls()) - before; n != 1 {
		t.Errorf("503: %d tries", n)
	}
	if d, _ := srv.Dispatch(cl.DispatchID); d.State != provisionertest.StateDispatched {
		t.Errorf("after the 503: %s", d.State)
	}
}

// TestAHandoverAfterALostAnswerIsResent drops the 200 of a handover after the fake
// handed the run over: the try sent again gets 409 handed_over, with Resent set.
func TestAHandoverAfterALostAnswerIsResent(t *testing.T) {
	srv, c, _, _ := setup(t)
	cl := claimed(t, srv, c, provisionertest.Run{})
	if err := c.Dispatched(context.Background(), cl.DispatchID); err != nil {
		t.Fatal(err)
	}
	srv.Script(provisionertest.RouteHandover, provisionertest.Reply{Drop: true})
	_, err := c.Handover(context.Background(), cl.DispatchID, cl.RunID)
	var r *provisioner.Refusal
	if !errors.Is(err, provisioner.ErrHandedOver) || !errors.As(err, &r) || !r.Resent {
		t.Errorf("err %v", err)
	}
	if d, _ := srv.Dispatch(cl.DispatchID); !d.HandedOver {
		t.Error("the fake did not hand the run over")
	}
}

// TestTheSameBytesAreNeverSentTwice sends one release twice within one second of the
// clock: the second waits for the next second, so its time and its bytes differ. A
// distinct request in the same second does not wait.
func TestTheSameBytesAreNeverSentTwice(t *testing.T) {
	srv, c, clk, _ := setup(t)
	clk.set(testTime.Add(300 * time.Millisecond))
	cl := claimed(t, srv, c, provisionertest.Run{})
	srv.Script(provisionertest.RouteRelease, provisionertest.Reply{Status: 204}, provisionertest.Reply{Status: 204}, provisionertest.Reply{Status: 204})
	ctx := context.Background()
	if err := c.Release(ctx, cl.DispatchID, "a reason", true); err != nil {
		t.Fatal(err)
	}
	if err := c.Release(ctx, cl.DispatchID, "another reason", true); err != nil {
		t.Fatal(err)
	}
	if w := clk.slept(); len(w) != 0 {
		t.Errorf("a distinct request waited %v", w)
	}
	if err := c.Release(ctx, cl.DispatchID, "a reason", true); err != nil {
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
	all := []provisionertest.Route{"claim", "dispatched", "handover", "release"}
	codes := []struct {
		err    error
		status int
		code   string
		on     []provisionertest.Route
	}{
		{provisioner.ErrForbidden, 403, "forbidden", all},
		{provisioner.ErrNotFound, 404, "not_found", all},
		{provisioner.ErrGone, 410, "gone", []provisionertest.Route{"dispatched", "handover", "release"}},
		{provisioner.ErrHandedOver, 409, "handed_over", []provisionertest.Route{"handover"}},
		{provisioner.ErrLeaseExpired, 409, "lease_expired", []provisionertest.Route{"dispatched", "handover", "release"}},
		{provisioner.ErrNotDispatched, 409, "not_dispatched", []provisionertest.Route{"handover"}},
		{provisioner.ErrNoGateway, 409, "no_gateway", []provisionertest.Route{"claim", "handover"}},
		{provisioner.ErrUnavailable, 503, "unavailable", []provisionertest.Route{"handover"}},
		{provisioner.ErrRateLimited, 429, "rate_limited", all},
	}
	for _, code := range codes {
		for _, route := range all {
			srv, c, _, _ := setup(t)
			cl, h := handed(t, srv, c, provisionertest.Run{})
			h.RunKey.Clear()
			srv.Script(route, provisionertest.Reply{Status: code.status, Code: code.code})
			err := send(c, route, cl)
			if want := slices.Contains(code.on, route); errors.Is(err, code.err) != want {
				t.Errorf("%s to %s: %v, matches %v, want %v", code.code, route, err, !want, want)
			}
			if code.status >= 500 && route != "handover" {
				continue
			}
			unsigned := provisionertest.Reply{Status: code.status, Code: code.code, Unsigned: true}
			srv.Script(route, unsigned, unsigned, unsigned, unsigned, unsigned)
			if err := send(c, route, cl); errors.Is(err, code.err) || !errors.Is(err, provisioner.ErrUnsigned) {
				t.Errorf("an unsigned %s to %s: %v", code.code, route, err)
			}
		}
	}
}

// send sends route's request for the claimed run.
func send(c *provisioner.Client, route provisionertest.Route, cl provisioner.Claimed) error {
	ctx := context.Background()
	switch route {
	case "claim":
		_, _, err := c.Claim(ctx, 0)
		return err
	case "dispatched":
		return c.Dispatched(ctx, cl.DispatchID)
	case "handover":
		h, err := c.Handover(ctx, cl.DispatchID, cl.RunID)
		h.RunKey.Clear()
		return err
	}
	return c.Release(ctx, cl.DispatchID, "a reason", true)
}

// TestTheModelsRefusals walks the fake's dispatches through the contract's refusals,
// each read as its error: a handover before dispatched, a second handover, a request
// after the release, a stale dispatch and an unknown one.
func TestTheModelsRefusals(t *testing.T) {
	srv, c, clk, _ := setup(t)
	ctx := context.Background()
	cl := claimed(t, srv, c, provisionertest.Run{})
	if _, err := c.Handover(ctx, cl.DispatchID, cl.RunID); !errors.Is(err, provisioner.ErrNotDispatched) {
		t.Errorf("before dispatched: %v", err)
	}
	_ = c.Dispatched(ctx, cl.DispatchID)
	h, err := c.Handover(ctx, cl.DispatchID, cl.RunID)
	h.RunKey.Clear()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Handover(ctx, cl.DispatchID, cl.RunID); !errors.Is(err, provisioner.ErrHandedOver) {
		t.Errorf("a second handover: %v", err)
	}
	if err := c.Release(ctx, cl.DispatchID, "a reason", true); err != nil {
		t.Errorf("a release after the handover: %v", err)
	}
	if err := c.Release(ctx, cl.DispatchID, "a reason again", true); !errors.Is(err, provisioner.ErrGone) {
		t.Errorf("a release after the end: %v", err)
	}
	if err := c.Dispatched(ctx, cl.DispatchID); !errors.Is(err, provisioner.ErrGone) {
		t.Errorf("dispatched after the end: %v", err)
	}

	stale := claimed(t, srv, c, provisionertest.Run{Lease: 30 * time.Second})
	clk.set(clk.read().Add(30 * time.Second))
	if err := c.Dispatched(ctx, stale.DispatchID); !errors.Is(err, provisioner.ErrLeaseExpired) {
		t.Errorf("a lapsed lease: %v", err)
	}
	if err := c.Release(ctx, stale.DispatchID, "a reason", true); !errors.Is(err, provisioner.ErrLeaseExpired) {
		t.Errorf("a release after a lapsed lease: %v", err)
	}
	expired := claimed(t, srv, c, provisionertest.Run{})
	_ = c.Dispatched(ctx, expired.DispatchID)
	srv.Expire(expired.DispatchID)
	if _, err := c.Handover(ctx, expired.DispatchID, expired.RunID); !errors.Is(err, provisioner.ErrLeaseExpired) {
		t.Errorf("a handover after the lease: %v", err)
	}
	if err := c.Release(ctx, "dp_0000000000000000", "a reason", true); !errors.Is(err, provisioner.ErrForbidden) {
		t.Errorf("an unknown dispatch: %v", err)
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
			cl, h := handed(t, srv, c, provisionertest.Run{})
			h.RunKey.Clear()
			srv.Script(route, provisionertest.Reply{Status: 429, Code: "rate_limited", RetryAfter: rc.header})
			err := send(c, route, cl)
			var r *provisioner.Refusal
			if !errors.Is(err, provisioner.ErrRateLimited) || !errors.As(err, &r) || r.RetryAfter != rc.want || !strings.Contains(err.Error(), "with a wait of") {
				t.Errorf("%s, Retry-After %s: %v", route, rc.header, err)
			}
		}
	}
}

// TestAnAnswerThatBreaksTheContractIsNotUsed sends a claim and a handover a signed 200
// of another revision, with a member the schema does not define, with a member twice,
// and for the handover another run and a run key outside its form, and dispatched a
// 200: each is ErrAnswerUnusable after one try, and its text quotes no value.
func TestAnAnswerThatBreaksTheContractIsNotUsed(t *testing.T) {
	edits := map[string]func(map[string]any){
		"revision 1":   func(d map[string]any) { d["revision"] = 1 },
		"revision 2":   func(d map[string]any) { d["revision"] = 2 },
		"revision 4":   func(d map[string]any) { d["revision"] = 4 },
		"a new member": func(d map[string]any) { d["extra"] = "the title of the ticket" },
		"another step": func(d map[string]any) { d["step"] = "build" },
	}
	for name, edit := range edits {
		srv, c, _, _ := setup(t)
		srv.Offer(provisionertest.Run{})
		srv.Script(provisionertest.RouteClaim, provisionertest.Reply{Edit: edit})
		_, ok, err := c.Claim(context.Background(), provisioner.MaxWaitSeconds)
		if name == "another step" {
			if err != nil || !ok {
				t.Errorf("a claim of another step is the caller's to drop: %v", err)
			}
			continue
		}
		if !errors.Is(err, provisioner.ErrAnswerUnusable) || strings.Contains(err.Error(), "ticket") || len(srv.Calls()) != 1 {
			t.Errorf("a claim with %s: %v", name, err)
		}
	}
	twice := atRevision3(t, "claim-answer/explore.json")
	twice = strings.Replace(twice, `"version": 1,`, `"version": 1, "version": 1,`, 1)
	srv, c, _, _ := setup(t)
	srv.Script(provisionertest.RouteClaim, provisionertest.Reply{Status: 200, Body: twice})
	if _, _, err := c.Claim(context.Background(), provisioner.MaxWaitSeconds); !errors.Is(err, provisioner.ErrAnswerUnusable) {
		t.Errorf("a member twice: %v", err)
	}

	handover := map[string]func(map[string]any){
		"revision 1":        func(d map[string]any) { d["revision"] = 1 },
		"revision 2":        func(d map[string]any) { d["revision"] = 2 },
		"another run":       func(d map[string]any) { d["run_id"] = "0192f0c1-7d4e-7a2b-8c3d-000000000000" },
		"a run key's shape": func(d map[string]any) { d["run_key"] = "qrk_" + d["run_key"].(string)[4:] + "!" },
		"no run key":        func(d map[string]any) { delete(d, "run_key") },
		"a number as key":   func(d map[string]any) { d["run_key"] = 7 },
		"a new member":      func(d map[string]any) { d["extra"] = 1 },
		"inputs too large": func(d map[string]any) {
			d["inputs"] = map[string]any{"version": 1, "x": strings.Repeat("a", contracts.MaxInputs)}
		},
		"a prebuild's input": func(d map[string]any) { d["step"] = "prebuild"; delete(d, "work_item") },
	}
	for name, edit := range handover {
		srv, c, _, log := setup(t)
		cl := claimed(t, srv, c, provisionertest.Run{})
		_ = c.Dispatched(context.Background(), cl.DispatchID)
		srv.Script(provisionertest.RouteHandover, provisionertest.Reply{Edit: edit})
		h, err := c.Handover(context.Background(), cl.DispatchID, cl.RunID)
		d, _ := srv.Dispatch(cl.DispatchID)
		if !errors.Is(err, provisioner.ErrAnswerUnusable) || h.RunKey.Len() != 0 || h.Inputs.Len() != 0 {
			t.Errorf("a handover with %s: %v", name, err)
		}
		if strings.Contains(err.Error(), d.Run.RunKey[4:20]) || accesskey.ContainsSecret(err.Error()) || accesskey.ContainsSecret(log.String()) {
			t.Errorf("a handover with %s: the error or the log contains the key: %v", name, err)
		}
	}

	srv, c, _, _ = setup(t)
	cl := claimed(t, srv, c, provisionertest.Run{})
	srv.Script(provisionertest.RouteDispatched, provisionertest.Reply{Status: 200, Body: `{}`})
	if err := c.Dispatched(context.Background(), cl.DispatchID); !errors.Is(err, provisioner.ErrAnswerUnusable) {
		t.Errorf("a 200 to dispatched: %v", err)
	}
}

// TestAnswersAreReadUpToTheirBound sends a claim's 200 and a handover's 200 one byte
// over their bounds, and each just within: over is ErrTooLarge, never read as signed;
// within is used.
func TestAnswersAreReadUpToTheirBound(t *testing.T) {
	pad := func(body string, limit int) int { return limit - len(body) }
	srv, c, _, _ := setup(t)
	claim := atRevision3(t, "claim-answer/explore.json")
	for range 5 {
		srv.Script(provisionertest.RouteClaim, provisionertest.Reply{Status: 200, Body: claim, Pad: pad(claim, provisioner.MaxClaimAnswer) + 1})
	}
	if _, _, err := c.Claim(context.Background(), provisioner.MaxWaitSeconds); !errors.Is(err, provisioner.ErrTooLarge) || errors.Is(err, provisioner.ErrUnsigned) {
		t.Errorf("a claim's 200 over its bound: %v", err)
	}
	srv.Script(provisionertest.RouteClaim, provisionertest.Reply{Status: 200, Body: claim, Pad: pad(claim, provisioner.MaxClaimAnswer)})
	if _, ok, err := c.Claim(context.Background(), provisioner.MaxWaitSeconds); err != nil || !ok {
		t.Errorf("a claim's 200 at its bound: %v", err)
	}

	srv, c, _, _ = setup(t)
	cl := claimed(t, srv, c, provisionertest.Run{RunID: "0192f0c1-7d4e-7a2b-8c3d-4e5f6a7b8c9d"})
	_ = c.Dispatched(context.Background(), cl.DispatchID)
	handover := atRevision3(t, "handover-answer/explore.json")
	for range 5 {
		srv.Script(provisionertest.RouteHandover, provisionertest.Reply{Status: 200, Body: handover, Pad: pad(handover, contracts.MaxHandoverAnswer) + 1})
	}
	if _, err := c.Handover(context.Background(), cl.DispatchID, cl.RunID); !errors.Is(err, provisioner.ErrTooLarge) {
		t.Errorf("a handover's 200 over its bound: %v", err)
	}
	// Every try gets the answer at its bound, so no try reaches the model.
	for range 5 {
		srv.Script(provisionertest.RouteHandover, provisionertest.Reply{Status: 200, Body: handover, Pad: pad(handover, contracts.MaxHandoverAnswer)})
	}
	if h, err := c.Handover(context.Background(), cl.DispatchID, cl.RunID); err != nil || h.RunKey.Len() == 0 {
		t.Errorf("a handover's 200 at its bound: %v", err)
	}
	refusal := `{"error":"gone"}`
	srv.Script(provisionertest.RouteRelease, provisionertest.Reply{Status: 410, Body: refusal, Pad: pad(refusal, provisioner.MaxAnswer)})
	if err := c.Release(context.Background(), cl.DispatchID, "a reason", true); !errors.Is(err, provisioner.ErrGone) {
		t.Errorf("a refusal at its bound: %v", err)
	}
}

// TestARequestThatBreaksItsSchemaIsNotSent stops, before anything is sent, a release
// whose reason contains a run key or an access key secret, is empty or too long, and a
// dispatch id or a run id outside its form. No error quotes the secret or the id.
func TestARequestThatBreaksItsSchemaIsNotSent(t *testing.T) {
	srv, c, _, _ := setup(t)
	ctx := context.Background()
	const dp = "dp_0123456789abcdef"
	for name, err := range map[string]error{
		"a run key":        c.Release(ctx, dp, "the key is qrk_abc.def", true),
		"a secret":         c.Release(ctx, dp, "QAK_abcdef", true),
		"an empty reason":  c.Release(ctx, dp, "", true),
		"a long reason":    c.Release(ctx, dp, strings.Repeat("a", 201), true),
		"a NUL":            c.Release(ctx, dp, "a\x00b", true),
		"a dispatch id":    c.Dispatched(ctx, "dp_../../v1/pools/claim"),
		"a secret as id":   c.Release(ctx, "qrk_abc.def", "a reason", true),
		"a handover's run": func() error { _, err := c.Handover(ctx, dp, "x"); return err }(),
	} {
		if err == nil || accesskey.ContainsSecret(err.Error()) || strings.Contains(err.Error(), "../") {
			t.Errorf("%s: %v", name, err)
		}
	}
	srv.Script(provisionertest.RouteRelease, provisionertest.Reply{Status: http.StatusNoContent})
	if err := c.Release(ctx, dp, strings.Repeat("é", 200), true); err != nil {
		t.Errorf("200 characters of two bytes each: %v", err)
	}
	if n := len(srv.Calls()); n != 1 {
		t.Errorf("%d requests sent", n)
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
	srv.Offer(provisionertest.Run{})
	srv.Script(provisionertest.RouteClaim, provisionertest.Reply{Header: http.Header{"Set-Cookie": {"session=abc; Path=/"}}})
	cl, _, err := c.Claim(context.Background(), provisioner.MaxWaitSeconds)
	if err != nil {
		t.Fatal(err)
	}
	srv.Script(provisionertest.RouteRelease, provisionertest.Reply{Status: 307, Header: http.Header{"Location": {elsewhere.URL + "/v1/dispatches/" + cl.DispatchID + "/release"}}})
	var r *provisioner.Refusal
	if err := c.Release(context.Background(), cl.DispatchID, "a reason", true); !errors.As(err, &r) || r.Status != 307 {
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
// then sends the signed 204, no run; a run offered while it is held is claimed at
// once; a cancelled context ends a held claim with its error and nothing logged.
func TestAClaimIsHeldAndCancelled(t *testing.T) {
	srv, c, _, log := setup(t)
	srv.Hold = 50 * time.Millisecond
	if _, ok, err := c.Claim(context.Background(), provisioner.MaxWaitSeconds); err != nil || ok {
		t.Errorf("an empty claim: %v %v", ok, err)
	}
	srv.Hold = 10 * time.Second
	go func() {
		time.Sleep(50 * time.Millisecond)
		srv.Offer(provisionertest.Run{})
	}()
	if _, ok, err := c.Claim(context.Background(), provisioner.MaxWaitSeconds); err != nil || !ok {
		t.Errorf("a run offered while held: %v %v", ok, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if _, _, err := c.Claim(ctx, provisioner.MaxWaitSeconds); !errors.Is(err, context.Canceled) || time.Since(start) > 5*time.Second {
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
	_, _, err := c.Claim(context.Background(), provisioner.MaxWaitSeconds)
	want := "check that the access key is approved in Qory Apiary and that this machine's clock is within 300 seconds of the correct time: " +
		"the claim returned 401 unauthorized, which the client does not take as a revoked key"
	if !errors.Is(err, provisioner.ErrUnsigned) || err.Error() != want {
		t.Errorf("err %v", err)
	}
}

// sprintf is fmt.Sprintf of one value.
func sprintf(format string, v any) string { return fmt.Sprintf(format, v) }
