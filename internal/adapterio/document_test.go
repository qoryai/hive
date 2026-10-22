package machines_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"io/fs"
	"strings"
	"testing"

	"github.com/qoryai/hive/machines"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// decoders are the Go decoders by schema file name.
var decoders = map[string]func([]byte) error{
	"check-request":  func(b []byte) error { _, err := machines.DecodeCheckRequest(b); return err },
	"check-answer":   func(b []byte) error { _, err := machines.DecodeCheckAnswer(b); return err },
	"make-request":   func(b []byte) error { _, err := machines.DecodeMakeRequest(b); return err },
	"make-answer":    func(b []byte) error { _, err := machines.DecodeMakeAnswer(b); return err },
	"list-request":   func(b []byte) error { _, err := machines.DecodeListRequest(b); return err },
	"list-answer":    func(b []byte) error { _, err := machines.DecodeListAnswer(b); return err },
	"log-request":    func(b []byte) error { _, err := machines.DecodeLogRequest(b); return err },
	"log-answer":     func(b []byte) error { _, err := machines.DecodeLogAnswer(b); return err },
	"delete-request": func(b []byte) error { _, err := machines.DecodeDeleteRequest(b); return err },
	"delete-answer":  func(b []byte) error { _, err := machines.DecodeDeleteAnswer(b); return err },
	"error":          func(b []byte) error { _, err := machines.DecodeErrorAnswer(b); return err },
}

// compile compiles every schema of [machines.Schemas], with format asserted.
func compile(t *testing.T) map[string]*jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	names, err := fs.Glob(machines.Schemas, "*.schema.json")
	if err != nil || len(names) != len(decoders)+1 {
		t.Fatalf("the schemas are %v, %v; want %d files", names, err, len(decoders)+1)
	}
	for _, name := range names {
		b, err := fs.ReadFile(machines.Schemas, name)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if id, _ := doc.(map[string]any)["$id"].(string); id != machines.SchemaBase+"/"+name {
			t.Fatalf("%s has the $id %q", name, id)
		}
		if err := c.AddResource(machines.SchemaBase+"/"+name, doc); err != nil {
			t.Fatal(err)
		}
	}
	out := map[string]*jsonschema.Schema{}
	for name := range decoders {
		s, err := c.Compile(machines.SchemaBase + "/" + name + ".schema.json")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out[name] = s
	}
	return out
}

// fixture is one document and whether version 1 accepts it.
type fixture struct {
	schema string
	json   string
	ok     bool
}

// b64 is standard base64 of n bytes.
func b64(n int) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'x'}, n)) }

