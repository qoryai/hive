package machines_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/qoryai/hive/machines"
)

// TestEncodeRoundTrip checks that every document that Encode writes decodes to the
// same value.
func TestEncodeRoundTrip(t *testing.T) {
	started := time.Date(2026, 10, 22, 10, 0, 0, 5, time.UTC)
	cases := []struct {
		doc    machines.Document
		decode func([]byte) (machines.Document, error)
	}{
		{machines.CheckRequest{Version: 1}, func(b []byte) (machines.Document, error) { return machines.DecodeCheckRequest(b) }},
		{machines.CheckAnswer{}, func(b []byte) (machines.Document, error) { return machines.DecodeCheckAnswer(b) }},
		{machines.MakeRequest{Version: 1, Key: "dp_0123456789abcdef", Pool: "np_0123456789abcdef", Size: "standard", StartData: []byte{0, 1, 2, 250}}, func(b []byte) (machines.Document, error) { return machines.DecodeMakeRequest(b) }},
		{machines.MakeAnswer{ID: "i-0123456789abcdef0"}, func(b []byte) (machines.Document, error) { return machines.DecodeMakeAnswer(b) }},
		{machines.ListRequest{Version: 1, Pool: "np_0123456789abcdef"}, func(b []byte) (machines.Document, error) { return machines.DecodeListRequest(b) }},
		{machines.ListAnswer{Machines: []machines.Machine{{ID: "i-1", Key: "dp_0123456789abcdef", State: machines.StateStopped, Started: started}}}, func(b []byte) (machines.Document, error) { return machines.DecodeListAnswer(b) }},
		{machines.ListAnswer{Machines: []machines.Machine{}}, func(b []byte) (machines.Document, error) { return machines.DecodeListAnswer(b) }},
		{machines.LogRequest{Version: 1, ID: "i-1"}, func(b []byte) (machines.Document, error) { return machines.DecodeLogRequest(b) }},
		{machines.LogAnswer{Log: "boot\n"}, func(b []byte) (machines.Document, error) { return machines.DecodeLogAnswer(b) }},
		{machines.DeleteRequest{Version: 1, ID: "i-1"}, func(b []byte) (machines.Document, error) { return machines.DecodeDeleteRequest(b) }},
		{machines.DeleteAnswer{State: machines.StateGone}, func(b []byte) (machines.Document, error) { return machines.DecodeDeleteAnswer(b) }},
		{machines.ErrorAnswer{Error: machines.Error{Kind: machines.KindLimit, Message: "Raise the quota."}}, func(b []byte) (machines.Document, error) { return machines.DecodeErrorAnswer(b) }},
	}
	for _, c := range cases {
		b, err := machines.Encode(c.doc)
		if err != nil {
			t.Fatalf("%T: %v", c.doc, err)
		}
		got, err := c.decode(b)
		if err != nil {
			t.Fatalf("%T: %s: %v", c.doc, b, err)
		}
		if a, ok := got.(machines.ListAnswer); ok && len(a.Machines) == 1 {
			if !a.Machines[0].Started.Equal(started) {
				t.Errorf("started is %v, want %v", a.Machines[0].Started, started)
			}
			a.Machines[0].Started = started
			got = a
		}
		if !reflect.DeepEqual(got, c.doc) {
			t.Errorf("%T: decoded %#v, want %#v", c.doc, got, c.doc)
		}
	}
	if b, _ := machines.Encode(machines.ListAnswer{}); string(b) != `{"machines":[]}` {
		t.Errorf("an empty list answer is %s", b)
	}
}

