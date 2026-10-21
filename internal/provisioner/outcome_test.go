package provisioner_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/qoryai/forager/accesskey"
	foragercontracts "github.com/qoryai/forager/contracts"
	"github.com/qoryai/forager/server"

	"github.com/qoryai/hive/internal/provisioner"
	"github.com/qoryai/hive/internal/provisioner/provisionertest"
)

// foragerFixture reads one fixture of Forager's contract.
func foragerFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := fs.ReadFile(foragercontracts.FS, "fixtures/"+name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestTheOutcomeBodiesAreTheFixtures posts an outcome with no state and one with a
// state, at the time of Forager's fixtures: each body sent is the fixture.
func TestTheOutcomeBodiesAreTheFixtures(t *testing.T) {
	srv, c, clk, _ := setup(t)
	clk.set(time.Date(2026, 10, 11, 12, 30, 0, 0, time.UTC))
	const run = "0192f0c1-7d4e-7a2b-8c3d-4e5f6a7b8c9d"
	for name, o := range map[string]provisioner.Outcome{
		"run-outcome/none.json":  {},
		"run-outcome/state.json": {State: provisioner.StateFailed},
	} {
		srv.Script(provisionertest.RouteOutcome, provisionertest.Reply{Status: http.StatusOK, Body: `{"version":1}`})
		if err := c.PostOutcome(context.Background(), srv.URL+provisionertest.RunPath, run, o); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		call := last(t, srv)
		if b := foragerFixture(t, name); !sameJSON(t, call.Body, b) || call.Target != provisionertest.RunPath+"/"+run+"/outcome" {
			t.Errorf("%s: %s %s, want %s", name, call.Target, call.Body, b)
		}
	}
}

// TestAnOutcomeIsGivenOnce posts the outcome of a handed-over run twice, then another
// one, then one for a run the fake did not hand over: the same one again is accepted,
// another is ErrOutcomeGiven and the unknown run ErrRunNotFound.
func TestAnOutcomeIsGivenOnce(t *testing.T) {
	srv, c, clk, _ := setup(t)
	ctx := context.Background()
	cl, h := handed(t, srv, c, provisionertest.Run{})
	h.RunKey.Clear()
	runURL := srv.URL + provisionertest.RunPath
	o := provisioner.Outcome{State: provisioner.StateSucceeded}
	if err := c.PostOutcome(ctx, runURL, cl.RunID, o); err != nil {
		t.Fatal(err)
	}
	clk.set(clk.read().Add(time.Second))
	if err := c.PostOutcome(ctx, runURL, cl.RunID, o); err != nil {
		t.Errorf("the same outcome again: %v", err)
	}
	if err := c.PostOutcome(ctx, runURL, cl.RunID, provisioner.Outcome{State: provisioner.StateCancelled}); !errors.Is(err, provisioner.ErrOutcomeGiven) {
		t.Errorf("another outcome: %v", err)
	}
	if err := c.PostOutcome(ctx, runURL, "0192f0c1-7d4e-7a2b-8c3d-000000000000", o); !errors.Is(err, provisioner.ErrRunNotFound) || errors.Is(err, provisioner.ErrNotFound) {
		t.Errorf("a run not handed over: %v", err)
	}
	if got := srv.Outcomes()[cl.RunID]; got.State != "succeeded" || got.Reason != "" {
		t.Errorf("recorded %+v", got)
	}
}

// TestAnOutcomeOutsideTheContractIsNotSent posts outcomes whose state, reason, run id
// or run endpoint is outside its form: each is an error before anything is sent, and
// no error quotes the run endpoint.
func TestAnOutcomeOutsideTheContractIsNotSent(t *testing.T) {
	srv, c, _, _ := setup(t)
	ctx := context.Background()
	runURL := srv.URL + provisionertest.RunPath
	const run = "0192f0c1-7d4e-7a2b-8c3d-4e5f6a7b8c9d"
	for name, err := range map[string]error{
		"a state":                c.PostOutcome(ctx, runURL, run, provisioner.Outcome{State: "done"}),
		"a reason with no state": c.PostOutcome(ctx, runURL, run, provisioner.Outcome{Reason: "time_limit"}),
		"a reason not a code":    c.PostOutcome(ctx, runURL, run, provisioner.Outcome{State: "failed", Reason: "Not A Code"}),
		"a long reason":          c.PostOutcome(ctx, runURL, run, provisioner.Outcome{State: "failed", Reason: "a" + strings.Repeat("b", 64)}),
		"a run id":               c.PostOutcome(ctx, runURL, strings.ToUpper(run), provisioner.Outcome{}),
		"http to a host":         c.PostOutcome(ctx, "http://runs.example.com/v1/runs", run, provisioner.Outcome{}),
		"a trailing slash":       c.PostOutcome(ctx, runURL+"/", run, provisioner.Outcome{}),
		"a query":                c.PostOutcome(ctx, runURL+"?a=b", run, provisioner.Outcome{}),
		"a secret":               c.PostOutcome(ctx, "https://runs.example.com/qrk_abc.def", run, provisioner.Outcome{}),
	} {
		if err == nil || accesskey.ContainsSecret(err.Error()) || strings.Contains(err.Error(), "example.com") || strings.Contains(err.Error(), "127.0.0.1") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := len(srv.Calls()); n != 0 {
		t.Errorf("%d requests sent", n)
	}
}

// TestAnOutcomeAnswerOutsideItsSchemaIsNotUsed sends an outcome a signed 200 whose body
// fails run-outcome-answer.schema.json: ErrAnswerUnusable after one try.
func TestAnOutcomeAnswerOutsideItsSchemaIsNotUsed(t *testing.T) {
	srv, c, _, _ := setup(t)
	srv.Script(provisionertest.RouteOutcome, provisionertest.Reply{Status: http.StatusOK, Body: `{"version":2}`})
	err := c.PostOutcome(context.Background(), srv.URL+provisionertest.RunPath, "0192f0c1-7d4e-7a2b-8c3d-4e5f6a7b8c9d", provisioner.Outcome{})
	if !errors.Is(err, provisioner.ErrAnswerUnusable) || len(srv.Calls()) != 1 {
		t.Errorf("err %v", err)
	}
}

// configurationDocument is a configuration document of the fake's node pool with the
// run endpoint runURL.
func configurationDocument(t *testing.T, srv *provisionertest.Server, runURL string) string {
	t.Helper()
	b, err := json.Marshal(server.Configuration{
		Version: 1, NodeID: provisionertest.PoolID, Workspaces: []string{provisionertest.WorkspaceID},
		Events: server.Events{URL: srv.URL + provisionertest.EventsPath, Types: []string{"*"}},
		Run:    &server.Endpoint{URL: runURL}, ApiaryPublicKey: srv.Pin(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestTheConfigurationDocumentIsChecked fetches the document with no
// X-Qory-Configuration, and with a run endpoint changed in one place each: each is
// ErrAnswerUnusable after one try, and its text quotes no run endpoint. A member the
// schema does not define is ignored.
func TestTheConfigurationDocumentIsChecked(t *testing.T) {
	srv, c, _, _ := setup(t)
	srv.Script(provisionertest.RouteConfiguration, provisionertest.Reply{Status: http.StatusOK, Body: configurationDocument(t, srv, srv.URL+provisionertest.RunPath)})
	if _, err := c.Configuration(context.Background()); !errors.Is(err, provisioner.ErrAnswerUnusable) || !strings.Contains(err.Error(), "X-Qory-Configuration") {
		t.Errorf("no X-Qory-Configuration: %v", err)
	}
	for name, runURL := range map[string]any{
		"http to a host":   "http://runs.example.com/v1/runs",
		"a trailing slash": "https://runs.example.com/v1/runs/",
		"a query":          "https://runs.example.com/v1/runs?a=b",
		"a secret":         "https://runs.example.com/qak_abc",
		"no run endpoint":  nil,
	} {
		srv, c, _, _ := setup(t)
		srv.Script(provisionertest.RouteConfiguration, provisionertest.Reply{Edit: func(d map[string]any) {
			if runURL == nil {
				delete(d, "run")
				return
			}
			d["run"] = map[string]any{"url": runURL}
		}})
		_, err := c.Configuration(context.Background())
		if !errors.Is(err, provisioner.ErrAnswerUnusable) || strings.Contains(err.Error(), "example.com") || len(srv.Calls()) != 1 {
			t.Errorf("%s: %v", name, err)
		}
	}
	srv, c, _, _ = setup(t)
	srv.Script(provisionertest.RouteConfiguration, provisionertest.Reply{Edit: func(d map[string]any) {
		d["later"] = map[string]any{"url": "https://later.example.com"}
		d["run"] = map[string]any{"url": "https://runs.example.com/v1/runs"}
	}})
	if conf, err := c.Configuration(context.Background()); err != nil || conf.RunURL != "https://runs.example.com/v1/runs" {
		t.Errorf("a member the schema does not define: %+v %v", conf, err)
	}
}

// TestTheConfigurationDocumentIsReadUpToItsBound sends the document one byte over
// server.MaxDocument to every try, then at the bound: over is ErrTooLarge, at the
// bound is read.
func TestTheConfigurationDocumentIsReadUpToItsBound(t *testing.T) {
	srv, c, _, _ := setup(t)
	doc := configurationDocument(t, srv, srv.URL+provisionertest.RunPath)
	over := provisionertest.Reply{Status: http.StatusOK, Body: doc, Pad: server.MaxDocument - len(doc) + 1}
	srv.Script(provisionertest.RouteConfiguration, over, over, over, over, over)
	if _, err := c.Configuration(context.Background()); !errors.Is(err, provisioner.ErrTooLarge) {
		t.Errorf("over the bound: %v", err)
	}
	srv.Script(provisionertest.RouteConfiguration, provisionertest.Reply{Edit: func(d map[string]any) {
		d["padding"] = strings.Repeat("a", server.MaxDocument-len(doc)-64)
	}})
	if _, err := c.Configuration(context.Background()); err != nil {
		t.Errorf("within the bound: %v", err)
	}
}
