package adapterio_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/integrations/contracts"
)

// TestEncodeRoundTrip checks that every document that Encode writes decodes to the
// same value.
func TestEncodeRoundTrip(t *testing.T) {
	data := []byte{0, 1, 2, 250}
	cases := []struct {
		doc    any
		decode func([]byte) (any, error)
	}{
		{contracts.MachinesCheck{Version: 1}, func(b []byte) (any, error) { return adapterio.DecodeCheck(b) }},
		{contracts.MachinesCheckAnswer{}, func(b []byte) (any, error) { return adapterio.DecodeCheckAnswer(b) }},
		{contracts.MachinesMake{Version: 1, Key: "dp_0123456789abcdef", Pool: "np_0123456789abcdef", Size: "standard", StartData: base64.StdEncoding.EncodeToString(data)}, func(b []byte) (any, error) {
			r, got, err := adapterio.DecodeMake(b)
			if !bytes.Equal(got, data) {
				t.Errorf("the start data is %v, want %v", got, data)
			}
			return r, err
		}},
		{contracts.MachinesMakeAnswer{ID: "i-0123456789abcdef0"}, func(b []byte) (any, error) { return adapterio.DecodeMakeAnswer(b) }},
		{contracts.MachinesList{Version: 1, Pool: "np_0123456789abcdef"}, func(b []byte) (any, error) { return adapterio.DecodeList(b) }},
		{contracts.MachinesListAnswer{Machines: []contracts.Machine{{ID: "i-1", Key: "dp_0123456789abcdef", State: contracts.MachineStopped, Started: "2026-10-22T10:00:00Z"}}}, func(b []byte) (any, error) { return adapterio.DecodeListAnswer(b) }},
		{contracts.MachinesListAnswer{Machines: []contracts.Machine{}}, func(b []byte) (any, error) { return adapterio.DecodeListAnswer(b) }},
		{contracts.MachinesLog{Version: 1, ID: "i-1"}, func(b []byte) (any, error) { return adapterio.DecodeLog(b) }},
		{contracts.MachinesLogAnswer{Log: "boot\n"}, func(b []byte) (any, error) { return adapterio.DecodeLogAnswer(b) }},
		{contracts.MachinesDelete{Version: 1, ID: "i-1"}, func(b []byte) (any, error) { return adapterio.DecodeDelete(b) }},
		{contracts.MachinesDeleteAnswer{State: contracts.MachineGone}, func(b []byte) (any, error) { return adapterio.DecodeDeleteAnswer(b) }},
		{contracts.MachinesError{Error: contracts.MachinesFailure{Kind: contracts.FailureLimit, Message: "Raise the quota."}}, func(b []byte) (any, error) { return adapterio.DecodeError(b) }},
	}
	for _, c := range cases {
		b, err := adapterio.Encode(c.doc)
		if err != nil {
			t.Fatalf("%T: %v", c.doc, err)
		}
		got, err := c.decode(b)
		if err != nil {
			t.Fatalf("%T: %s: %v", c.doc, b, err)
		}
		if !reflect.DeepEqual(got, c.doc) {
			t.Errorf("%T: decoded %#v, want %#v", c.doc, got, c.doc)
		}
	}
	if b, _ := adapterio.Encode(contracts.MachinesListAnswer{}); string(b) != `{"machines":[]}` {
		t.Errorf("an empty list answer is %s", b)
	}
}

// TestEncodeRefusesInvalid checks that Encode refuses a document that fails a rule.
func TestEncodeRefusesInvalid(t *testing.T) {
	for _, doc := range []any{
		contracts.MachinesCheck{},
		contracts.MachinesMake{Version: 1, Key: "dp_0123456789abcdef", Pool: "np_0123456789abcdef", Size: "s"},
		contracts.MachinesMake{Version: 1, Key: "dp_0123456789abcdef", Pool: "np_0123456789abcdef", Size: "s", StartData: base64.StdEncoding.EncodeToString(make([]byte, contracts.MaxStartData+1))},
		contracts.MachinesMakeAnswer{ID: adapterio.NoID},
		contracts.MachinesListAnswer{Machines: []contracts.Machine{{ID: "i", Key: "dp_0123456789abcdef", State: contracts.MachineGone, Started: "2026-10-22T10:00:00Z"}}},
		contracts.MachinesListAnswer{Machines: []contracts.Machine{{ID: "i", Key: "dp_0123456789abcdef", State: contracts.MachineRunning, Started: "10000-01-01T00:00:00Z"}}},
		contracts.MachinesDeleteAnswer{State: contracts.MachineRunning},
		contracts.MachinesLogAnswer{Log: "\xff"},
		contracts.MachinesLogAnswer{Log: "boot\x00"},
		contracts.MachinesError{Error: contracts.MachinesFailure{Kind: "fatal", Message: "m"}},
		contracts.MachinesError{Error: contracts.MachinesFailure{Kind: contracts.FailureOther, Message: strings.Repeat("m", adapterio.MaxMessage+1)}},
		&contracts.MachinesCheck{Version: 1},
	} {
		if b, err := adapterio.Encode(doc); err == nil {
			t.Errorf("%#v encoded as %s", doc, b)
		}
	}
}

