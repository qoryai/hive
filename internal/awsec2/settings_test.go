package awsec2

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/integrations/contracts"
)

// validSettings is a settings document with neutral example values.
const validSettings = `{
  "subnet_id": "subnet-0123456789abcdef0",
  "security_group_id": "sg-0123456789abcdef0",
  "image_id": "ami-0123456789abcdef0"
}`

// writeSettings writes doc to a settings file in a temporary folder and returns its
// path.
func writeSettings(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aws-ec2.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestParseSettings pins the settings that ReadSettings returns for a valid document,
// and the ids of 8 hexadecimal digits.
func TestParseSettings(t *testing.T) {
	s, err := ReadSettings(writeSettings(t, validSettings))
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{
		SubnetID:        "subnet-0123456789abcdef0",
		SecurityGroupID: "sg-0123456789abcdef0",
		ImageID:         "ami-0123456789abcdef0",
	}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("settings %+v, want %+v", s, want)
	}

	doc := `{"subnet_id": "subnet-01234567", "security_group_id": "sg-01234567", "image_id": "ami-01234567"}`
	s, err = ParseSettings([]byte(doc))
	if err != nil || s.SubnetID != "subnet-01234567" || s.SecurityGroupID != "sg-01234567" || s.ImageID != "ami-01234567" {
		t.Errorf("settings %+v, error %v", s, err)
	}
}

// withTags is validSettings with the member tags set to the JSON value tags.
func withTags(tags string) string {
	return strings.TrimSuffix(validSettings, "\n}") + ",\n  \"tags\": " + tags + "\n}"
}

// TestParseSettingsTags pins the member tags: absent and an empty object give no
// tags, and the bounds that pass are 20 entries, a key of 128 characters, a value of
// 256 characters, an empty value, letters and digits of any script, spaces, and
// _ . : / = + - @.
func TestParseSettingsTags(t *testing.T) {
	for _, doc := range []string{validSettings, withTags(`{}`)} {
		s, err := ParseSettings([]byte(doc))
		if err != nil || s.Tags != nil {
			t.Errorf("%s: tags %#v, error %v, want none", doc, s.Tags, err)
		}
	}

	s, err := ParseSettings([]byte(withTags(`{"qory:stack": "example-stack"}`)))
	if want := map[string]string{"qory:stack": "example-stack"}; err != nil || !reflect.DeepEqual(s.Tags, want) {
		t.Errorf("tags %#v, error %v, want %v", s.Tags, err, want)
	}

	var entries []string
	want := map[string]string{}
	for i := range 17 {
		entries = append(entries, fmt.Sprintf(`"tag-%d": "value-%d"`, i, i))
		want[fmt.Sprintf("tag-%d", i)] = fmt.Sprintf("value-%d", i)
	}
	long := strings.Repeat("ü", maxTagKey)
	entries = append(entries, `"`+long+`": "`+strings.Repeat("9", maxTagValue)+`"`, `"empty": ""`, `"Team Name_1.a:b/c=d+e-f@g": "Ünïcode 名前 ٣ x_.:/=+-@"`)
	want[long] = strings.Repeat("9", maxTagValue)
	want["empty"] = ""
	want["Team Name_1.a:b/c=d+e-f@g"] = "Ünïcode 名前 ٣ x_.:/=+-@"
	s, err = ParseSettings([]byte(withTags(`{` + strings.Join(entries, ", ") + `}`)))
	if err != nil || !reflect.DeepEqual(s.Tags, want) {
		t.Errorf("20 tags at their bounds: tags %#v, error %v", s.Tags, err)
	}

	s, err = ParseSettings([]byte(withTags(`{"qory:stacks": "x", "awsx": "y", "aw:s": "z"}`)))
	if err != nil || len(s.Tags) != 3 {
		t.Errorf("keys close to the refused ones: tags %#v, error %v", s.Tags, err)
	}
}

// TestParseSettingsRefusesRemoved pins that max_root_gib and instance_types, which
// the settings no longer have, fail with the unknown-member error. The disk of each
// machine comes only from the run's machine spec, and the instance type from the
// types that AWS offers in the subnet's zone.
func TestParseSettingsRefusesRemoved(t *testing.T) {
	for _, member := range []string{`"max_root_gib": 500`, `"instance_types": ["t3.medium", "m7i.large"]`} {
		doc := strings.TrimSuffix(validSettings, "\n}") + ",\n  " + member + "\n}"
		_, err := ParseSettings([]byte(doc))
		if err == nil || err.Error() != "Remove every member other than subnet_id, security_group_id, image_id and tags from aws-ec2.json." || kindOf(err) != contracts.FailureDefect {
			t.Errorf("%s: error %v", member, err)
		}
	}
}

