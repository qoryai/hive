package provisioner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/qoryai/forager/accesskey"
	"github.com/qoryai/integrations/contracts"

	"github.com/qoryai/hive/internal/provisioner"
	"github.com/qoryai/hive/internal/provisioner/provisionertest"
)

// The marker values of a run whose secrets the tests look for.
const (
	markerKey    = "qrk_bWFya2VyLXJ1bi1rZXktcGF5bG9hZA.bWFya2VyLXJ1bi1rZXktc2lnbmF0dXJl"
	markerTitle  = "Marker title 4f1c9a"
	markerInputs = "marker-inputs-7d2e05"
)

// markerRun is a run whose run key, title and inputs are the markers.
func markerRun() provisionertest.Run {
	return provisionertest.Run{
		RunKey: markerKey,
		WorkItem: &contracts.WorkItem{
			ID: "wi_8Kx2mQ4vT9", URL: "https://control.example.com/work-items/wi_8Kx2mQ4vT9", Title: markerTitle,
		},
		Inputs: json.RawMessage(`{"version":1,"note":"` + markerInputs + `"}`),
	}
}

// leaks are the parts of the markers that must appear nowhere.
var leaks = []string{markerKey, markerKey[4:24], markerTitle, "4f1c9a", markerInputs, "7d2e05"}

// TestNoSecretLeaks claims and hands over the marker run after tries that fail with
// bodies that contain the markers, with the client's log on; sends answers that break
// the contract and contain the markers, and refusals whose bodies contain them; and
// prints, logs and marshals what the client returned. No log line, no error and no printout contains a marker, and the run key,
// the title and the inputs are each the run's.
func TestNoSecretLeaks(t *testing.T) {
	srv, c, _, log := setup(t)
	ctx := context.Background()
	var out bytes.Buffer
	body := `{"error":"` + markerKey + `","title":"` + markerTitle + `","inputs":"` + markerInputs + `"}`
	srv.Script(provisionertest.RouteClaim,
		provisionertest.Reply{Status: 409, Body: body, Unsigned: true},
		provisionertest.Reply{Status: 500, Body: body},
	)
	srv.Offer(markerRun())
	cl, ok, err := c.Claim(ctx, provisioner.MaxWaitSeconds)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := c.Dispatched(ctx, cl.DispatchID); err != nil {
		t.Fatal(err)
	}
	srv.Script(provisionertest.RouteHandover,
		provisionertest.Reply{Status: 204, Drop: true},
		provisionertest.Reply{Status: 409, Body: body, Unsigned: true},
		provisionertest.Reply{Status: 500, Body: body},
	)
	h, err := c.Handover(ctx, cl.DispatchID, cl.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if written(t, h.RunKey) != markerKey || written(t, h.WorkItem.Title) != markerTitle || !strings.Contains(written(t, h.Inputs), markerInputs) {
		t.Fatal("the run key, the title or the inputs are not the run's")
	}

	for _, edit := range []func(map[string]any){
		func(d map[string]any) { d["revision"] = 2 },
		func(d map[string]any) { d["extra"] = markerTitle },
		func(d map[string]any) { d["run_id"] = "0192f0c1-7d4e-7a2b-8c3d-000000000000" },
	} {
		srv.Offer(markerRun())
		srv.Script(provisionertest.RouteClaim, provisionertest.Reply{Edit: edit})
		if _, _, err := c.Claim(ctx, provisioner.MaxWaitSeconds); err != nil {
			out.WriteString(err.Error() + "\n")
		}
		next := claimed(t, srv, c, markerRun())
		_ = c.Dispatched(ctx, next.DispatchID)
		srv.Script(provisionertest.RouteHandover, provisionertest.Reply{Edit: edit})
		if _, err := c.Handover(ctx, next.DispatchID, next.RunID); err == nil {
			t.Error("a handover that breaks the contract is used")
		} else {
			out.WriteString(err.Error() + "\n")
		}
	}
	srv.Script(provisionertest.RouteRelease, provisionertest.Reply{Status: 409, Body: body})
	if err := c.Release(ctx, cl.DispatchID, "a reason", true); err != nil {
		out.WriteString(err.Error() + "\n")
	}

	slog.New(slog.NewTextHandler(&out, nil)).Info("x", "claimed", cl, "handed", h, "key", h.RunKey, "inputs", h.Inputs)
	slog.New(slog.NewJSONHandler(&out, nil)).Info("x", "claimed", cl, "handed", h, "work_item", h.WorkItem, "title", h.WorkItem.Title)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		for _, v := range []any{cl, &cl, cl.WorkItem, h, &h, h.RunKey, h.Inputs, h.WorkItem, *h.WorkItem} {
			out.WriteString(sprintf(verb, v))
		}
	}
	for _, v := range []any{cl, h} {
		b, _ := json.Marshal(v)
		out.Write(b)
	}
	out.WriteString(log.String())
	if log.Len() == 0 {
		t.Error("nothing logged")
	}
	text := strings.ToLower(out.String())
	for _, s := range leaks {
		if strings.Contains(text, strings.ToLower(s)) {
			t.Errorf("%q is in the output:\n%s", s, out.String())
		}
	}
	if accesskey.ContainsSecret(out.String()) {
		t.Errorf("what looks like a secret is in the output:\n%s", out.String())
	}
	for _, call := range srv.Calls() {
		for _, s := range leaks {
			if strings.Contains(string(call.Body), s) || strings.Contains(call.Target, s) {
				t.Errorf("%s: a request contains %q", call.Route, s)
			}
		}
	}
	h.RunKey.Clear()
	h.Inputs.Clear()
	if h.RunKey.Len() != 0 || h.Inputs.Len() != 0 {
		t.Error("a value is kept after Clear")
	}
}

// TestAReleaseOfAHandedOverRunIsNotResentOnASecret stops a release whose reason
// contains the marker run key, and logs nothing.
func TestAReleaseOfAHandedOverRunIsNotResentOnASecret(t *testing.T) {
	srv, c, _, log := setup(t)
	cl, h := handed(t, srv, c, markerRun())
	h.RunKey.Clear()
	err := c.Release(context.Background(), cl.DispatchID, "the run key is "+markerKey, true)
	if err == nil || strings.Contains(err.Error(), markerKey[4:24]) || log.Len() != 0 {
		t.Errorf("err %v, logged %s", err, log)
	}
}