// handler is an [adapterio.Handler] that records its calls.
type handler struct {
	settings  string
	make      contracts.MachinesMake
	startData []byte
	err       error
	answer    contracts.MachinesLogAnswer
	list      contracts.MachinesListAnswer
}

func (h *handler) Check(_ context.Context, settings string, _ contracts.MachinesCheck) (contracts.MachinesCheckAnswer, error) {
	h.settings = settings
	return contracts.MachinesCheckAnswer{}, h.err
}

func (h *handler) Make(_ context.Context, settings string, req contracts.MachinesMake, startData []byte) (contracts.MachinesMakeAnswer, error) {
	h.settings, h.make, h.startData = settings, req, startData
	return contracts.MachinesMakeAnswer{ID: "i-0123456789abcdef0"}, h.err
}

func (h *handler) List(context.Context, string, contracts.MachinesList) (contracts.MachinesListAnswer, error) {
	return h.list, h.err
}

func (h *handler) Log(context.Context, string, contracts.MachinesLog) (contracts.MachinesLogAnswer, error) {
	return h.answer, h.err
}

func (h *handler) Delete(context.Context, string, contracts.MachinesDelete) (contracts.MachinesDeleteAnswer, error) {
	return contracts.MachinesDeleteAnswer{State: contracts.MachineDeleting}, h.err
}

// serve runs Serve with args and the request in, and returns the exit status and the
// output.
func serve(h adapterio.Handler, in string, args ...string) (int, string) {
	var out bytes.Buffer
	status := adapterio.Serve(context.Background(), args, strings.NewReader(in), &out, h)
	return status, out.String()
}

func TestServe(t *testing.T) {
	h := &handler{}
	status, out := serve(h, `{"version": 1, "key": "dp_0123456789abcdef", "pool": "np_0123456789abcdef", "size": "standard", "start_data": "AAEC+g=="}`, "make", "--settings", "/etc/adapter.json")
	if status != adapterio.ExitOK || out != `{"id":"i-0123456789abcdef0"}`+"\n" {
		t.Fatalf("make: %d %q", status, out)
	}
	want := contracts.MachinesMake{Version: 1, Key: "dp_0123456789abcdef", Pool: "np_0123456789abcdef", Size: "standard", StartData: "AAEC+g=="}
	if h.settings != "/etc/adapter.json" || h.make != want || !bytes.Equal(h.startData, []byte{0, 1, 2, 250}) {
		t.Errorf("the handler received %q, %#v, %v", h.settings, h.make, h.startData)
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
		if status != adapterio.ExitFailure {
			t.Errorf("%q: status %d", args, status)
		}
		doc, err := adapterio.DecodeError([]byte(out))
		if err != nil || doc.Error.Kind != contracts.FailureDefect || !strings.HasPrefix(doc.Error.Message, "Run the machine adapter as <command> --settings FILE") {
			t.Errorf("%q: %q, %v", args, out, err)
		}
	}
}

// errorOf runs Serve and returns the failure of the error document that it wrote with
// status 1.
func errorOf(t *testing.T, h adapterio.Handler, in string, args ...string) contracts.MachinesFailure {
	t.Helper()
	status, out := serve(h, in, args...)
	if status != adapterio.ExitFailure {
		t.Fatalf("status %d, output %q", status, out)
	}
	doc, err := adapterio.DecodeError([]byte(out))
	if err != nil {
		t.Fatalf("%q: %v", out, err)
	}
	return doc.Error
}

