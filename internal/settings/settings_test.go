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
		"apiary_url":      "https://apiary.example.com",
		"gateway_address": "10.0.1.5",
		"adapter": map[string]any{
			"command":  "/usr/local/bin/qory-machines-aws-ec2",
			"settings": "/etc/qory-provisioner/aws-ec2.json",
		},
	}
}

// set sets the member name of m to v, where name is a member of the document or
// adapter. and a member of adapter.
func set(m map[string]any, name string, v any) {
	if sub, ok := strings.CutPrefix(name, "adapter."); ok {
		m["adapter"].(map[string]any)[sub] = v
		return
	}
	m[name] = v
}

// parse parses the settings m.
func parse(t *testing.T, m map[string]any) (Settings, error) {
	t.Helper()
	return Parse(mustJSON(t, m))
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseTakesEveryMember(t *testing.T) {
	s, err := parse(t, valid())
	if err != nil {
		t.Fatal(err)
	}
	if s.ApiaryURL != "https://apiary.example.com" || s.GatewayAddress.String() != "10.0.1.5" ||
		s.Adapter.Command != "/usr/local/bin/qory-machines-aws-ec2" || s.Adapter.Settings != "/etc/qory-provisioner/aws-ec2.json" {
		t.Errorf("%+v", s)
	}
}

// TestParseSetsTheDefaults pins no enrolment code file when the member is left out.
func TestParseSetsTheDefaults(t *testing.T) {
	s, err := parse(t, valid())
	if err != nil {
		t.Fatal(err)
	}
	if s.EnrolmentCodeFile != "" {
		t.Errorf("%+v", s)
	}
}

// TestParseTakesTheEnrolmentCodeFile pins enrolment_code_file, an optional absolute
// path, kept as it is written.
func TestParseTakesTheEnrolmentCodeFile(t *testing.T) {
	m := valid()
	m["enrolment_code_file"] = "/run/qory-enrolment/provisioner/code"
	s, err := parse(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if s.EnrolmentCodeFile != "/run/qory-enrolment/provisioner/code" {
		t.Errorf("%+v", s)
	}
	_, err = Parse([]byte(`{"apiary_url": "https://apiary.example.com", "gateway_address": "10.0.1.5", "adapter": {"command": "/a", "settings": "/b"}, "enrolment_code_file": "/c", "enrolment_code_file": "/d"}`))
	if err == nil || err.Error() != `Write each member once in the settings file: "/enrolment_code_file" appears more than once.` {
		t.Errorf("%v", err)
	}
	m = valid()
	m["enrolment_code"] = "/run/qory-enrolment/provisioner/code"
	if _, err := parse(t, m); err == nil || err.Error() != `Remove the unknown members from the settings file: "enrolment_code".` {
		t.Errorf("%v", err)
	}
}

func TestParseRefusesAMissingMember(t *testing.T) {
	m := valid()
	delete(m, "gateway_address")
	delete(m, "apiary_url")
	_, err := parse(t, m)
	if err == nil || err.Error() != "Add the missing members to the settings file: apiary_url, gateway_address." {
		t.Errorf("%v", err)
	}
	_, err = Parse([]byte(`{}`))
	if err == nil || err.Error() != "Add the missing members to the settings file: apiary_url, gateway_address, adapter." {
		t.Errorf("%v", err)
	}
	m = valid()
	m["adapter"] = map[string]any{}
	_, err = parse(t, m)
	if err == nil || err.Error() != "Add the missing members to the settings file: adapter.command, adapter.settings." {
		t.Errorf("%v", err)
	}
	m = valid()
	delete(m["adapter"].(map[string]any), "settings")
	_, err = parse(t, m)
	if err == nil || err.Error() != "Add the missing members to the settings file: adapter.settings." {
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
	m = valid()
	set(m, "adapter.args", "--verbose")
	set(m, "adapter.env", "x")
	_, err = parse(t, m)
	if err == nil || err.Error() != `Remove the unknown members from the settings file: "adapter.args", "adapter.env".` {
		t.Errorf("%v", err)
	}
}

// TestParseRefusesTheRemovedMembers pins that each member the settings no longer have,
// the machine settings that moved to the adapter's settings, the gateway's URL and the
// machine's lifetime, is refused as unknown.
func TestParseRefusesTheRemovedMembers(t *testing.T) {
	for name, v := range map[string]any{
		"run_subnet_id":         "subnet-0123456789abcdef0",
		"run_security_group_id": "sg-0123456789abcdef0",
		"machine_image_id":      "ami-0123456789abcdef0",
		"machine_types":         map[string]any{"small": "t3.medium"},
		"gateway_name":          "gateway.example.com",
		"gateway_port":          8443,
		"gateway_url":           "https://gateway.example.com:8443",
		"max_lifetime_minutes":  60,
	} {
		m := valid()
		m[name] = v
		_, err := parse(t, m)
		if err == nil || err.Error() != "Remove the unknown members from the settings file: "+strconv.Quote(name)+"." {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestParseRefusesMaxMachines pins that max_machines, which the settings no longer
// have, fails as an unknown member. The pool's Machine limit in Qory Apiary is the
// only limit on how many machines exist at once, so a settings file with the member
// must fail rather than let an admin believe that a limit applies.
func TestParseRefusesMaxMachines(t *testing.T) {
	m := valid()
	m["max_machines"] = 4
	_, err := parse(t, m)
	if err == nil || err.Error() != `Remove the unknown members from the settings file: "max_machines".` {
		t.Errorf("%v", err)
	}
}

func TestParseRefusesADuplicateMember(t *testing.T) {
	for doc, want := range map[string]string{
		`{"apiary_url": "https://a.example.com", "apiary_url": "https://b.example.com"}`: `"/apiary_url"`,
		`{"adapter": {"command": "/usr/bin/a", "command": "/usr/bin/b"}}`:                `"/adapter/command"`,
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

// wantPathRule is the rule of adapter.command and adapter.settings after their name.
const wantPathRule = " in the settings file to an absolute path of at most 4096 bytes, with no . or .. element, no double slash, no trailing slash and no NUL byte, such as "

// TestParseRefusesAWrongValue pins, for each member, values of the wrong type and out
// of bounds, and that the error states the rule and never the value.
func TestParseRefusesAWrongValue(t *testing.T) {
	rules := map[string]string{
		"apiary_url":          "Set apiary_url in the settings file to an https origin, or for a test an http origin on localhost, 127.0.0.1 or [::1].",
		"gateway_address":     "Set gateway_address in the settings file to the gateway's IPv4 or IPv6 address on the machines' network, such as 10.0.1.5, with no zone and no port, and not an unspecified, multicast or loopback address.",
		"adapter":             "Set adapter in the settings file to an object with two members, command and settings.",
		"adapter.command":     "Set adapter.command" + wantPathRule + "/usr/local/bin/qory-machines-aws-ec2.",
		"adapter.settings":    "Set adapter.settings" + wantPathRule + "/etc/qory-provisioner/aws-ec2.json.",
		"enrolment_code_file": "Set enrolment_code_file" + wantPathRule + "/run/qory-enrolment/provisioner/code, or leave it out.",
	}
	paths := []string{`null`, `1`, `""`, `true`, `[]`, `{}`, `"qory-machines-aws-ec2"`, `"bin/qory-machines-aws-ec2"`, `"./qory-machines-aws-ec2"`,
		`"/usr//local/bin/x"`, `"/usr/local/bin/"`, `"/usr/./local/bin/x"`, `"/usr/local/../bin/x"`, `"/usr/local/bin/x/.."`, `"/usr/local/bin/."`,
		`"//usr/local/bin/x"`, `"/usr/local/bin/x\u0000"`, `"/` + strings.Repeat("a", 4096) + `"`}
	bad := map[string][]string{
		"apiary_url": {`null`, `1`, `""`, `"apiary.example.com"`, `"http://apiary.example.com"`, `"https://apiary.example.com/v1"`, `"https://apiary.example.com?x=1"`, `"https://user@apiary.example.com"`,
			`"https://apiary.example.com#x"`, `"ftp://apiary.example.com"`, `"https://apiary.example.com:0"`, `"https://apiary.example.com:65536"`, `"https://\u00e1piary.example.com"`, `"https://"`},
		"gateway_address": {`null`, `1`, `""`, `true`, `[]`, `{}`, `"gateway.example.com"`, `"10.0.1"`, `"010.0.1.5"`, `"10.0.1.256"`, `" 10.0.1.5"`, `"10.0.1.5 "`,
			`"10.0.1.5:8443"`, `"[fd00::5]"`, `"[fd00::5]:8443"`, `"10.0.1.5/32"`, `"fd00::5%eth0"`, `"fe80::1%eth0"`,
			`"0.0.0.0"`, `"::"`, `"::ffff:0.0.0.0"`, `"127.0.0.1"`, `"127.1.2.3"`, `"::1"`, `"::ffff:127.0.0.1"`, `"224.0.0.1"`, `"239.255.255.250"`, `"ff02::1"`, `"::ffff:224.0.0.1"`},
		"adapter":             {`null`, `1`, `""`, `"/usr/local/bin/qory-machines-aws-ec2"`, `true`, `[]`, `["/usr/local/bin/qory-machines-aws-ec2", "/etc/qory-provisioner/aws-ec2.json"]`},
		"adapter.command":     paths,
		"adapter.settings":    paths,
		"enrolment_code_file": paths,
	}
	for name, values := range bad {
		for _, v := range values {
			m := valid()
			set(m, name, json.RawMessage(v))
			_, err := parse(t, m)
			if err == nil || err.Error() != rules[name] {
				t.Errorf("%s = %s: %v", name, v, err)
			}
		}
	}
}

// TestParseTakesTheBounds pins the edges of each bounded member and the shapes of the
// addresses, the paths and the origin.
func TestParseTakesTheBounds(t *testing.T) {
	longest := "/" + strings.Repeat("a", 4095)
	for name, values := range map[string][]any{
		"apiary_url":          {"https://apiary.example.com/", "https://APIARY.example.com", "https://apiary.example.com:8443", "http://localhost:4000", "http://127.0.0.1:4000", "http://[::1]:4000", "https://10.0.0.1"},
		"gateway_address":     {"10.0.1.5", "10.255.255.255", "192.168.0.1", "169.254.0.1", "fd00::5", "2001:db8::1", "fe80::1", "::ffff:10.0.1.5"},
		"adapter.command":     {"/a", longest, "/opt/qory machines/aws-ec2", "/opt/a..b/.c", "/usr/local/bin/qory-machines-aws-ec2"},
		"adapter.settings":    {"/a", longest, "/etc/qory-provisioner/aws-ec2.json"},
		"enrolment_code_file": {"/a", longest, "/run/qory-enrolment/provisioner/code"},
	} {
		for _, v := range values {
			m := valid()
			set(m, name, v)
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
	m = valid()
	m["gateway_address"] = "FD00:0:0::5"
	if s, err := parse(t, m); err != nil || s.GatewayAddress.String() != "fd00::5" {
		t.Errorf("%v %v", s.GatewayAddress, err)
	}
	m = valid()
	set(m, "adapter.command", longest)
	if s, err := parse(t, m); err != nil || s.Adapter.Command != longest {
		t.Errorf("%d %v", len(s.Adapter.Command), err)
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
		code    = "Remove the enrolment code (qecn_, qecp_, qecg_ or qeci_) from the settings file: pass a node pool's code in QORY_ENROLMENT_CODE, or in the file at enrolment_code_file."
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
			`{"gateway_address": "` + tc.value + `"}`,
			`{"adapter": {"command": "/` + tc.value + `"}}`,
			`{"adapter": {"settings": "` + tc.value + `"}}`,
			`{"enrolment_code_file": "/run/` + tc.value + `"}`,
			`{"` + tc.value + `": 1}`,
			`{"adapter": {"` + tc.value + `": 1}}`,
			`{"apiary_url": "https://apiary.example.com", "apiary_url": "` + tc.value + `"}`,
			`{"apiary_url": "` + tc.value + `", "apiary_url": "https://apiary.example.com"}`,
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
	for _, name := range []string{"apiary_url", "gateway_address", "adapter", "adapter.command", "adapter.settings", "enrolment_code_file"} {
		m := valid()
		set(m, name, marker)
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
	if err != nil || s.GatewayAddress.String() != "10.0.1.5" {
		t.Errorf("%+v %v", s, err)
	}
	if _, err := Read(filepath.Join(t.TempDir(), "none.json")); err == nil || !strings.HasPrefix(err.Error(), "Create the settings file: open ") {
		t.Errorf("%v", err)
	}
	// Root ignores file modes, so a file of mode 0000 is readable by root.
	if os.Geteuid() != 0 {
		unreadable := filepath.Join(t.TempDir(), "unreadable.json")
		if err := os.WriteFile(unreadable, b, 0o000); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(unreadable); err == nil || !strings.HasPrefix(err.Error(), "Make the settings file readable by uid "+strconv.Itoa(os.Geteuid())+": open ") {
			t.Errorf("%v", err)
		}
	}
	// A folder opens, and reading it fails, on Linux and on macOS.
	folder := t.TempDir()
	if _, err := Read(folder); err == nil || !strings.HasPrefix(err.Error(), "Fix the settings file, then start the provisioner again: reading it failed: read "+folder+": ") {
		t.Errorf("%v", err)
	}
	large := filepath.Join(t.TempDir(), "large.json")
	if err := os.WriteFile(large, []byte(`{"gateway_address": "`+strings.Repeat("a", maxFile)+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(large); err == nil || err.Error() != "Shorten the settings file "+large+" to at most 64 KiB." {
		t.Errorf("%v", err)
	}
}
