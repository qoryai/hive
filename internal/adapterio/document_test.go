package adapterio_test

import (
	"bytes"
	"encoding/base64"
	"io/fs"
	"path"
	"strings"
	"testing"

	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/integrations/contracts"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// decoders are the decoders by the name of their schema file without .schema.json.
var decoders = map[string]func([]byte) error{
	"check":         func(b []byte) error { _, err := adapterio.DecodeCheck(b); return err },
	"check-answer":  func(b []byte) error { _, err := adapterio.DecodeCheckAnswer(b); return err },
	"make":          func(b []byte) error { _, _, err := adapterio.DecodeMake(b); return err },
	"make-answer":   func(b []byte) error { _, err := adapterio.DecodeMakeAnswer(b); return err },
	"list":          func(b []byte) error { _, err := adapterio.DecodeList(b); return err },
	"list-answer":   func(b []byte) error { _, err := adapterio.DecodeListAnswer(b); return err },
	"log":           func(b []byte) error { _, err := adapterio.DecodeLog(b); return err },
	"log-answer":    func(b []byte) error { _, err := adapterio.DecodeLogAnswer(b); return err },
	"delete":        func(b []byte) error { _, err := adapterio.DecodeDelete(b); return err },
	"delete-answer": func(b []byte) error { _, err := adapterio.DecodeDeleteAnswer(b); return err },
	"error":         func(b []byte) error { _, err := adapterio.DecodeError(b); return err },
}

// schema compiles the schema of machines/v1 for the decoder name.
func schema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	s, err := contracts.CompileMachines(name + ".schema.json")
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return s
}

// fixture is one document, whether its schema accepts it, and whether its decoder
// accepts it. The decoder refuses more than the schema: see the package comment.
type fixture struct {
	schema   string
	json     string
	schemaOK bool
	ok       bool
}

// b64 is standard base64 of n bytes.
func b64(n int) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'x'}, n)) }