func TestServeFailures(t *testing.T) {
	args := []string{"log", "--settings", "a.json"}
	in := `{"version": 1, "id": "i-1"}`

	e := errorOf(t, &handler{err: &adapterio.Failure{Kind: contracts.FailureLimit, Message: "Raise the quota."}}, in, args...)
	if e != (contracts.MachinesFailure{Kind: contracts.FailureLimit, Message: "Raise the quota."}) {
		t.Errorf("a handler's *Failure became %#v", e)
	}
	e = errorOf(t, &handler{err: errors.New("Check the network.")}, in, args...)
	if e != (contracts.MachinesFailure{Kind: contracts.FailureOther, Message: "Check the network."}) {
		t.Errorf("a plain error became %#v", e)
	}
	e = errorOf(t, &handler{err: &adapterio.Failure{Kind: "fatal"}}, in, args...)
	if e.Kind != contracts.FailureOther || e.Message != "Fix the machine adapter: it failed with an empty message" {
		t.Errorf("an invalid *Failure became %#v", e)
	}
	e = errorOf(t, &handler{err: errors.New(strings.Repeat("é", 4096))}, in, args...)
	if e.Message != strings.Repeat("é", adapterio.MaxMessage) {
		t.Errorf("a message of 4096 characters became %d characters", utf8.RuneCountInString(e.Message))
	}
	e = errorOf(t, &handler{err: errors.New("Check\x00 the\xff network.")}, in, args...)
	if e.Message != "Check� the� network." {
		t.Errorf("a message with U+0000 and invalid UTF-8 became %q", e.Message)
	}
	e = errorOf(t, &handler{}, `{"version": 2, "id": "i-1"}`, args...)
	if e.Kind != contracts.FailureDefect || e.Message != "Report this defect in the provisioner: its log request to the machine adapter is not valid: the member version must be 1" {
		t.Errorf("a bad request became %#v", e)
	}
	e = errorOf(t, &handler{}, strings.Repeat(" ", contracts.MaxMachinesAnswer+1), args...)
	if e.Kind != contracts.FailureDefect || e.Message != "Report this defect in the provisioner: its log request to the machine adapter is larger than 1048576 bytes" {
		t.Errorf("a large request became %#v", e)
	}
	e = errorOf(t, &handler{answer: contracts.MachinesLogAnswer{Log: strings.Repeat("a", adapterio.MaxLog+1)}}, in, args...)
	if e.Kind != contracts.FailureDefect || e.Message != "Fix the machine adapter: its log answer is not valid: the member log must have at most 786432 characters" {
		t.Errorf("an invalid answer became %#v", e)
	}
	many := make([]contracts.Machine, 12000)
	for i := range many {
		many[i] = contracts.Machine{ID: "i-0123456789abcdef0", Key: "dp_0123456789abcdef", State: contracts.MachineRunning, Started: "2026-10-22T10:00:00Z"}
	}
	e = errorOf(t, &handler{list: contracts.MachinesListAnswer{Machines: many}}, `{"version": 1, "pool": "np_0123456789abcdef"}`, "list", "--settings", "a.json")
	if e.Kind != contracts.FailureDefect || e.Message != "Fix the machine adapter: its list answer is larger than 1048576 bytes" {
		t.Errorf("a large answer became %#v", e)
	}
	e = errorOf(t, &handler{answer: contracts.MachinesLogAnswer{Log: strings.Repeat("\x01", adapterio.MaxLog)}}, in, args...)
	if e.Kind != contracts.FailureDefect || e.Message != "Fix the machine adapter: its log answer is larger than 1048576 bytes" {
		t.Errorf("a log answer larger than 1 MiB became %#v", e)
	}
}

