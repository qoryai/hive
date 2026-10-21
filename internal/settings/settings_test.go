package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// valid is a whole settings document the provisioner takes, as a map a test changes.
func valid() map[string]any {
	return map[string]any{
		"apiary_url":            "https://apiary.example.com",
		"run_subnet_id":         "subnet-0123456789abcdef0",
		"run_security_group_id": "sg-0123456789abcdef0",
		"machine_image_id":      "ami-0123456789abcdef0",
		"machine_types":         map[string]any{"small": "t3.medium", "large-2": "m7i.2xlarge"},
		"gateway_name":          "gateway.acme.example.com",
		"gateway_port":          8443,
		"max_machines":          4,
		"max_lifetime_minutes":  90,
	}
}

// parse parses the settings m.
func parse(t *testing.T, m map[string]any) (Settings, error) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return Parse(b)
}

func TestParseTakesEveryMember(t *testing.T) {
	s, err := parse(t, valid())
	if err != nil {
		t.Fatal(err)
	}
	if s.ApiaryURL != "https://apiary.example.com" || s.RunSubnetID != "subnet-0123456789abcdef0" || s.RunSecurityGroupID != "sg-0123456789abcdef0" ||
		s.MachineImageID != "ami-0123456789abcdef0" || len(s.MachineTypes) != 2 || s.MachineTypes["small"] != "t3.medium" || s.MachineTypes["large-2"] != "m7i.2xlarge" ||
		s.GatewayName != "gateway.acme.example.com" || s.GatewayPort != 8443 || s.MaxMachines != 4 || s.MaxLifetimeMinutes != 90 {
		t.Errorf("%+v", s)
	}
}