// TestEncodeRefusesInvalid checks that Encode refuses a document that fails a rule.
func TestEncodeRefusesInvalid(t *testing.T) {
	for _, doc := range []machines.Document{
		machines.CheckRequest{},
		machines.MakeRequest{Version: 1, Key: "dp_0123456789abcdef", Pool: "np_0123456789abcdef", Size: "s"},
		machines.MakeRequest{Version: 1, Key: "dp_0123456789abcdef", Pool: "np_0123456789abcdef", Size: "s", StartData: make([]byte, machines.MaxStartData+1)},
		machines.MakeAnswer{ID: machines.NoID},
		machines.ListAnswer{Machines: []machines.Machine{{ID: "i", Key: "dp_0123456789abcdef", State: machines.StateGone, Started: time.Now()}}},
		machines.ListAnswer{Machines: []machines.Machine{{ID: "i", Key: "dp_0123456789abcdef", State: machines.StateRunning, Started: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}}},
		machines.DeleteAnswer{State: machines.StateRunning},
		machines.LogAnswer{Log: "\xff"},
		machines.ErrorAnswer{Error: machines.Error{Kind: "fatal", Message: "m"}},
	} {
		if b, err := machines.Encode(doc); err == nil {
			t.Errorf("%#v encoded as %s", doc, b)
		}
	}
}

// handler is a [machines.Handler] that records its calls.
type handler struct {
	settings string
	make     machines.MakeRequest
	err      error
	answer   machines.LogAnswer
	list     machines.ListAnswer
}

func (h *handler) Check(_ context.Context, settings string, _ machines.CheckRequest) (machines.CheckAnswer, error) {
	h.settings = settings
	return machines.CheckAnswer{}, h.err
}

func (h *handler) Make(_ context.Context, settings string, req machines.MakeRequest) (machines.MakeAnswer, error) {
	h.settings, h.make = settings, req
	return machines.MakeAnswer{ID: "i-0123456789abcdef0"}, h.err
}

func (h *handler) List(context.Context, string, machines.ListRequest) (machines.ListAnswer, error) {
	return h.list, h.err
}

func (h *handler) Log(context.Context, string, machines.LogRequest) (machines.LogAnswer, error) {
	return h.answer, h.err
}

func (h *handler) Delete(context.Context, string, machines.DeleteRequest) (machines.DeleteAnswer, error) {
	return machines.DeleteAnswer{State: machines.StateDeleting}, h.err
}

// serve runs Serve with args and the request in, and returns the exit status and the
// output.
func serve(h machines.Handler, in string, args ...string) (int, string) {
	var out bytes.Buffer
	status := machines.Serve(context.Background(), args, strings.NewReader(in), &out, h)
	return status, out.String()
}

func TestServe(t *testing.T) {
	h := &handler{}
	status, out := serve(h, `{"version": 1, "key": "dp_0123456789abcdef", "pool": "np_0123456789abcdef", "size": "standard", "start_data": "AAEC+g=="}`, "make", "--settings", "/etc/adapter.json")
	if status != machines.ExitOK || out != `{"id":"i-0123456789abcdef0"}`+"\n" {
		t.Fatalf("make: %d %q", status, out)
	}
	want := machines.MakeRequest{Version: 1, Key: "dp_0123456789abcdef", Pool: "np_0123456789abcdef", Size: "standard", StartData: []byte{0, 1, 2, 250}}
	if h.settings != "/etc/adapter.json" || !reflect.DeepEqual(h.make, want) {
		t.Errorf("the handler received %q, %#v", h.settings, h.make)
	}
	if status, out := serve(h, `{"version": 1}`, "check", "--settings", "a.json"); status != 0 || out != "{}\n" {
		t.Errorf("check: %d %q", status, out)
	}
	if status, out := serve(h, `{"version": 1, "pool": "np_0123456789abcdef"}`, "list", "--settings", "a.json"); status != 0 || out != `{"machines":[]}`+"\n" {
		t.Errorf("list: %d %q", status, out)
	}
	if status, out := serve(h, `{"version": 1, "id": "i-1"}`, "delete", "--settings", "a.json"); status != 0 || out != `{"state":"deleting"}`+"\n" {
		t.Errorf("delete: %d %q", status, out)
	}
}

