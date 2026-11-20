package adapterio_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/integrations/contracts"
)

// understandCase is a valid document of machines/v1, a pointer that the document may
// list in must_understand because its decoder defines it, and the places that take an
// undefined member: the root, and each object below it.
type understandCase struct {
	name    string
	json    string
	defined string
	nested  []string
}

// understandCases is one valid document for each of the 11 decoders.
var understandCases = []understandCase{
	{"check", `{"version": 1}`, "/version", nil},
	{"check-answer", `{"offers": [{"vcpus": 2, "memory_mib": 4096, "disk_gib": 30, "arch": "amd64", "virtualization": false}]}`, "/offers", []string{"offers.0"}},
	{"make", `{"version": 1, "dispatch_id": "dp_0123456789abcdef", "pool": "np_0123456789abcdef", "machine_spec": {"vcpus": 2, "memory_mib": 4096, "disk_gib": 30, "arch": "amd64", "virtualization": false}, "start_data": "eA=="}`, "/start_data", []string{"machine_spec"}},
	{"make-answer", `{"id": "i-1", "machine_type": "m7i.large"}`, "/machine_type", nil},
	{"list", `{"version": 1, "pool": "np_0123456789abcdef"}`, "/pool", nil},
	{"list-answer", `{"machines": [{"id": "i-1", "dispatch_id": "dp_0123456789abcdef", "state": "running", "started": "2026-10-22T10:00:00Z"}]}`, "/machines/0/state", []string{"machines.0"}},
	{"log", `{"version": 1, "id": "i-1"}`, "/id", nil},
	{"log-answer", `{"log": "boot\n"}`, "/log", nil},
	{"delete", `{"version": 1, "id": "i-1"}`, "/id", nil},
	{"delete-answer", `{"state": "gone"}`, "/state", nil},
	{"error", `{"error": {"kind": "other", "message": "m"}}`, "/error/kind", []string{"error"}},
}