func fixtures() []fixture {
	makeReq := func(key, pool, size, data string) string {
		return `{"version": 1, "key": ` + key + `, "pool": ` + pool + `, "size": ` + size + `, "start_data": ` + data + `}`
	}
	good := `"` + b64(10) + `"`
	machine := func(id, key, state, started string) string {
		return `{"machines": [{"id": ` + id + `, "key": ` + key + `, "state": ` + state + `, "started": ` + started + `}]}`
	}
	t0 := `"2026-10-22T10:00:00Z"`
	f := []fixture{
		{"check-request", `{"version": 1}`, true},
		{"check-request", ` {"version": 1.0} `, true},
		{"check-request", `{"version": 1e0}`, true},
		{"check-request", `{"version": 2}`, false},
		{"check-request", `{"version": "1"}`, false},
		{"check-request", `{"version": null}`, false},
		{"check-request", `{}`, false},
		{"check-request", `{"version": 1, "extra": true}`, false},
		{"check-request", `[]`, false},
		{"check-request", `null`, false},
		{"check-request", `"x"`, false},

		{"check-answer", `{}`, true},
		{"check-answer", `{"ok": true}`, false},
		{"check-answer", `[]`, false},

		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"standard"`, good), true},
		{"make-request", makeReq(`"dp_vwxyz0123456789a"`, `"np_hjkmnpqrstvwxyz0"`, `"s"`, good), true},
		{"make-request", makeReq(`"dp_0123456789abcde"`, `"np_0123456789abcdef"`, `"s"`, good), false},
		{"make-request", makeReq(`"dp_0123456789abcdef0"`, `"np_0123456789abcdef"`, `"s"`, good), false},
		{"make-request", makeReq(`"dp_0123456789abcdei"`, `"np_0123456789abcdef"`, `"s"`, good), false},
		{"make-request", makeReq(`"dp_0123456789abcdeu"`, `"np_0123456789abcdef"`, `"s"`, good), false},
		{"make-request", makeReq(`"dp_0123456789ABCDEF"`, `"np_0123456789abcdef"`, `"s"`, good), false},
		{"make-request", makeReq(`"np_0123456789abcdef"`, `"np_0123456789abcdef"`, `"s"`, good), false},
		{"make-request", makeReq(`""`, `"np_0123456789abcdef"`, `"s"`, good), false},
		{"make-request", makeReq(`1`, `"np_0123456789abcdef"`, `"s"`, good), false},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"dp_0123456789abcdef"`, `"s"`, good), false},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdel"`, `"s"`, good), false},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdeo"`, `"s"`, good), false},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"`+"s"+strings.Repeat("a", 63)+`"`, good), true},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"`+"s"+strings.Repeat("a", 64)+`"`, good), false},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"Standard"`, good), false},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"1x"`, good), false},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `""`, good), false},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"x-1"`, good), true},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"s"`, `""`), false},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"s"`, `"`+b64(1)+`"`), true},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"s"`, `"`+b64(2)+`"`), true},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"s"`, `"`+b64(16382)+`"`), true},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"s"`, `"`+b64(16383)+`"`), true},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"s"`, `"`+b64(16384)+`"`), true},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"s"`, `"`+b64(16385)+`"`), false},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"s"`, `"`+b64(16386)+`"`), false},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"s"`, `"eA"`), false},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"s"`, `"eB=="`), false},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"s"`, `"eA=="`), true},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"s"`, `"eHg\n="`), false},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"s"`, `"eH-="`), false},
		{"make-request", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"s"`, `"eHh4"`), true},
		{"make-request", `{"version": 1, "key": "dp_0123456789abcdef", "pool": "np_0123456789abcdef", "size": "s"}`, false},
		{"make-request", `{"version": 2, "key": "dp_0123456789abcdef", "pool": "np_0123456789abcdef", "size": "s", "start_data": "eA=="}`, false},

		{"make-answer", `{"id": "i-0123456789abcdef0"}`, true},
		{"make-answer", `{"id": "-"}`, false},
		{"make-answer", `{"id": "--"}`, true},
		{"make-answer", `{"id": "a b"}`, false},
		{"make-answer", `{"id": ""}`, false},
		{"make-answer", `{"id": "` + strings.Repeat("i", 128) + `"}`, true},
		{"make-answer", `{"id": "` + strings.Repeat("i", 129) + `"}`, false},
		{"make-answer", `{"id": "é"}`, false},
		{"make-answer", `{"id": "i-1", "key": "d"}`, false},
		{"make-answer", `{"id": 7}`, false},

		{"list-request", `{"version": 1, "pool": "np_0123456789abcdef"}`, true},
		{"list-request", `{"version": 1}`, false},
		{"list-request", `{"version": 1, "pool": "np_0123456789abcdef", "id": "i"}`, false},

		{"list-answer", `{"machines": []}`, true},
		{"list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"starting"`, t0), true},
		{"list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `"2026-10-22T10:00:00.123456789Z"`), true},
		{"list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `"2026-10-22T10:00:00.5Z"`), true},
		{"list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `"2026-10-22T10:00:00+02:00"`), false},
		{"list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"stopped"`, `"2026-10-22T10:00:00+00:00"`), false},
		{"list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"deleting"`, t0), true},
		{"list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"gone"`, t0), false},
		{"list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"Running"`, t0), false},
		{"list-answer", machine(`"-"`, `"dp_0123456789abcdef"`, `"running"`, t0), false},
		{"list-answer", machine(`"i-1"`, `"d 1"`, `"running"`, t0), false},
		{"list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `"2026-10-22t10:00:00z"`), false},
		{"list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `"2026-10-22 10:00:00Z"`), false},
		{"list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `"2026-02-30T10:00:00Z"`), false},
		{"list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `"2026-13-01T10:00:00Z"`), false},
		{"list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `"2016-12-31T23:59:60Z"`), false},
		{"list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `"2026-10-22T10:00:00"`), false},
		{"list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `"2026-10-22T10:00:00.1234567890Z"`), false},
		{"list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `1761127200`), false},
		{"list-answer", `{"machines": [{"id": "i-1", "key": "dp_0123456789abcdef", "state": "running"}]}`, false},
		{"list-answer", `{"machines": [{"id": "i-1", "key": "dp_0123456789abcdef", "state": "running", "started": "2026-10-22T10:00:00Z", "size": "s"}]}`, false},
		{"list-answer", `{"machines": [7]}`, false},
		{"list-answer", `{"machines": null}`, false},
		{"list-answer", `{"machines": {}}`, false},
		{"list-answer", `{}`, false},

		{"log-request", `{"version": 1, "id": "i-0123456789abcdef0"}`, true},
		{"log-request", `{"version": 1, "id": "-"}`, false},
		{"log-request", `{"version": 1, "id": ""}`, false},

		{"log-answer", `{"log": ""}`, true},
		{"log-answer", `{"log": "boot\nstarted\n"}`, true},
		{"log-answer", `{"log": "` + strings.Repeat("a", machines.MaxLog) + `"}`, true},
		{"log-answer", `{"log": "` + strings.Repeat("a", machines.MaxLog+1) + `"}`, false},
		{"log-answer", `{"log": "` + strings.Repeat(`\u0000`, machines.MaxLog) + `"}`, true},
		{"log-answer", `{"log": "` + strings.Repeat("é", machines.MaxLog/2) + `"}`, true},
		{"log-answer", `{"log": null}`, false},

		{"delete-request", `{"version": 1, "id": "i-1"}`, true},
		{"delete-request", `{"version": 1, "id": "-"}`, false},
		{"delete-request", `{"version": 0, "id": "i-1"}`, false},

		{"delete-answer", `{"state": "deleting"}`, true},
		{"delete-answer", `{"state": "gone"}`, true},
		{"delete-answer", `{"state": "stopped"}`, false},
		{"delete-answer", `{"state": ""}`, false},

		{"error", `{"error": {"kind": "limit", "message": "Raise the quota."}}`, true},
		{"error", `{"error": {"kind": "retry", "message": "m"}}`, true},
		{"error", `{"error": {"kind": "defect", "message": "m"}}`, true},
		{"error", `{"error": {"kind": "other", "message": "m"}}`, true},
		{"error", `{"error": {"kind": "fatal", "message": "m"}}`, false},
		{"error", `{"error": {"kind": "other", "message": ""}}`, false},
		{"error", `{"error": {"kind": "other", "message": "` + strings.Repeat("m", machines.MaxMessage) + `"}}`, true},
		{"error", `{"error": {"kind": "other", "message": "` + strings.Repeat("m", machines.MaxMessage+1) + `"}}`, false},
		{"error", `{"error": {"kind": "other"}}`, false},
		{"error", `{"error": {"kind": "other", "message": "m", "code": 1}}`, false},
		{"error", `{"error": {"kind": "other", "message": "m"}, "id": "i-1"}`, false},
		{"error", `{"error": "m"}`, false},
	}
	return f
}

// TestSchemasAndDecodersAgree checks that the schemas and the Go decoders accept and
// refuse the same fixtures.
func TestSchemasAndDecodersAgree(t *testing.T) {
	schemas := compile(t)
	for i, f := range fixtures() {
		doc, err := jsonschema.UnmarshalJSON(strings.NewReader(f.json))
		schemaOK := err == nil && schemas[f.schema].Validate(doc) == nil
		goErr := decoders[f.schema]([]byte(f.json))
		if schemaOK != f.ok || (goErr == nil) != f.ok {
			short := f.json
			if len(short) > 120 {
				short = short[:120] + "…"
			}
			t.Errorf("fixture %d, %s, %s: schema accepts %v, decoder error %v; want accepted %v", i, f.schema, short, schemaOK, goErr, f.ok)
		}
	}
}

// TestDecodersRefuseDuplicateNames checks the rules that JSON Schema does not state.
func TestDecodersRefuseDuplicateNames(t *testing.T) {
	for _, b := range []string{
		`{"version": 1, "version": 1}`,
		"{\"version\": 1, \"x\xff\": 1}",
	} {
		if _, err := machines.DecodeCheckRequest([]byte(b)); err == nil {
			t.Errorf("%q: decoded", b)
		}
	}
	if _, err := machines.DecodeErrorAnswer([]byte(`{"error": {"kind": "other", "kind": "retry", "message": "m"}}`)); err == nil {
		t.Error("an error document with kind twice decoded")
	}
	if !jsontext.Value(`{"a": 1}` + "\n").IsValid() {
		t.Error("a document with a trailing newline is not valid")
	}
}

// TestDecoderRefusesLogBytes checks the one bound that the log schema states in
// characters and the decoder in bytes: a log of MaxLog/2+1 characters of 2 bytes each
// passes the schema and fails the decoder.
func TestDecoderRefusesLogBytes(t *testing.T) {
	b := []byte(`{"log": "` + strings.Repeat("é", machines.MaxLog/2+1) + `"}`)
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil || compile(t)["log-answer"].Validate(doc) != nil {
		t.Fatal("the schema refuses a log within its maxLength")
	}
	if _, err := machines.DecodeLogAnswer(b); err == nil || err.Error() != "the member log must be at most 786432 bytes of UTF-8" {
		t.Errorf("the decoder error is %v", err)
	}
}

// TestDecodeErrorsHaveNoValue checks that a decoder's error states the rule and never
// the value that failed it.
func TestDecodeErrorsHaveNoValue(t *testing.T) {
	const marker = "qecr_MARKER7f3a"
	cases := map[string]string{
		"make-request": `{"version": 1, "key": "` + marker + `", "pool": "np_0123456789abcdef", "size": "s", "start_data": "eA=="}`,
		"make-answer":  `{"id": "` + marker + ` "}`,
		"list-answer":  `{"machines": [{"id": "i", "key": "dp_0123456789abcdef", "state": "` + marker + `", "started": "2026-10-22T10:00:00Z"}]}`,
		"error":        `{"error": {"kind": "` + marker + `", "message": "m"}}`,
		"check-answer": `{"` + marker + `": 1}`,
		"log-answer":   `{"log": 1, "` + marker + `": 2}`,
	}
	for name, b := range cases {
		err := decoders[name]([]byte(b))
		if err == nil || strings.Contains(err.Error(), "MARKER") {
			t.Errorf("%s: the error is %v", name, err)
		}
	}
}