// TestParseSetsTheDefaults pins max_machines 1 and max_lifetime_minutes 60 when the
// members are left out.
func TestParseSetsTheDefaults(t *testing.T) {
	m := valid()
	delete(m, "max_machines")
	delete(m, "max_lifetime_minutes")
	s, err := parse(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if s.MaxMachines != 1 || s.MaxLifetimeMinutes != 60 {
		t.Errorf("%+v", s)
	}
}

func TestParseRefusesAMissingMember(t *testing.T) {
	m := valid()
	delete(m, "gateway_port")
	delete(m, "apiary_url")
	_, err := parse(t, m)
	if err == nil || err.Error() != "Add the missing members to the settings file: apiary_url, gateway_port." {
		t.Errorf("%v", err)
	}
}

func TestParseRefusesAnUnknownMember(t *testing.T) {
	m := valid()
	m["region"] = "x"
	m["access_key_id"] = "ak_0123456789abcdef"
	_, err := parse(t, m)
	if err == nil || err.Error() != `Remove the unknown members from the settings file: "access_key_id", "region".` {
		t.Errorf("%v", err)
	}
}

func TestParseRefusesADuplicateMember(t *testing.T) {
	for doc, want := range map[string]string{
		`{"gateway_port": 1, "gateway_port": 2}`:                        `"/gateway_port"`,
		`{"machine_types": {"small": "t3.small", "small": "t3.large"}}`: `"/machine_types/small"`,
	} {
		_, err := Parse([]byte(doc))
		if err == nil || err.Error() != "Write each member once in the settings file: "+want+" appears more than once." {
			t.Errorf("%s: %v", doc, err)
		}
	}
}

func TestParseRefusesWhatIsNotOneObject(t *testing.T) {
	for doc, want := range map[string]string{
		``:                  "Write the settings file as one JSON object: the file is empty.",
		"  \n":              "Write the settings file as one JSON object: the file is empty.",
		`null`:              "Write the settings file as one JSON object.",
		`[1]`:               "Write the settings file as one JSON object.",
		`"x"`:               "Write the settings file as one JSON object.",
		`{"a": 1,}`:         "Fix the JSON in the settings file at byte offset 7.",
		`{"a": 1} {}`:       "Fix the JSON in the settings file at byte offset 9.",
		"{\"a\": \"\xff\"}": "Fix the JSON in the settings file at byte offset 7.",
	} {
		_, err := Parse([]byte(doc))
		if err == nil || err.Error() != want {
			t.Errorf("%q: %v, want %q", doc, err, want)
		}
	}
}

// TestParseRefusesAWrongValue pins, for each member, values of the wrong type and out
// of bounds, and that the error states the rule and never the value.
func TestParseRefusesAWrongValue(t *testing.T) {
	rules := map[string]string{
		"apiary_url":            "Set apiary_url in the settings file to an https origin, or for a test an http origin on localhost, 127.0.0.1 or [::1].",
		"run_subnet_id":         "Set run_subnet_id in the settings file to subnet- and 8 or 17 lower-case hexadecimal digits.",
		"run_security_group_id": "Set run_security_group_id in the settings file to sg- and 8 or 17 lower-case hexadecimal digits.",
		"machine_image_id":      "Set machine_image_id in the settings file to ami- and 8 or 17 lower-case hexadecimal digits.",
		"machine_types":         "Set machine_types in the settings file to an object of 1 to 20 members, each a name of 1 to 40 lower-case letters, digits and dashes that starts with a letter, set to an EC2 instance type such as t3.medium.",
		"gateway_name":          "Set gateway_name in the settings file to a DNS name in lower case, such as gateway.example.com.",
		"gateway_port":          "Set gateway_port in the settings file to a whole number from 1 to 65535.",
		"max_machines":          "Set max_machines in the settings file to a whole number from 1 to 100, or leave it out for 1.",
		"max_lifetime_minutes":  "Set max_lifetime_minutes in the settings file to a whole number from 5 to 720, or leave it out for 60.",
	}
	ids := []string{`null`, `1`, `""`, `true`, `[]`, `{}`, `"subnet-0123456"`, `"subnet-0123456789"`, `"subnet-0123456789abcdef"`, `"subnet-0123456789ABCDEF0"`, `" subnet-01234567"`, `"sg-01234567x"`, `"ami-0123456789abcdef01"`}
	bad := map[string][]string{
		"apiary_url": {`null`, `1`, `""`, `"apiary.example.com"`, `"http://apiary.example.com"`, `"https://apiary.example.com/v1"`, `"https://apiary.example.com?x=1"`, `"https://user@apiary.example.com"`,
			`"https://apiary.example.com#x"`, `"ftp://apiary.example.com"`, `"https://apiary.example.com:0"`, `"https://apiary.example.com:65536"`, `"https://\u00e1piary.example.com"`, `"https://"`},
		"run_subnet_id":         append(ids, `"sg-01234567"`, `"subnet-"`),
		"run_security_group_id": append(ids, `"subnet-01234567"`, `"sg-"`),
		"machine_image_id":      append(ids, `"sg-01234567"`, `"ami-"`),
		"machine_types": {`null`, `1`, `"t3.medium"`, `["t3.medium"]`, `{}`, `{"Small": "t3.medium"}`, `{"1small": "t3.medium"}`,
			`{"small_1": "t3.medium"}`, `{"` + strings.Repeat("a", 41) + `": "t3.medium"}`, `{"": "t3.medium"}`,
			`{"small": "t3"}`, `{"small": "T3.medium"}`, `{"small": "t3.medium.x"}`, `{"small": "3t.medium"}`,
			`{"small": 1}`, `{"small": null}`, `{"small": ""}`, `{"small": "t3.Medium"}`, twentyOne(t)},
		"gateway_name": {`null`, `1`, `""`, `"Gateway.example.com"`, `"gateway..example.com"`, `".gateway.example.com"`, `"gateway.example.com."`, `"-gateway.example.com"`, `"gateway-.example.com"`,
			`"gate_way.example.com"`, `"10.0.0.1"`, `"gateway.123"`, `"` + strings.Repeat("a", 64) + `.example.com"`, `"` + strings.Repeat(strings.Repeat("a", 63)+".", 4) + `com"`, `"gateway example.com"`},
		"gateway_port":         {`null`, `"8443"`, `0`, `65536`, `-1`, `1.5`, `8443.0`, `1e3`, `true`, `08443`},
		"max_machines":         {`null`, `"1"`, `0`, `101`, `-1`, `1.5`, `2.0`, `true`},
		"max_lifetime_minutes": {`null`, `"60"`, `0`, `4`, `721`, `1.5`, `60.0`, `false`},
	}
	for name, values := range bad {
		for _, v := range values {
			m := valid()
			m[name] = json.RawMessage(v)
			b, err := json.Marshal(m)
			if err != nil {
				// 08443 is no JSON, which json.Marshal refuses: the document is written
				// by hand.
				b = []byte(`{"` + name + `": ` + v + `}`)
			}
			_, err = Parse(b)
			if name == "gateway_port" && v == `08443` {
				if err == nil || !strings.HasPrefix(err.Error(), "Fix the JSON in the settings file at byte offset ") {
					t.Errorf("%s = %s: %v", name, v, err)
				}
				continue
			}
			if err == nil || err.Error() != rules[name] {
				t.Errorf("%s = %s: %v", name, v, err)
			}
		}
	}
}

// twentyOne is machine_types with 21 members, one more than the most, as JSON.
func twentyOne(t *testing.T) string {
	m := map[string]any{}
	for i := range 21 {
		m["type-"+string(rune('a'+i))] = "t3.medium"
	}
	return string(mustJSON(t, m))
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestParseTakesTheBounds pins the edges of each bounded member and the shapes of the
// ids, the names and the origin.
func TestParseTakesTheBounds(t *testing.T) {
	m20 := map[string]any{}
	for i := range 20 {
		m20["t"+string(rune('a'+i))] = "t3.medium"
	}
	for name, values := range map[string][]any{
		"apiary_url":            {"https://apiary.example.com/", "https://APIARY.example.com", "https://apiary.example.com:8443", "http://localhost:4000", "http://127.0.0.1:4000", "http://[::1]:4000", "https://10.0.0.1"},
		"run_subnet_id":         {"subnet-01234567", "subnet-0123456789abcdef0"},
		"run_security_group_id": {"sg-01234567", "sg-0123456789abcdef0"},
		"machine_image_id":      {"ami-01234567", "ami-0123456789abcdef0"},
		"machine_types":         {map[string]any{"a": "t3.medium"}, m20, map[string]any{strings.Repeat("a", 40): "u-24tb1.112xlarge"}},
		"gateway_name":          {"gateway", "a.b", strings.Repeat("a", 63) + ".example.com", "gateway-1.example.com", "1gateway.example.com"},
		"gateway_port":          {1, 65535},
		"max_machines":          {1, 100},
		"max_lifetime_minutes":  {5, 720},
	} {
		for _, v := range values {
			m := valid()
			m[name] = v
			if _, err := parse(t, m); err != nil {
				t.Errorf("%s = %s: %v", name, mustJSON(t, v), err)
			}
		}
	}
	m := valid()
	m["apiary_url"] = "https://APIARY.Example.com:8443/"
	if s, err := parse(t, m); err != nil || s.ApiaryURL != "https://apiary.example.com:8443" {
		t.Errorf("%q %v", s.ApiaryURL, err)
	}
}

// TestParseRefusesASecretOrACode pins that settings that contain what looks like an
// access key secret, a run key or an enrolment code, in a value or a member's name, in
// any case and behind a JSON escape, are refused before anything else, with an error
// that quotes nothing of the file.
func TestParseRefusesASecretOrACode(t *testing.T) {
	const (
		secret  = "Remove the access key secret (qak_) from the settings file: the provisioner stores its access key in the state folder."
		runKey  = "Remove the run key (qrk_) from the settings file: a run key belongs in no file."
		runCode = "Remove the run code (qecr_) from the settings file: a run code belongs in no file."
		code    = "Remove the enrolment code (qecn_, qecp_, qecg_ or qeci_) from the settings file: pass a node pool's code in QORY_ENROLMENT_CODE."
		marker  = "MARKER0123456789ABCDEFGHJK"
		escaped = `q`
	)
	for _, tc := range []struct{ value, want string }{
		{"qak_" + marker, secret},
		{"QAK_" + marker, secret},
		{escaped + "ak_" + marker, secret},
		{"qrk_" + marker, runKey},
		{"qrk_" + marker + " qak_", secret},
		{"qecr_" + marker + ".0123456789abcdefghijkl", runCode},
		{"QECR_" + marker, runCode},
		{escaped + "ecr_" + marker, runCode},
		{"qecr_" + marker + " qrk_", runKey},
		{"qecp_" + marker + " qecr_", runCode},
		{"qecp_" + marker, code},
		{"QECN_" + marker, code},
		{"qecg_" + marker, code},
		{escaped + "eci_" + marker, code},
	} {
		for _, doc := range []string{
			`{"gateway_name": "` + tc.value + `"}`,
			`{"machine_types": {"small": "` + tc.value + `"}}`,
			`{"` + tc.value + `": 1}`,
			`{"gateway_port": 1, "gateway_port": "` + tc.value + `"}`,
			`{"gateway_port": "` + tc.value + `", "gateway_port": 1}`,
			`{"` + tc.value + `": 1, "` + tc.value + `": 2}`,
			`[` + `"` + tc.value + `"`,
		} {
			_, err := Parse([]byte(doc))
			if err == nil || err.Error() != tc.want {
				t.Errorf("%s: %v, want %q", doc, err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), marker) {
				t.Errorf("%s: the error quotes the value: %v", doc, err)
			}
		}
	}
}

// TestNoErrorQuotesAValue pins that an error about a member's value never contains the
// value, whatever the member.
func TestNoErrorQuotesAValue(t *testing.T) {
	const marker = "ZZ marker value"
	for name := range valid() {
		m := valid()
		m[name] = marker
		if name == "machine_types" {
			m[name] = map[string]any{"small": marker}
		}
		_, err := parse(t, m)
		if err == nil || strings.Contains(err.Error(), marker) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestReadReadsTheFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "provisioner.json")
	b, _ := json.Marshal(valid())
	if err := os.WriteFile(file, b, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Read(file)
	if err != nil || s.GatewayPort != 8443 {
		t.Errorf("%+v %v", s, err)
	}
	if _, err := Read(filepath.Join(t.TempDir(), "none.json")); err == nil || !strings.HasPrefix(err.Error(), "Create the settings file: open ") {
		t.Errorf("%v", err)
	}
	unreadable := filepath.Join(t.TempDir(), "unreadable.json")
	if err := os.WriteFile(unreadable, b, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(unreadable); err == nil || !strings.HasPrefix(err.Error(), "Make the settings file readable by uid "+strconv.Itoa(os.Geteuid())+": open ") {
		t.Errorf("%v", err)
	}
	// A folder opens, and reading it fails, on Linux and on macOS.
	folder := t.TempDir()
	if _, err := Read(folder); err == nil || !strings.HasPrefix(err.Error(), "Fix the settings file, then start the provisioner again: reading it failed: read "+folder+": ") {
		t.Errorf("%v", err)
	}
	large := filepath.Join(t.TempDir(), "large.json")
	if err := os.WriteFile(large, []byte(`{"gateway_name": "`+strings.Repeat("a", maxFile)+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(large); err == nil || err.Error() != "Shorten the settings file "+large+" to at most 64 KiB." {
		t.Errorf("%v", err)
	}
}