func TestServeUsage(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"make"},
		{"make", "--settings"},
		{"make", "--settings", ""},
		{"start", "--settings", "a.json"},
		{"make", "--config", "a.json"},
		{"make", "--settings", "a.json", "extra"},
	} {
		status, out := serve(&handler{}, `{"version": 1}`, args...)
		if status != machines.ExitFailure {
			t.Errorf("%q: status %d", args, status)
		}
		doc, err := machines.DecodeErrorAnswer([]byte(out))
		if err != nil || doc.Error.Kind != machines.KindDefect || !strings.HasPrefix(doc.Error.Message, "Run the machine adapter as <command> --settings FILE") {
			t.Errorf("%q: %q, %v", args, out, err)
		}
	}
}

// errorOf runs Serve and returns the error document that it wrote with status 1.
func errorOf(t *testing.T, h machines.Handler, in string, args ...string) machines.Error {
	t.Helper()
	status, out := serve(h, in, args...)
	if status != machines.ExitFailure {
		t.Fatalf("status %d, output %q", status, out)
	}
	doc, err := machines.DecodeErrorAnswer([]byte(out))
	if err != nil {
		t.Fatalf("%q: %v", out, err)
	}
	return doc.Error
}

func TestServeFailures(t *testing.T) {
	args := []string{"log", "--settings", "a.json"}
	in := `{"version": 1, "id": "i-1"}`

	e := errorOf(t, &handler{err: &machines.Error{Kind: machines.KindLimit, Message: "Raise the quota."}}, in, args...)
	if e != (machines.Error{Kind: machines.KindLimit, Message: "Raise the quota."}) {
		t.Errorf("a handler's *Error became %#v", e)
	}
	e = errorOf(t, &handler{err: errors.New("Check the network.")}, in, args...)
	if e != (machines.Error{Kind: machines.KindOther, Message: "Check the network."}) {
		t.Errorf("a plain error became %#v", e)
	}
	e = errorOf(t, &handler{err: &machines.Error{Kind: "fatal"}}, in, args...)
	if e.Kind != machines.KindOther || e.Message != "Fix the machine adapter: it failed with an empty message" {
		t.Errorf("an invalid *Error became %#v", e)
	}
	e = errorOf(t, &handler{err: errors.New(strings.Repeat("é", machines.MaxMessage+5))}, in, args...)
	if len([]rune(e.Message)) != machines.MaxMessage {
		t.Errorf("a long message has %d characters", len([]rune(e.Message)))
	}
	e = errorOf(t, &handler{}, `{"version": 2, "id": "i-1"}`, args...)
	if e.Kind != machines.KindDefect || e.Message != "Report this defect in the provisioner: its log request to the machine adapter is not valid: the member version must be 1" {
		t.Errorf("a bad request became %#v", e)
	}
	e = errorOf(t, &handler{}, strings.Repeat(" ", machines.MaxAnswer+1), args...)
	if e.Kind != machines.KindDefect || e.Message != "Report this defect in the provisioner: its log request to the machine adapter is larger than 1048576 bytes" {
		t.Errorf("a large request became %#v", e)
	}
	e = errorOf(t, &handler{answer: machines.LogAnswer{Log: strings.Repeat("a", machines.MaxLog+1)}}, in, args...)
	if e.Kind != machines.KindDefect || e.Message != "Fix the machine adapter: its log answer is not valid: the member log must be at most 786432 bytes of UTF-8" {
		t.Errorf("an invalid answer became %#v", e)
	}
	many := make([]machines.Machine, 12000)
	for i := range many {
		many[i] = machines.Machine{ID: "i-0123456789abcdef0", Key: "dp_0123456789abcdef", State: machines.StateRunning, Started: time.Now()}
	}
	e = errorOf(t, &handler{list: machines.ListAnswer{Machines: many}}, `{"version": 1, "pool": "np_0123456789abcdef"}`, "list", "--settings", "a.json")
	if e.Kind != machines.KindDefect || e.Message != "Fix the machine adapter: its list answer is larger than 1048576 bytes" {
		t.Errorf("a large answer became %#v", e)
	}
}

