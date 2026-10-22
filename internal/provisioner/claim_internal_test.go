package provisioner

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// claimRefused is the error of a claim answer that fails the schema.
const claimRefused = "it fails claim-answer.schema.json"

// testCode is a run code of the contract's form.
const testCode = "qecr_01JB8Z3K5Q7R9T2V4W6X8Y0A1C.uoES-kuj1vk0sq0qoGlmAg"

// aClaim is a claim's 200 at [Revision] with a machine. Each case changes it in one
// place.
func aClaim() map[string]any {
	return map[string]any{
		"version": 1, "revision": Revision, "dispatch_id": "dp_8w3k0q9t2v7m4r6x", "pool_id": "np_f1xt0re000000000",
		"lease_until": "2026-10-15T12:05:00Z", "machine": "small", "run_code": testCode,
		"gateway_url": "https://gateway.example.com:8443", "max_lifetime_minutes": 120,
	}
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAClaimIsRead reads a claim with a machine and one without: every member is the
// answer's, and the run code is written whole.
func TestAClaimIsRead(t *testing.T) {
	cl, err := readClaim(marshal(t, aClaim()))
	if err != nil {
		t.Fatal(err)
	}
	var w bytes.Buffer
	_, _ = cl.RunCode.WriteTo(&w)
	if cl.DispatchID != "dp_8w3k0q9t2v7m4r6x" || cl.PoolID != "np_f1xt0re000000000" || cl.LeaseUntil.Format("15:04:05") != "12:05:00" ||
		cl.Machine != "small" || w.String() != testCode || cl.GatewayURL != "https://gateway.example.com:8443" || cl.MaxLifetimeMinutes != 120 {
		t.Errorf("claimed %+v, run code %q", cl, w.String())
	}
	d := aClaim()
	delete(d, "machine")
	if cl, err := readClaim(marshal(t, d)); err != nil || cl.Machine != "" {
		t.Errorf("no machine: %+v %v", cl, err)
	}
}

// TestAClaimOfAnotherFormIsRefused changes a claim in one place each: a member the
// schema does not define, another revision, a machine, a gateway URL or a lifetime
// outside its form, null for a member, or a member missing. Each fails, with no run
// code, and the error quotes no value.
func TestAClaimOfAnotherFormIsRefused(t *testing.T) {
	for name, edit := range map[string]func(d map[string]any){
		"a run id": func(d map[string]any) { d["run_id"] = "0192f0c1-7d4e-7a2b-8c3d-4e5f6a7b8c9d" },
		"a step":   func(d map[string]any) { d["step"] = "explore" },
		"a target": func(d map[string]any) {
			d["target"] = map[string]any{"system": "code.example.com", "path": "acme/shop"}
		},
		"the next revision":        func(d map[string]any) { d["revision"] = Revision + 1 },
		"revision 0":               func(d map[string]any) { d["revision"] = 0 },
		"machine empty":            func(d map[string]any) { d["machine"] = "" },
		"machine null":             func(d map[string]any) { d["machine"] = nil },
		"machine in upper case":    func(d map[string]any) { d["machine"] = "Small" },
		"gateway URL with a path":  func(d map[string]any) { d["gateway_url"] = "https://gateway.example.com/v1" },
		"gateway URL over http":    func(d map[string]any) { d["gateway_url"] = "http://gateway.example.com" },
		"gateway URL null":         func(d map[string]any) { d["gateway_url"] = nil },
		"no gateway URL":           func(d map[string]any) { delete(d, "gateway_url") },
		"a lifetime of 14":         func(d map[string]any) { d["max_lifetime_minutes"] = 14 },
		"a lifetime of 1471":       func(d map[string]any) { d["max_lifetime_minutes"] = 1471 },
		"a lifetime as a string":   func(d map[string]any) { d["max_lifetime_minutes"] = "120" },
		"no lifetime":              func(d map[string]any) { delete(d, "max_lifetime_minutes") },
		"no lease":                 func(d map[string]any) { delete(d, "lease_until") },
		"a lease with a fraction":  func(d map[string]any) { d["lease_until"] = "2026-10-15T12:05:00.5Z" },
		"a dispatch id upper case": func(d map[string]any) { d["dispatch_id"] = "dp_8W3K0Q9T2V7M4R6X" },
	} {
		cl, err := readClaim(marshal(t, func() map[string]any { d := aClaim(); edit(d); return d }()))
		if err == nil || err.Error() != claimRefused || cl.RunCode.Len() != 0 {
			t.Errorf("%s: got %v, want %s", name, err, claimRefused)
		}
	}
}

// TestTheRunCodeIsReadFromTheBytes reads a claim whose run code is written with an
// escape: the run code is the member's, unescaped. Clearing the answer after leaves the
// run code whole, so it shares no bytes with the answer.
func TestTheRunCodeIsReadFromTheBytes(t *testing.T) {
	d := aClaim()
	d["run_code"] = "RUNCODE"
	// The body writes the q of qecr_ as a JSON escape: a backslash, then u0071. The
	// backslash is built from its code, 92, so that the source holds no escape that a
	// tool which edits the source could decode.
	escaped := `"` + string(rune(92)) + "u0071" + testCode[1:] + `"`
	body := bytes.Replace(marshal(t, d), []byte(`"RUNCODE"`), []byte(escaped), 1)
	if bytes.Contains(body, []byte(testCode)) {
		t.Fatal("the body has the run code unescaped")
	}
	cl, err := readClaim(body)
	if err != nil {
		t.Fatal(err)
	}
	clear(body)
	var w bytes.Buffer
	_, _ = cl.RunCode.WriteTo(&w)
	if w.String() != testCode {
		t.Errorf("the run code %q", w.String())
	}
}

// TestARunCodeOutsideItsShapeIsRefused reads claims whose run code is outside its form
// once unescaped, is not a string, or is missing: each is an error that quotes none of
// it. A run code with two fingerprints is read.
func TestARunCodeOutsideItsShapeIsRefused(t *testing.T) {
	for _, code := range []string{
		`"qecr_01JB8Z3K5Q7R9T2V4W6X8Y0A1C"`, `"qecr_01jb8z3k5q7r9t2v4w6x8y0a1c.uoES-kuj1vk0sq0qoGlmAg"`,
		`"qecr_01JB8Z3K5Q7R9T2V4W6X8Y0A1I.uoES-kuj1vk0sq0qoGlmAg"`, `"qecr_01JB8Z3K5Q7R9T2V4W6X8Y0A1C.uoES-kuj1vk0sq0qoGlmAg\n"`,
		`"qecr_01JB8Z3K5Q7R9T2V4W6X8Y0A1C.uoES-kuj1vk0sq0qoGlmAg.uoES-kuj1vk0sq0qoGlmAg.uoES-kuj1vk0sq0qoGlmAg"`,
		`"qecn_01JB8Z3K5Q7R9T2V4W6X8Y0A1C.uoES-kuj1vk0sq0qoGlmAg"`,
		`7`, `true`, `null`, `{"qecr_a":"b"}`,
	} {
		d := aClaim()
		d["run_code"] = "RUNCODE"
		body := bytes.Replace(marshal(t, d), []byte(`"RUNCODE"`), []byte(code), 1)
		_, err := readClaim(body)
		if err == nil || strings.Contains(strings.ToLower(err.Error()), "qecr_") {
			t.Errorf("%s: %v", code, err)
		}
	}
	d := aClaim()
	delete(d, "run_code")
	if _, err := readClaim(marshal(t, d)); err == nil {
		t.Error("no run code")
	}
	d = aClaim()
	d["run_code"] = testCode + ".uoES-kuj1vk0sq0qoGlmAg"
	if _, err := readClaim(marshal(t, d)); err != nil {
		t.Errorf("a run code with two fingerprints: %v", err)
	}
	if _, _, ok := memberSpan([]byte(`{"x":{"run_code":"qecr_a"}}`), "run_code"); ok {
		t.Error("a nested run_code is taken for the answer's")
	}
}

// TestALargeAnswerIsReadIntoOneBuffer reads a body by its length and with none: within
// the bound it is whole, over it it is too large.
func TestALargeAnswerIsReadIntoOneBuffer(t *testing.T) {
	for _, length := range []int64{-1, 10} {
		b, large, err := readLimited(strings.NewReader("0123456789"), length, 10)
		if err != nil || large || string(b) != "0123456789" || cap(b) != 10 {
			t.Errorf("length %d: %q %v %v cap %d", length, b, large, err, cap(b))
		}
		if _, large, _ := readLimited(strings.NewReader("0123456789x"), length, 10); !large && length < 0 {
			t.Errorf("length %d: over the bound is read", length)
		}
	}
}

// TestFitLogCountsTheEscapedBytes cuts logs whose escaped size is just within and just
// over the request's bound: within is whole, over is cut at a line.
func TestFitLogCountsTheEscapedBytes(t *testing.T) {
	budget := 1<<20 - logOverhead
	// Each "<" is 6 bytes escaped and each line feed 2.
	line := strings.Repeat("<", 9) + "\n"
	lines := budget / 56
	within := strings.Repeat(line, lines)
	if part, cut := fitLog(within); cut || part != within {
		t.Errorf("within: cut %v, %d of %d bytes", cut, len(part), len(within))
	}
	over := line + within
	if part, cut := fitLog(over); !cut || part != within {
		t.Errorf("over: cut %v, %d of %d bytes", cut, len(part), len(within))
	}
}
