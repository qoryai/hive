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
  "image_id": "ami-0123456789abcdef0",
  "sizes": {"standard": "t3.medium", "large": "m7g.large"}
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

// TestParseSettings pins the settings that a valid document gives, and the bounds of
// sizes that pass: 20 entries and a name of 40 characters.
func TestParseSettings(t *testing.T) {
	s, err := ReadSettings(writeSettings(t, validSettings))
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{
		SubnetID:        "subnet-0123456789abcdef0",
		SecurityGroupID: "sg-0123456789abcdef0",
		ImageID:         "ami-0123456789abcdef0",
		Sizes:           map[string]string{"standard": "t3.medium", "large": "m7g.large"},
	}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("settings %+v, want %+v", s, want)
	}

	var entries []string
	for i := range 19 {
		entries = append(entries, fmt.Sprintf(`"size-%d": "t3.medium"`, i))
	}
	entries = append(entries, `"`+"a"+strings.Repeat("b-9", 13)+`": "c7i.metal-48xl"`)
	doc := `{"subnet_id": "subnet-01234567", "security_group_id": "sg-01234567", "image_id": "ami-01234567", "sizes": {` + strings.Join(entries, ", ") + `}}`
	s, err = ParseSettings([]byte(doc))
	if err != nil {
		t.Fatalf("20 sizes and a name of 40 characters: %v", err)
	}
	if len(s.Sizes) != 20 || s.SubnetID != "subnet-01234567" {
		t.Errorf("settings %+v", s)
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
			"sizes":             `{"standard": "t3.medium"}`,
		}
		doc[name] = value
		return fmt.Sprintf(`{"subnet_id": %s, "security_group_id": %s, "image_id": %s, "sizes": %s}`, doc["subnet_id"], doc["security_group_id"], doc["image_id"], doc["sizes"])
	}
	var many []string
	for i := range 21 {
		many = append(many, fmt.Sprintf(`"size-%d": "t3.medium"`, i))
	}
	subnetRule := "Set subnet_id in " + file + " to subnet- and 8 or 17 lower-case hexadecimal digits."
	sgRule := "Set security_group_id in " + file + " to sg- and 8 or 17 lower-case hexadecimal digits."
	imageRule := "Set image_id in " + file + " to ami- and 8 or 17 lower-case hexadecimal digits."
	sizesRule := "Set sizes in " + file + " to an object of 1 to 20 members, each a size name of 1 to 40 characters of a-z, 0-9 and - that starts with a letter, set to an EC2 instance type such as t3.medium."
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
		{member("sizes", `{"standard": "t3.medium", "standard": "`+marker+`"}`), "Write each member once in " + file + "."},
		{`{"` + marker + `": 1}`, "Remove every member other than subnet_id, security_group_id, image_id and sizes from " + file + "."},
		{`{"run_subnet_id": "subnet-01234567"}`, "Remove every member other than subnet_id, security_group_id, image_id and sizes from " + file + "."},
		{`{}`, "Add the missing members to " + file + ": subnet_id, security_group_id, image_id, sizes."},
		{`{"subnet_id": "subnet-01234567", "sizes": {}}`, "Add the missing members to " + file + ": security_group_id, image_id."},
		{member("subnet_id", `"`+marker+`"`), subnetRule},
		{member("subnet_id", `"subnet-0123456"`), subnetRule},
		{member("subnet_id", `"subnet-0123456789ABCDEF0"`), subnetRule},
		{member("subnet_id", `7`), subnetRule},
		{member("subnet_id", `null`), subnetRule},
		{member("security_group_id", `"subnet-0123456789abcdef0"`), sgRule},
		{member("image_id", `"ami-`+marker+`"`), imageRule},
		{member("image_id", `["ami-0123456789abcdef0"]`), imageRule},
		{member("sizes", `{}`), sizesRule},
		{member("sizes", `{`+strings.Join(many, ", ")+`}`), sizesRule},
		{member("sizes", `{"`+marker+`": "t3.medium"}`), sizesRule},
		{member("sizes", `{"Standard": "t3.medium"}`), sizesRule},
		{member("sizes", `{"9large": "t3.medium"}`), sizesRule},
		{member("sizes", `{"a`+strings.Repeat("b", 40)+`": "t3.medium"}`), sizesRule},
		{member("sizes", `{"standard": "`+marker+`"}`), sizesRule},
		{member("sizes", `{"standard": "t3"}`), sizesRule},
		{member("sizes", `{"standard": "t3.-medium"}`), sizesRule},
		{member("sizes", `{"standard": "T3.medium"}`), sizesRule},
		{member("sizes", `{"standard": 3}`), sizesRule},
		{member("sizes", `"standard"`), sizesRule},
		{member("sizes", `[]`), sizesRule},
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