// TestServeLogBound checks that a log answer of MaxLog bytes, each escaped as \u00XX,
// fits the bound of log.
func TestServeLogBound(t *testing.T) {
	status, out := serve(&handler{answer: machines.LogAnswer{Log: strings.Repeat("\x00", machines.MaxLog)}}, `{"version": 1, "id": "i-1"}`, "log", "--settings", "a.json")
	if status != machines.ExitOK || len(out) > machines.MaxOutput("log") || len(out) <= machines.MaxAnswer {
		t.Errorf("status %d, %d bytes", status, len(out))
	}
}

func TestServeRefusesSecretMessage(t *testing.T) {
	data := []byte(`{"version":1,"run_code":"qecr_MARKERc0ffee7a1b","gateway":{"url":"https://gateway.example.com:8443","address":"10.0.1.5"}}`)
	enc := base64.StdEncoding.EncodeToString(data)
	in := `{"version": 1, "key": "dp_0123456789abcdef", "pool": "np_0123456789abcdef", "size": "standard", "start_data": "` + enc + `"}`
	for _, err := range []error{
		errors.New("Check the start data " + enc[:80]),
		errors.New("Check the start data " + string(data)),
		&machines.Error{Kind: machines.KindLimit, Message: "Check the code qecr_abc"},
	} {
		e := errorOf(t, &handler{err: err}, in, "make", "--settings", "a.json")
		if e.Kind != machines.KindDefect || e.Message != "Fix the machine adapter: its error message for make contains what looks like a run code or the start data, so the machine adapter wrote this message in its place" {
			t.Errorf("the error document is %#v", e)
		}
	}
}

// TestContainsSecret checks that the run code prefix is found in base64 at each byte
// offset, whatever bytes surround it.
func TestContainsSecret(t *testing.T) {
	for offset := range 3 {
		for _, fill := range []byte{0x00, 0x5a, 0xff} {
			for tail := range 3 {
				b := append(bytes.Repeat([]byte{fill}, offset), "qecr_"...)
				b = append(b, bytes.Repeat([]byte{fill ^ 0xa5}, tail)...)
				for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding} {
					if text := enc.EncodeToString(b); !machines.ContainsSecret(text, nil) {
						t.Errorf("offset %d, fill %#x, tail %d: %s is not found", offset, fill, tail, text)
					}
				}
			}
		}
	}
	for _, text := range []string{
		"Raise the instance quota of the account.",
		"Add the size to the adapter's settings: the adapter has no size of this name",
		"Check the subnet subnet-0123456789abcdef0 and the image ami-0123456789abcdef0.",
	} {
		if machines.ContainsSecret(text, []byte("start")) {
			t.Errorf("%q is found", text)
		}
	}
}

// TestContainsSecretWindows checks the pieces of the start data in base64 and of the
// run code after its prefix.
func TestContainsSecretWindows(t *testing.T) {
	data := []byte(`{"version":1,"run_code":"qecr_MARKERc0ffee7a1b","gateway":{"url":"https://gateway.example.com:8443","address":"10.0.1.5"}}`)
	enc := base64.StdEncoding.EncodeToString(data)
	for _, text := range []string{
		"Check the start data " + enc[100:116],
		"Check the start data " + enc[len(enc)-16:],
		"Check " + enc[57:80] + " now",
		"Check the code MARKERc0ffee7a1b",
		"Check the code ARKERc0ffee7a now",
		"Check the code ERc0ffee7a1b.",
	} {
		if !machines.ContainsSecret(text, data) {
			t.Errorf("a piece is not found in %q", text[:20])
		}
	}
	for _, text := range []string{
		"Check the start data " + enc[100:115],
		"Check the code Rc0ffee7a1b.",
		"Raise the instance quota of the account.",
	} {
		if machines.ContainsSecret(text, data) {
			t.Errorf("%q is found", text[:20])
		}
	}
	if machines.ContainsSecret("Check the code MARKERc0ffee7a1b", []byte("not json")) {
		t.Error("a run code is found in start data that is not JSON")
	}
	short := []byte(`{"run_code":"qecr_abc"}`)
	if !machines.ContainsSecret("Check abc.", short) {
		t.Error("a run code shorter than the window is not found")
	}
}