func fixtures() []fixture {
	makeReq := func(key, pool, spec, data string) string {
		return `{"version": 1, "key": ` + key + `, "pool": ` + pool + `, "machine_spec": ` + spec + `, "start_data": ` + data + `}`
	}
	// spec is a machine_spec with the vCPUs, memory, disk, architecture and
	// virtualization written as JSON.
	spec := func(vcpus, mib, disk, arch, virt string) string {
		return `{"vcpus": ` + vcpus + `, "memory_mib": ` + mib + `, "disk_gib": ` + disk + `, "arch": ` + arch + `, "virtualization": ` + virt + `}`
	}
	s := spec("2", "4096", "30", `"amd64"`, "false")
	offers := func(n int) string { return `{"offers": [` + strings.TrimSuffix(strings.Repeat(s+",", n), ",") + `]}` }
	good := `"` + b64(10) + `"`
	machine := func(id, key, state, started string) string {
		return `{"machines": [{"id": ` + id + `, "key": ` + key + `, "state": ` + state + `, "started": ` + started + `}]}`
	}
	t0 := `"2026-10-22T10:00:00Z"`
	// both is a fixture that the schema and the decoder accept or refuse alike.
	both := func(schema, json string, ok bool) fixture { return fixture{schema, json, ok, ok} }
	return []fixture{
		both("check", `{"version": 1}`, true),
		both("check", ` {"version": 1.0} `, true),
		both("check", `{"version": 1e0}`, true),
		both("check", `{"version": 2}`, false),
		both("check", `{"version": "1"}`, false),
		both("check", `{"version": null}`, false),
		both("check", `{}`, false),
		both("check", `{"version": 1, "extra": true}`, false),
		both("check", `[]`, false),
		both("check", `null`, false),
		both("check", `"x"`, false),

		both("check-answer", offers(1), true),
		both("check-answer", offers(64), true),
		both("check-answer", offers(65), false),
		both("check-answer", offers(0), false),
		both("check-answer", `{"offers": null}`, false),
		both("check-answer", `{}`, false),
		both("check-answer", `{"ok": true}`, false),
		both("check-answer", `[]`, false),

		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, s, good), true),
		both("make", makeReq(`"dp_vwxyz0123456789a"`, `"np_hjkmnpqrstvwxyz0"`, s, good), true),
		both("make", makeReq(`"dp_0123456789abcde"`, `"np_0123456789abcdef"`, s, good), false),
		both("make", makeReq(`"dp_0123456789abcdef0"`, `"np_0123456789abcdef"`, s, good), false),
		both("make", makeReq(`"dp_0123456789abcdei"`, `"np_0123456789abcdef"`, s, good), false),
		both("make", makeReq(`"dp_0123456789abcdeu"`, `"np_0123456789abcdef"`, s, good), false),
		both("make", makeReq(`"dp_0123456789ABCDEF"`, `"np_0123456789abcdef"`, s, good), false),
		both("make", makeReq(`"np_0123456789abcdef"`, `"np_0123456789abcdef"`, s, good), false),
		both("make", makeReq(`""`, `"np_0123456789abcdef"`, s, good), false),
		both("make", makeReq(`1`, `"np_0123456789abcdef"`, s, good), false),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"dp_0123456789abcdef"`, s, good), false),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdel"`, s, good), false),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdeo"`, s, good), false),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, spec("1", "512", "1", `"arm64"`, "true"), good), true),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, spec("1024", "8388608", "65536", `"amd64"`, "false"), good), true),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, spec("0", "4096", "30", `"amd64"`, "false"), good), false),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, spec("1025", "4096", "30", `"amd64"`, "false"), good), false),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, spec("2", "511", "30", `"amd64"`, "false"), good), false),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, spec("2", "8388609", "30", `"amd64"`, "false"), good), false),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, spec("2", "4096", "0", `"amd64"`, "false"), good), false),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, spec("2", "4096", "65537", `"amd64"`, "false"), good), false),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, spec("2.5", "4096", "30", `"amd64"`, "false"), good), false),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, spec("2", "4096", "30", `"x86_64"`, "false"), good), false),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, spec("2", "4096", "30", `"amd64"`, `"no"`), good), false),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `{"vcpus": 2, "memory_mib": 4096, "disk_gib": 30, "arch": "amd64"}`, good), false),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, strings.Replace(s, "}", `, "gpus": 1}`, 1), good), false),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, `"standard"`, good), false),
		both("make", `{"version": 1, "key": "dp_0123456789abcdef", "pool": "np_0123456789abcdef", "size": "standard", "start_data": "eA=="}`, false),
		{"make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, s, `""`), true, false},
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, s, `"`+b64(1)+`"`), true),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, s, `"`+b64(2)+`"`), true),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, s, `"`+b64(16382)+`"`), true),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, s, `"`+b64(16383)+`"`), true),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, s, `"`+b64(16384)+`"`), true),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, s, `"`+b64(16385)+`"`), false),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, s, `"`+b64(16386)+`"`), false),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, s, `"eA"`), false),
		{"make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, s, `"eB=="`), true, false},
		{"make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, s, `"eHl="`), true, false},
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, s, `"eA=="`), true),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, s, `"eHg\n="`), false),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, s, `"eH-="`), false),
		both("make", makeReq(`"dp_0123456789abcdef"`, `"np_0123456789abcdef"`, s, `"eHh4"`), true),
		both("make", `{"version": 1, "key": "dp_0123456789abcdef", "pool": "np_0123456789abcdef", "machine_spec": `+s+`}`, false),
		both("make", `{"version": 1, "key": "dp_0123456789abcdef", "pool": "np_0123456789abcdef", "start_data": "eA=="}`, false),
		both("make", `{"version": 2, "key": "dp_0123456789abcdef", "pool": "np_0123456789abcdef", "machine_spec": `+s+`, "start_data": "eA=="}`, false),

		both("make-answer", `{"id": "i-0123456789abcdef0"}`, true),
		both("make-answer", `{"id": "-"}`, false),
		both("make-answer", `{"id": "--"}`, true),
		both("make-answer", `{"id": "a b"}`, false),
		both("make-answer", `{"id": ""}`, false),
		both("make-answer", `{"id": "`+strings.Repeat("i", 128)+`"}`, true),
		both("make-answer", `{"id": "`+strings.Repeat("i", 129)+`"}`, false),
		both("make-answer", `{"id": "é"}`, false),
		both("make-answer", `{"id": "i-1", "key": "d"}`, false),
		both("make-answer", `{"id": 7}`, false),
		both("make-answer", `{"id": "i-1", "machine_type": "m7i.large"}`, true),
		both("make-answer", `{"id": "i-1", "machine_type": "`+strings.Repeat("t", 64)+`"}`, true),
		both("make-answer", `{"id": "i-1", "machine_type": "`+strings.Repeat("t", 65)+`"}`, false),
		both("make-answer", `{"id": "i-1", "machine_type": "m7i large"}`, false),
		both("make-answer", `{"id": "i-1", "machine_type": ""}`, false),

		both("list", `{"version": 1, "pool": "np_0123456789abcdef"}`, true),
		both("list", `{"version": 1}`, false),
		both("list", `{"version": 1, "pool": "np_0123456789abcdef", "id": "i"}`, false),

		both("list-answer", `{"machines": []}`, true),
		both("list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"starting"`, t0), true),
		both("list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `"2026-10-22T10:00:00.123456789Z"`), false),
		both("list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `"2026-10-22T10:00:00.5Z"`), false),
		both("list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `"2026-10-22T10:00:00+02:00"`), false),
		both("list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"stopped"`, `"2026-10-22T10:00:00+00:00"`), false),
		both("list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"deleting"`, t0), true),
		both("list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"gone"`, t0), false),
		both("list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"Running"`, t0), false),
		both("list-answer", machine(`"-"`, `"dp_0123456789abcdef"`, `"running"`, t0), false),
		both("list-answer", machine(`"i-1"`, `"d 1"`, `"running"`, t0), false),
		both("list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `"2026-10-22t10:00:00z"`), false),
		both("list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `"2026-10-22 10:00:00Z"`), false),
		both("list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `"2026-02-30T10:00:00Z"`), false),
		both("list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `"2026-13-01T10:00:00Z"`), false),
		both("list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `"2016-12-31T23:59:60Z"`), false),
		both("list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `"2026-10-22T10:00:00"`), false),
		both("list-answer", machine(`"i-1"`, `"dp_0123456789abcdef"`, `"running"`, `1761127200`), false),
		both("list-answer", `{"machines": [{"id": "i-1", "key": "dp_0123456789abcdef", "state": "running"}]}`, false),
		both("list-answer", `{"machines": [{"id": "i-1", "key": "dp_0123456789abcdef", "state": "running", "started": "2026-10-22T10:00:00Z", "size": "s"}]}`, false),
		both("list-answer", `{"machines": [7]}`, false),
		both("list-answer", `{"machines": null}`, false),
		both("list-answer", `{"machines": {}}`, false),
		both("list-answer", `{}`, false),

		both("log", `{"version": 1, "id": "i-0123456789abcdef0"}`, true),
		both("log", `{"version": 1, "id": "-"}`, false),
		both("log", `{"version": 1, "id": ""}`, false),

		both("log-answer", `{"log": ""}`, true),
		both("log-answer", `{"log": "boot\nstarted\n"}`, true),
		both("log-answer", `{"log": "`+strings.Repeat("a", adapterio.MaxLog)+`"}`, true),
		both("log-answer", `{"log": "`+strings.Repeat("a", adapterio.MaxLog+1)+`"}`, false),
		both("log-answer", `{"log": "boot\u0000"}`, false),
		both("log-answer", `{"log": "`+strings.Repeat("é", adapterio.MaxLog/2)+`"}`, true),
		{"log-answer", `{"log": "` + strings.Repeat("é", adapterio.MaxLog/2+1) + `"}`, true, false},
		both("log-answer", `{"log": null}`, false),

		both("delete", `{"version": 1, "id": "i-1"}`, true),
		both("delete", `{"version": 1, "id": "-"}`, false),
		both("delete", `{"version": 0, "id": "i-1"}`, false),

		both("delete-answer", `{"state": "deleting"}`, true),
		both("delete-answer", `{"state": "gone"}`, true),
		both("delete-answer", `{"state": "stopped"}`, false),
		both("delete-answer", `{"state": ""}`, false),

		both("error", `{"error": {"kind": "limit", "message": "Raise the quota."}}`, true),
		both("error", `{"error": {"kind": "retry", "message": "m"}}`, true),
		both("error", `{"error": {"kind": "defect", "message": "m"}}`, true),
		both("error", `{"error": {"kind": "other", "message": "m"}}`, true),
		both("error", `{"error": {"kind": "fatal", "message": "m"}}`, false),
		both("error", `{"error": {"kind": "other", "message": ""}}`, false),
		both("error", `{"error": {"kind": "other", "message": "`+strings.Repeat("é", adapterio.MaxMessage)+`"}}`, true),
		both("error", `{"error": {"kind": "other", "message": "`+strings.Repeat("m", adapterio.MaxMessage+1)+`"}}`, false),
		both("error", `{"error": {"kind": "other", "message": "m\u0000"}}`, false),
		both("error", `{"error": {"kind": "other"}}`, false),
		both("error", `{"error": {"kind": "other", "message": "m", "code": 1}}`, false),
		both("error", `{"error": {"kind": "other", "message": "m"}, "id": "i-1"}`, false),
		both("error", `{"error": "m"}`, false),
	}
}

// TestDecodersAndSchemasAgree checks that each decoder accepts what the schema of
// machines/v1 accepts, except the rules that the decoders add, and refuses what the
// schema refuses.
func TestDecodersAndSchemasAgree(t *testing.T) {
	schemas := map[string]*jsonschema.Schema{}
	for name := range decoders {
		schemas[name] = schema(t, name)
	}
	for i, f := range fixtures() {
		doc, err := jsonschema.UnmarshalJSON(strings.NewReader(f.json))
		schemaOK := err == nil && schemas[f.schema].Validate(doc) == nil
		goErr := decoders[f.schema]([]byte(f.json))
		if schemaOK != f.schemaOK || (goErr == nil) != f.ok {
			short := f.json
			if len(short) > 120 {
				short = short[:120] + "…"
			}
			t.Errorf("fixture %d, %s, %s: schema accepts %v, decoder error %v; want schema %v, decoder %v", i, f.schema, short, schemaOK, goErr, f.schemaOK, f.ok)
		}
	}
}

// TestDecodersReadTheContractFixtures checks each decoder against the fixtures of
// machines/v1: fixtures/<name>/ are accepted, and fixtures/invalid/<name>-*.json are
// refused.
func TestDecodersReadTheContractFixtures(t *testing.T) {
	n := 0
	err := fs.WalkDir(contracts.MachinesFS, "fixtures", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(contracts.MachinesFS, p)
		if err != nil {
			return err
		}
		dir, file := path.Split(p)
		name, valid := path.Base(dir), true
		if name == "invalid" {
			name, valid = "", false
			for d := range decoders {
				if strings.HasPrefix(file, d+"-") && len(d) > len(name) {
					name = d
				}
			}
		}
		decode, ok := decoders[name]
		if !ok {
			t.Errorf("%s: no decoder", p)
			return nil
		}
		n++
		if err := decode(b); (err == nil) != valid {
			t.Errorf("%s: decoder error %v, want accepted %v", p, err, valid)
		}
		return nil
	})
	if err != nil || n < 50 {
		t.Fatalf("read %d fixtures: %v", n, err)
	}
}

// TestBoundsAreTheSchemas checks that MaxMessage and MaxLog are the maxLength of the
// message and of the log in machines/v1.
func TestBoundsAreTheSchemas(t *testing.T) {
	maxLength := func(file string, path ...string) any {
		doc, err := contracts.MachinesDocument(file)
		if err != nil {
			t.Fatal(err)
		}
		v := doc
		for _, p := range path {
			v = v.(map[string]any)[p]
		}
		return v.(map[string]any)["maxLength"]
	}
	if got := maxLength("error.schema.json", "properties", "error", "properties", "message"); got.(interface{ String() string }).String() != "200" || adapterio.MaxMessage != 200 {
		t.Errorf("the message's maxLength is %v, MaxMessage %d", got, adapterio.MaxMessage)
	}
	if got := maxLength("log-answer.schema.json", "properties", "log"); got.(interface{ String() string }).String() != "786432" || adapterio.MaxLog != 786432 {
		t.Errorf("the log's maxLength is %v, MaxLog %d", got, adapterio.MaxLog)
	}
}

// TestDecodersRefuseDuplicateNames checks the rules that JSON Schema does not state.
func TestDecodersRefuseDuplicateNames(t *testing.T) {
	for _, b := range []string{
		`{"version": 1, "version": 1}`,
		"{\"version\": 1, \"x\xff\": 1}",
	} {
		if _, err := adapterio.DecodeCheck([]byte(b)); err == nil {
			t.Errorf("%q: decoded", b)
		}
	}
	if _, err := adapterio.DecodeError([]byte(`{"error": {"kind": "other", "kind": "retry", "message": "m"}}`)); err == nil {
		t.Error("an error document with kind twice decoded")
	}
}

// TestDecoderRefusesLogBytes checks the one bound that the log schema states in
// characters and the decoder in bytes.
func TestDecoderRefusesLogBytes(t *testing.T) {
	b := []byte(`{"log": "` + strings.Repeat("é", adapterio.MaxLog/2+1) + `"}`)
	if _, err := adapterio.DecodeLogAnswer(b); err == nil || err.Error() != "the member log must be at most 786432 bytes of UTF-8" {
		t.Errorf("the decoder error is %v", err)
	}
}

// TestDecodeErrorsHaveNoValue checks that a decoder's error states the rule and never
// the value that failed it.
func TestDecodeErrorsHaveNoValue(t *testing.T) {
	const marker = "qecr_MARKER7f3a"
	cases := map[string]string{
		"make":         `{"version": 1, "key": "` + marker + `", "pool": "np_0123456789abcdef", "machine_spec": {"vcpus": 2, "memory_mib": 4096, "disk_gib": 30, "arch": "amd64", "virtualization": false}, "start_data": "eA=="}`,
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

// TestDecodeErrors checks the text of a decoder's error for each kind of rule.
func TestDecodeErrors(t *testing.T) {
	for _, c := range []struct{ name, json, want string }{
		{"check", `{"version": 2}`, "the member version must be 1"},
		{"check", `{"version": 1, "x": 1}`, "the document has a member that version 1 does not define"},
		{"check", `[]`, "the document must be one JSON object in UTF-8, with no member name twice"},
		{"make", `{"version": 1, "key": "dp_0123456789abcdef", "pool": "np_0123456789abcdef", "machine_spec": {"vcpus": 2, "memory_mib": 4096, "disk_gib": 30, "arch": "amd64", "virtualization": false}}`, "the member start_data is missing"},
		{"make", `{"version": 1, "key": "dp_0123456789abcdef", "pool": "np_0123456789abcdef", "start_data": "eA=="}`, "the member machine_spec is missing"},
		{"make", `{"version": 1, "key": "d", "pool": "np_0123456789abcdef", "machine_spec": {"vcpus": 2, "memory_mib": 4096, "disk_gib": 30, "arch": "amd64", "virtualization": false}, "start_data": "eA=="}`, "the member key must match the pattern ^dp_[0-9a-hjkmnp-tv-z]{16}$"},
		{"make", `{"version": 1, "key": "dp_0123456789abcdef", "pool": "np_0123456789abcdef", "machine_spec": {"vcpus": 2, "memory_mib": 4096, "disk_gib": 30, "arch": "amd64", "virtualization": false}, "start_data": ""}`, "the member start_data must be standard base64 of 1 to 16384 bytes, with padding bits of zero"},
		{"make-answer", `{"id": 7}`, "the member id must be a string"},
		{"list-answer", `{"machines": [{"id": "i", "key": "dp_0123456789abcdef", "state": "gone", "started": "2026-10-22T10:00:00Z"}]}`, "the member machines[0].state must be starting, running, stopped or deleting"},
		{"list-answer", `{"machines": [{"id": "i", "key": "dp_0123456789abcdef", "state": "running"}]}`, "the member machines[0].started is missing"},
		{"list-answer", `{"machines": [{"id": "i", "key": "dp_0123456789abcdef", "state": "running", "started": "2026-10-22T10:00:00Z", "x": 1}]}`, "machines[0]: the document has a member that version 1 does not define"},
		{"error", `{"error": {"kind": "other", "message": ""}}`, "the member error.message must not be empty"},
		{"error", `{"error": {"kind": "fatal", "message": "m"}}`, "the member error.kind must be limit, retry, defect or other"},
	} {
		if err := decoders[c.name]([]byte(c.json)); err == nil || err.Error() != c.want {
			t.Errorf("%s %s: the error is %v, want %q", c.name, c.json, err, c.want)
		}
	}
}

// TestCheckKey checks the shape of a key.
func TestCheckKey(t *testing.T) {
	if err := adapterio.CheckKey("dp_0123456789abcdef"); err != nil {
		t.Error(err)
	}
	for _, key := range []string{"", "dp_0123456789abcdeu", "np_0123456789abcdef", "dp_0123456789abcdef0"} {
		if adapterio.CheckKey(key) == nil {
			t.Errorf("%q passes", key)
		}
	}
}