// TestParseSettingsErrors pins each error, word for word, and its kind defect. No
// error quotes a value or a member name of the document: each document contains the
// marker in a place that the error would quote.
func TestParseSettingsErrors(t *testing.T) {
	const marker = "qecr_marker7f3a"
	const file = "aws-ec2.json"
	member := func(name, value string) string {
		doc := map[string]string{
			"subnet_id":         `"subnet-0123456789abcdef0"`,
			"security_group_id": `"sg-0123456789abcdef0"`,
			"image_id":          `"ami-0123456789abcdef0"`,
		}
		doc[name] = value
		return fmt.Sprintf(`{"subnet_id": %s, "security_group_id": %s, "image_id": %s}`, doc["subnet_id"], doc["security_group_id"], doc["image_id"])
	}
	subnetRule := "Set subnet_id in " + file + " to subnet- and 8 or 17 lower-case hexadecimal digits."
	sgRule := "Set security_group_id in " + file + " to sg- and 8 or 17 lower-case hexadecimal digits."
	imageRule := "Set image_id in " + file + " to ami- and 8 or 17 lower-case hexadecimal digits."
	tagsRule := "Set tags in " + file + " to an object of at most 20 members, each a tag key of 1 to 128 characters set to a string of 0 to 256 characters."
	tagText := "Write the keys and values of tags in " + file + " with only letters, digits, spaces and _ . : / = + - @."
	tagAWS := "Remove every key that starts with aws: from tags in " + file + ": AWS reserves that prefix."
	tagAdapter := "Remove qory:managed, qory:pool and qory:key, in any case, from tags in " + file + ": the adapter sets these tags on every machine."
	var manyTags []string
	for i := range 21 {
		manyTags = append(manyTags, fmt.Sprintf(`"tag-%d": "x"`, i))
	}
	cases := []struct {
		doc  string
		want string
	}{
		{"", "Write " + file + " as one JSON object: the file is empty."},
		{" \n\t", "Write " + file + " as one JSON object: the file is empty."},
		{`["` + marker + `"]`, "Write " + file + " as one JSON object."},
		{`"` + marker + `"`, "Write " + file + " as one JSON object."},
		{`null`, "Write " + file + " as one JSON object."},
		{`{"subnet_id": "` + marker + `",}`, "Fix the JSON in " + file + " at byte offset 31."},
		{"{\"subnet_id\": \"" + marker + "\xff\"}", "Fix the JSON in " + file + " at byte offset 30."},
		{`{"subnet_id": "subnet-01234567", "subnet_id": "` + marker + `"}`, "Write each member once in " + file + "."},
		{`{"` + marker + `": 1}`, "Remove every member other than subnet_id, security_group_id, image_id and tags from " + file + "."},
		{`{"run_subnet_id": "subnet-01234567"}`, "Remove every member other than subnet_id, security_group_id, image_id and tags from " + file + "."},
		{`{"sizes": {"standard": "t3.medium"}}`, "Remove every member other than subnet_id, security_group_id, image_id and tags from " + file + "."},
		{`{"tags": {}}`, "Add the missing members to " + file + ": subnet_id, security_group_id, image_id."},
		{`{}`, "Add the missing members to " + file + ": subnet_id, security_group_id, image_id."},
		{`{"subnet_id": "subnet-01234567"}`, "Add the missing members to " + file + ": security_group_id, image_id."},
		{`{"subnet_id": "subnet-01234567", "instance_types": []}`, "Remove every member other than subnet_id, security_group_id, image_id and tags from " + file + "."},
		{member("subnet_id", `"`+marker+`"`), subnetRule},
		{member("subnet_id", `"subnet-0123456"`), subnetRule},
		{member("subnet_id", `"subnet-0123456789ABCDEF0"`), subnetRule},
		{member("subnet_id", `7`), subnetRule},
		{member("subnet_id", `null`), subnetRule},
		{member("security_group_id", `"subnet-0123456789abcdef0"`), sgRule},
		{member("image_id", `"ami-`+marker+`"`), imageRule},
		{member("image_id", `["ami-0123456789abcdef0"]`), imageRule},
		{withTags(`["` + marker + `"]`), tagsRule},
		{withTags(`"` + marker + `"`), tagsRule},
		{withTags(`null`), tagsRule},
		{withTags(`{` + strings.Join(manyTags, ", ") + `}`), tagsRule},
		{withTags(`{"": "` + marker + `"}`), tagsRule},
		{withTags(`{"` + strings.Repeat("k", 129) + `": "` + marker + `"}`), tagsRule},
		{withTags(`{"` + marker + `": "` + strings.Repeat("v", 257) + `"}`), tagsRule},
		{withTags(`{"` + marker + `": 7}`), tagsRule},
		{withTags(`{"` + marker + `": null}`), tagsRule},
		{withTags(`{"` + marker + `": ["x"]}`), tagsRule},
		{withTags(`{"` + marker + `": "a", "` + marker + `": "b"}`), "Write each member once in " + file + "."},
		{withTags(`{"` + marker + `!": "x"}`), tagText},
		{withTags(`{"` + marker + `": "x;"}`), tagText},
		{withTags(`{"` + marker + `": "line\nbreak"}`), tagText},
		{withTags(`{"` + marker + `": "x*"}`), tagText},
		{withTags(`{"aws:` + marker + `": "x"}`), tagAWS},
		{withTags(`{"AWS:` + marker + `": "x"}`), tagAWS},
		{withTags(`{"aWs:": "` + marker + `"}`), tagAWS},
		{withTags(`{"qory:managed": "` + marker + `"}`), tagAdapter},
		{withTags(`{"QORY:POOL": "` + marker + `"}`), tagAdapter},
		{withTags(`{"qory:Key": "` + marker + `"}`), tagAdapter},
		// Of several faults, the error is the one of the first check, on every read.
		{withTags(`{"aws:a": "x", "qory:key": "y", "b": "z!"}`), tagText},
		{withTags(`{"qory:key": "y", "aws:a": "x"}`), tagAWS},
	}
	for _, tc := range cases {
		_, err := ParseSettings([]byte(tc.doc))
		var e *adapterio.Failure
		if !errors.As(err, &e) || e.Kind != contracts.FailureDefect {
			t.Errorf("%q: error %#v, want a *adapterio.Failure of kind defect", tc.doc, err)
			continue
		}
		if err.Error() != tc.want {
			t.Errorf("%q: error\n%s\nwant\n%s", tc.doc, err, tc.want)
		}
		if strings.Contains(err.Error(), marker) {
			t.Errorf("%q: the error quotes the marker", tc.doc)
		}
	}
}

