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

// The marker values whose parts the tests look for: a run code of the contract's form,
// a run key and an access key secret.
const (
	markerCode   = "qecr_MARKERC0DE0123456789ABCDEF.bWFya2VyLXJ1bi1jb2RlAA"
	markerKey    = "qrk_bWFya2VyLXJ1bi1rZXktcGF5bG9hZA.bWFya2VyLXJ1bi1rZXktc2lnbmF0dXJl"
	markerSecret = "qak_bWFya2VyLWFjY2Vzcy1rZXktc2VjcmV0"
)

// leaks are the parts of the markers that must appear nowhere.
var leaks = []string{
	markerCode, "MARKERC0DE0123456789ABCDEF", "bWFya2VyLXJ1bi1jb2RlAA", "qecr_",
	markerKey, markerKey[4:24], markerSecret, markerSecret[4:24],
}

// leaked is the first leak in s, in any case, or "".
func leaked(s string) string {
	s = strings.ToLower(s)
	for _, l := range leaks {
		if strings.Contains(s, strings.ToLower(l)) {
			return l
		}
	}
	return ""
}

// TestNoSecretLeaks claims work whose run code is the marker after tries that fail with
// bodies that contain the markers, with the client's log on; sends claim answers that
// break the contract and contain the markers, and refusals whose bodies contain them;
// stops a release and a dispatched that contain them; uploads a machine log with lines
// that contain them; and prints, logs and marshals what the client returned. No log
// line, no error, no printout and no request contains a marker, and the run code is the
// work's.
func TestNoSecretLeaks(t *testing.T) {
	srv, c, _, log := setup(t)
	ctx := context.Background()
	var out bytes.Buffer
	body := `{"error":"` + markerCode + `","key":"` + markerKey + `","secret":"` + markerSecret + `"}`
	srv.Script(provisionertest.RouteClaim,
		provisionertest.Reply{Status: 409, Body: body, Unsigned: true},
		provisionertest.Reply{Status: 500, Body: body},
	)
	work := provisionertest.Work{RunCode: markerCode}
	srv.Offer(work)
	cl, ok, err := c.Claim(ctx, provisioner.MaxWaitSeconds)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if written(t, cl.RunCode) != markerCode {
		t.Fatal("the run code is not the work's")
	}

	for _, edit := range []func(map[string]any){
		func(d map[string]any) { d["revision"] = 3 },
		func(d map[string]any) { d["extra"] = markerCode },
		func(d map[string]any) { d["machine"] = markerCode },
		func(d map[string]any) { d["run_code"] = strings.ToLower(markerCode) },
		func(d map[string]any) { d["run_code"] = markerCode + "!" },
		func(d map[string]any) { d["run_code"] = markerKey },
		func(d map[string]any) {
			d["gateway_url"] = "https://" + strings.ToLower(markerCode[:31]) + ".example.com"
		},
		func(d map[string]any) { d["run_key"] = markerKey },
	} {
		srv.Offer(work)
		srv.Script(provisionertest.RouteClaim, provisionertest.Reply{Edit: edit})
		if got, _, err := c.Claim(ctx, provisioner.MaxWaitSeconds); err == nil || got.RunCode.Len() != 0 {
			t.Error("a claim answer that breaks the contract is used")
		} else {
			out.WriteString(err.Error() + "\n")
		}
	}
	for _, route := range []provisionertest.Route{"dispatched", "release", "machine-log"} {
		srv.Script(route, provisionertest.Reply{Status: 409, Body: body})
		if err := send(c, route, cl); err == nil {
			t.Errorf("%s: a signed 409 with a marker as its code is not a refusal", route)
		} else {
			out.WriteString(err.Error() + "\n")
		}
	}
	for _, err := range []error{
		c.Release(ctx, cl.DispatchID, "the run code is "+markerCode),
		c.Release(ctx, cl.DispatchID, "the run key is "+markerKey),
		c.Dispatched(ctx, cl.DispatchID, &contracts.External{Type: "example-ci", Ref: markerCode, URL: "https://ci.example.com/runs/1"}),
		c.Dispatched(ctx, cl.DispatchID, &contracts.External{Type: "example-ci", Ref: "i-0123456789abcdef0", URL: "https://ci.example.com/" + markerSecret}),
	} {
		if err == nil {
			t.Error("a request that contains a marker is sent")
		} else {
			out.WriteString(err.Error() + "\n")
		}
	}
	machineLog := "boot\nrun code " + markerCode + "\nrun key " + markerKey + "\nkey " + strings.ToUpper(markerSecret) + "\nend\n"
	if err := c.MachineLog(ctx, cl.DispatchID, machineLog, false); err != nil {
		t.Fatal(err)
	}

	slog.New(slog.NewTextHandler(&out, nil)).Info("x", "claimed", cl, "code", cl.RunCode)
	slog.New(slog.NewJSONHandler(&out, nil)).Info("x", "claimed", cl, "code", cl.RunCode, slog.Any("any", &cl))
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		for _, v := range []any{cl, &cl, cl.RunCode, &cl.RunCode} {
			out.WriteString(sprintf(verb, v))
		}
	}
	for _, v := range []any{cl, &cl, cl.RunCode} {
		b, _ := json.Marshal(v)
		out.Write(b)
	}
	out.WriteString(log.String())
	if log.Len() == 0 {
		t.Error("nothing logged")
	}
	if l := leaked(out.String()); l != "" {
		t.Errorf("%q is in the output:\n%s", l, out.String())
	}
	if accesskey.ContainsSecret(out.String()) {
		t.Errorf("what looks like a secret is in the output:\n%s", out.String())
	}
	for _, call := range srv.Calls() {
		if l := leaked(string(call.Body) + call.Target); l != "" {
			t.Errorf("%s: a request contains %q", call.Route, l)
		}
	}
	cl.RunCode.Clear()
	if cl.RunCode.Len() != 0 {
		t.Error("the run code is kept after Clear")
	}
}