// with returns the JSON object doc with member set to value, below the places in path.
func with(t *testing.T, doc string, path []string, member string, value any) string {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal([]byte(doc), &root); err != nil {
		t.Fatal(err)
	}
	at := root
	for _, p := range path {
		switch next := at[p].(type) {
		case map[string]any:
			at = next
		case []any:
			at = next[0].(map[string]any)
		}
	}
	at[member] = value
	b, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestADecoderIgnoresAnUndefinedMember adds a member that machines/v1 does not define to
// each valid document, at the root and in each object below it: the decoder reads the
// same value as for the document without it, also when must_understand lists only
// members that the decoder defines.
func TestADecoderIgnoresAnUndefinedMember(t *testing.T) {
	for _, c := range understandCases {
		if err := decoders[c.name]([]byte(c.json)); err != nil {
			t.Fatalf("%s: the valid document fails: %v", c.name, err)
		}
		places := [][]string{nil}
		for _, n := range c.nested {
			places = append(places, strings.Split(n, "."))
		}
		for _, path := range places {
			doc := with(t, c.json, path, "extra", "an example value")
			if err := decoders[c.name]([]byte(doc)); err != nil {
				t.Errorf("%s with an undefined member at %v: %v", c.name, path, err)
			}
			doc = with(t, doc, nil, "must_understand", []string{c.defined})
			if err := decoders[c.name]([]byte(doc)); err != nil {
				t.Errorf("%s with an undefined member at %v and the pointer %s: %v", c.name, path, c.defined, err)
			}
		}
	}
	// The value read is the one of the document without the member.
	b := []byte(with(t, understandCases[2].json, []string{"machine_spec"}, "gpus", 1))
	got, data, err := adapterio.DecodeMake(b)
	plain, plainData, _ := adapterio.DecodeMake([]byte(understandCases[2].json))
	if err != nil || !reflect.DeepEqual(got, plain) || string(data) != string(plainData) {
		t.Errorf("a make request with an undefined member: %+v, %v", got, err)
	}
}

// thirtyThree is 33 different pointers, one more than a must_understand may list.
func thirtyThree() []string {
	var p []string
	for i := range 33 {
		p = append(p, fmt.Sprintf("/extra%d", i))
	}
	return p
}

// TestADecoderRefusesAnUnmetPointer lists a pointer in must_understand that the decoder
// does not define, at the root and below it, or a must_understand that is not a list of
// pointers: each fails with the one text for it, which quotes no pointer.
func TestADecoderRefusesAnUnmetPointer(t *testing.T) {
	const want = "the member must_understand lists a pointer that version 1 does not define, or is not a list"
	for _, c := range understandCases {
		for name, must := range map[string]any{
			"an undefined member": []string{"/extra"},
			"after a defined one": []string{c.defined, "/extra"},
			"below a defined one": []string{c.defined + "/extra"},
			"the list itself":     []string{"/must_understand"},
			"not a list":          "/" + "extra",
			"a pointer with no /": []string{"extra"},
			"a repeat":            []string{c.defined, c.defined},
			"a bad escape":        []string{"/a~2"},
			"a line break":        []string{"/a\nb"},
			"33 pointers":         thirtyThree(),
		} {
			doc := with(t, c.json, nil, "must_understand", must)
			err := decoders[c.name]([]byte(doc))
			if err == nil || err.Error() != want {
				t.Errorf("%s, %s: the error is %v", c.name, name, err)
			}
		}
	}
}

// TestADecoderStillRefusesWhatItDefines checks that the guards stay: a document that is
// followed by another, a document that is not an object, and a known member of the wrong
// type or outside its rule fail, whatever else the document lists.
func TestADecoderStillRefusesWhatItDefines(t *testing.T) {
	for _, c := range understandCases {
		for name, doc := range map[string]string{
			"trailing data":         c.json + ` {}`,
			"two objects":           c.json + c.json,
			"a repeated member":     strings.Replace(c.json, "{", `{"extra": 1, "extra": 2, `, 1),
			"an array":              `[` + c.json + `]`,
			"an undefined member":   `{"extra": 1}`,
			"a null in place of it": `null`,
		} {
			if err := decoders[c.name]([]byte(doc)); err == nil {
				t.Errorf("%s, %s: accepted", c.name, name)
			}
		}
	}
	for _, doc := range []string{
		`{"version": "1", "extra": 1}`,
		`{"version": 2, "extra": 1}`,
		`{"extra": 1}`,
	} {
		if _, err := adapterio.DecodeCheck([]byte(doc)); err == nil {
			t.Errorf("a check request %s is accepted", doc)
		}
	}
	for name, doc := range map[string]string{
		"a spec member of the wrong type": with(t, understandCases[2].json, []string{"machine_spec"}, "vcpus", "2"),
		"a spec member out of range":      with(t, understandCases[2].json, []string{"machine_spec"}, "vcpus", 0),
		"an undefined member and a pool":  with(t, with(t, understandCases[2].json, []string{"machine_spec"}, "gpus", 1), nil, "pool", 7),
	} {
		if _, _, err := adapterio.DecodeMake([]byte(doc)); err == nil {
			t.Errorf("a make request with %s is accepted", name)
		}
	}
}

// TestServeReadsAnUndefinedMemberAndRefusesAnUnmetPointer sends Serve a request with an
// undefined member, which it serves, and one whose must_understand lists a pointer that
// version 1 does not define, which it answers with a failure of kind defect and no call
// of the handler.
func TestServeReadsAnUndefinedMemberAndRefusesAnUnmetPointer(t *testing.T) {
	args := []string{"make", "--settings", "a.json"}
	request := understandCases[2].json
	h := &handler{}
	if status, out := serve(h, with(t, request, nil, "extra", true), args...); status != adapterio.ExitOK || out != `{"id":"i-0123456789abcdef0"}`+"\n" {
		t.Fatalf("a request with an undefined member: %d %q", status, out)
	}
	if h.settings == "" {
		t.Error("the handler was not called")
	}

	h = &handler{}
	status, out := serve(h, with(t, request, nil, "must_understand", []string{"/extra"}), args...)
	doc, err := adapterio.DecodeError([]byte(out))
	if status != adapterio.ExitFailure || err != nil || doc.Error.Kind != contracts.FailureDefect || h.settings != "" {
		t.Fatalf("a request with an unmet pointer: %d %q %v, handler called: %v", status, out, err, h.settings != "")
	}
	if strings.Contains(out, "extra") {
		t.Errorf("the failure quotes the pointer: %s", out)
	}
}