// TestLogAnswer checks that LogAnswer keeps the end of the text within MaxLog bytes
// and an answer of at most 1 MiB, with U+0000 and invalid UTF-8 replaced.
func TestLogAnswer(t *testing.T) {
	head := strings.Repeat("h", 1000)
	tail := "é" + strings.Repeat("t", adapterio.MaxLog-2)
	for _, c := range []struct{ in, want string }{
		{"boot\n", "boot\n"},
		{"boot\x00\xff\n", "boot��\n"},
		{head + "x" + tail, "é" + strings.Repeat("t", adapterio.MaxLog-2)},
		{head + strings.Repeat("t", adapterio.MaxLog-1) + "é", strings.Repeat("t", adapterio.MaxLog-2) + "é"},
	} {
		got := adapterio.LogAnswer(c.in).Log
		if got != c.want {
			t.Errorf("%d bytes became %d bytes, want %d", len(c.in), len(got), len(c.want))
		}
	}
	for _, text := range []string{
		strings.Repeat("\x00", adapterio.MaxLog),
		strings.Repeat("\x01", adapterio.MaxLog+5),
		strings.Repeat("ab\x02", adapterio.MaxLog/3) + "end",
		strings.Repeat("é\"", adapterio.MaxLog),
	} {
		a := adapterio.LogAnswer(text)
		b, err := adapterio.Encode(a)
		whole := len(a.Log) > adapterio.MaxLog-utf8.UTFMax
		if err != nil || len(b)+1 > contracts.MaxMachinesAnswer || (!whole && len(b)+1 < contracts.MaxMachinesAnswer-6) || len(a.Log) > adapterio.MaxLog {
			t.Errorf("a text of %d bytes: an answer of %d bytes, a log of %d bytes, %v", len(text), len(b), len(a.Log), err)
		}
		if strings.HasSuffix(text, "end") && !strings.HasSuffix(a.Log, "end") {
			t.Error("the end of the text is not kept")
		}
		status, out := serve(&handler{answer: a}, `{"version": 1, "id": "i-1"}`, "log", "--settings", "a.json")
		if status != adapterio.ExitOK || len(out) > contracts.MaxMachinesAnswer {
			t.Errorf("Serve: status %d, %d bytes", status, len(out))
		}
	}
}

func TestServeRefusesSecretMessage(t *testing.T) {
	data := []byte(`{"version":1,"run_code":"qecr_MARKERc0ffee7a1b","gateway":{"url":"https://gateway.example.com:8443","address":"10.0.1.5"}}`)
	enc := base64.StdEncoding.EncodeToString(data)
	in := `{"version": 1, "key": "dp_0123456789abcdef", "pool": "np_0123456789abcdef", "size": "standard", "start_data": "` + enc + `"}`
	for _, err := range []error{
		errors.New("Check the start data " + enc[:80]),
		errors.New("Check the start data " + string(data)),
		errors.New(strings.Repeat("m", 300) + " qecr_abc"),
		&adapterio.Failure{Kind: contracts.FailureLimit, Message: "Check the code qecr_abc"},
	} {
		e := errorOf(t, &handler{err: err}, in, "make", "--settings", "a.json")
		if e.Kind != contracts.FailureDefect || e.Message != "Fix the machine adapter: its error message for make contains what looks like a run code or the start data, so the machine adapter wrote this message in its place" {
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
					if text := enc.EncodeToString(b); !adapterio.ContainsSecret(text, nil) {
						t.Errorf("offset %d, fill %#x, tail %d: %s is not found", offset, fill, tail, text)
					}
				}
			}
		}
	}
	for _, text := range []string{"QECR_abc", "Qecr_abc", "qecr_"} {
		if !adapterio.ContainsSecret(text, nil) {
			t.Errorf("%q is not found", text)
		}
	}
	for _, text := range []string{
		"Raise the instance quota of the account.",
		"Add the size to the adapter's settings: the adapter has no size of this name",
		"Check the subnet subnet-0123456789abcdef0 and the image ami-0123456789abcdef0.",
	} {
		if adapterio.ContainsSecret(text, []byte("start")) {
			t.Errorf("%q is found", text)
		}
	}
}

// TestContainsSecretWindows checks the raw start data, the pieces of the start data in
// base64 and of the run code after its prefix.
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
		if !adapterio.ContainsSecret(text, data) {
			t.Errorf("a piece is not found in %q", text[:20])
		}
	}
	if !adapterio.ContainsSecret("Check the user data start", []byte("start")) {
		t.Error("the raw start data is not found")
	}
	for _, text := range []string{
		"Check the start data " + enc[100:115],
		"Check the code Rc0ffee7a1b.",
		"Raise the instance quota of the account.",
	} {
		if adapterio.ContainsSecret(text, data) {
			t.Errorf("%q is found", text[:20])
		}
	}
	if adapterio.ContainsSecret("Check the code MARKERc0ffee7a1b", []byte("not json")) {
		t.Error("a run code is found in start data that is not JSON")
	}
	short := []byte(`{"run_code":"qecr_abc"}`)
	if !adapterio.ContainsSecret("Check abc.", short) {
		t.Error("a run code shorter than the window is not found")
	}
}