// TestARequestWithASecretIsNotSent stops a release whose reason contains the marker run
// code, and logs and sends nothing.
func TestARequestWithASecretIsNotSent(t *testing.T) {
	srv, c, _, log := setup(t)
	cl := claimed(t, srv, c, provisionertest.Work{RunCode: markerCode})
	cl.RunCode.Clear()
	before := len(srv.Calls())
	err := c.Release(context.Background(), cl.DispatchID, "the run code is "+markerCode)
	if err == nil || leaked(err.Error()) != "" || log.Len() != 0 || len(srv.Calls()) != before {
		t.Errorf("err %v, logged %s", err, log)
	}
}

// TestAMachineLogOverItsBoundHasNoMarker uploads a log as an adapter returns it: marker
// lines of the run code, the run key and the access key secret in upper and lower case
// at its start and at its end, around lines that JSON escapes to more than a mebibyte.
// The body sent is within the request's bound, truncated is set, the log is the end of
// the log with each marker line replaced, and no request contains a marker.
func TestAMachineLogOverItsBoundHasNoMarker(t *testing.T) {
	srv, c, _, log := setup(t)
	cl := claimed(t, srv, c, provisionertest.Work{})
	cl.RunCode.Clear()
	markers := "run code " + markerCode + "\nrun key " + strings.ToUpper(markerKey) + "\nkey " + markerSecret + "\n"
	sent := markers + strings.Repeat("<<<<<<<<<\n", 70<<10) + markers + "end\n"
	if err := c.MachineLog(context.Background(), cl.DispatchID, sent, false); err != nil {
		t.Fatal(err)
	}
	d, _ := srv.Dispatch(cl.DispatchID)
	body := last(t, srv).Body
	hidden := "[hidden: the line contained what looks like an access key secret, a run key or a run code]\n"
	switch {
	case d.MachineLog == nil:
		t.Fatal("no machine log")
	case !d.MachineLog.Truncated || len(body) > contracts.MaxRequest || len(d.MachineLog.Log) > provisioner.MaxLog:
		t.Errorf("truncated %v, a body of %d bytes", d.MachineLog.Truncated, len(body))
	case !strings.HasSuffix(d.MachineLog.Log, "<<<<<<<<<\n"+strings.Repeat(hidden, 3)+"end\n"):
		t.Error("the end of the log is not the log's end with each marker line replaced")
	}
	for _, call := range srv.Calls() {
		if l := leaked(string(call.Body)); l != "" {
			t.Errorf("%s: a request contains %q", call.Route, l)
		}
	}
	if l := leaked(log.String()); l != "" {
		t.Errorf("the log contains %q", l)
	}
}