// TestReadSettingsFile pins the errors of a missing file, a file larger than 64 KiB,
// an unreadable file and a folder, and that a file of exactly 64 KiB is read.
func TestReadSettingsFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "aws-ec2.json")
	_, err := ReadSettings(missing)
	want := "Create aws-ec2.json at the path in the provisioner's adapter.settings: opening it failed: no such file or directory: " + missing
	if err == nil || err.Error() != want || kindOf(err) != contracts.FailureDefect {
		t.Errorf("missing file: error %v, want %q of kind defect", err, want)
	}

	big := validSettings + strings.Repeat(" ", 64<<10-len(validSettings))
	if _, err := ReadSettings(writeSettings(t, big)); err != nil {
		t.Errorf("64 KiB: %v", err)
	}
	_, err = ReadSettings(writeSettings(t, big+" "))
	const wantBig = "Shorten aws-ec2.json to at most 64 KiB."
	if err == nil || err.Error() != wantBig || kindOf(err) != contracts.FailureDefect {
		t.Errorf("64 KiB and 1 byte: error %v, want %q of kind defect", err, wantBig)
	}

	if os.Geteuid() != 0 {
		locked := writeSettings(t, validSettings)
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		_, err = ReadSettings(locked)
		wantLocked := fmt.Sprintf("Make aws-ec2.json readable by uid %d: opening it failed: permission denied: %s", os.Geteuid(), locked)
		if err == nil || err.Error() != wantLocked || kindOf(err) != contracts.FailureDefect {
			t.Errorf("unreadable file: error %v, want %q of kind defect", err, wantLocked)
		}
	}

	dir := t.TempDir()
	_, err = ReadSettings(dir)
	const prefix = "Fix aws-ec2.json: reading it failed: "
	if err == nil || !strings.HasPrefix(err.Error(), prefix) || kindOf(err) != contracts.FailureDefect {
		t.Errorf("a folder: error %v, want the prefix %q of kind defect", err, prefix)
	}
}

// kindOf returns the kind of a *adapterio.Failure in err's chain, or "".
func kindOf(err error) string {
	var e *adapterio.Failure
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}
