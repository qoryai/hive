package awsec2

import (
	"bytes"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/qoryai/hive/machines"
)

// Settings are the EC2 adapter's settings, from its settings file aws-ec2.json.
type Settings struct {
	// SubnetID is the run subnet, where each machine's network interface is:
	// subnet- and 8 or 17 hexadecimal digits.
	SubnetID string
	// SecurityGroupID is the security group of each machine's network interface: sg-
	// and 8 or 17 hexadecimal digits.
	SecurityGroupID string
	// ImageID is the image each machine starts from: ami- and 8 or 17 hexadecimal
	// digits.
	ImageID string
	// Sizes maps a size name of a make request to an EC2 instance type, such as
	// standard to t3.medium: 1 to 20 entries.
	Sizes map[string]string
}

// members are the settings' members, in the order an error lists the missing ones.
// Every member is required.
var members = []string{"subnet_id", "security_group_id", "image_id", "sizes"}

// maxFile is the most bytes a settings file may contain.
const maxFile = 64 << 10

// maxSizes is the most entries of sizes.
const maxSizes = 20

// settingsFile is how an error refers to the adapter's settings file.
const settingsFile = "the EC2 adapter's settings file aws-ec2.json"

// The rule of each member, which the error of a wrong value states. An error never
// quotes a value.
var rules = map[string]string{
	"subnet_id":         "subnet- and 8 or 17 lower-case hexadecimal digits",
	"security_group_id": "sg- and 8 or 17 lower-case hexadecimal digits",
	"image_id":          "ami- and 8 or 17 lower-case hexadecimal digits",
	"sizes":             "an object of 1 to 20 members, each a size name of 1 to 40 lower-case letters, digits and dashes that starts with a letter, set to an EC2 instance type such as t3.medium",
}

var (
	subnetShape        = regexp.MustCompile(`^subnet-([0-9a-f]{8}|[0-9a-f]{17})$`)
	securityGroupShape = regexp.MustCompile(`^sg-([0-9a-f]{8}|[0-9a-f]{17})$`)
	imageShape         = regexp.MustCompile(`^ami-([0-9a-f]{8}|[0-9a-f]{17})$`)
	sizeShape          = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	instanceTypeShape  = regexp.MustCompile(`^[a-z][a-z0-9-]*\.[a-z0-9][a-z0-9-]*$`)
)

// ReadSettings reads the settings file at path, as [ParseSettings] reads its bytes. A
// file larger than 64 KiB is refused. Its errors are *machines.Error of the kind
// defect.
func ReadSettings(path string) (Settings, error) {
	f, err := os.Open(path)
	if err != nil {
		return Settings{}, fileError(err, "opening")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return Settings{}, fileError(err, "reading")
	}
	if len(b) > maxFile {
		return Settings{}, settingsError("Shorten " + settingsFile + " to at most 64 KiB.")
	}
	return ParseSettings(b)
}

// fileError is the error of opening or reading the settings file, op, that failed
// with err: a missing file is to be created, a permission error is to be fixed for the
// uid the adapter runs as, and any other error is given with its operation. The error
// of the operating system contains the file's path.
func fileError(err error, op string) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return settingsError(fmt.Sprintf("Create %s at the path in the provisioner's adapter.settings: %v", settingsFile, err))
	case errors.Is(err, fs.ErrPermission):
		return settingsError(fmt.Sprintf("Make %s readable by uid %d: %v", settingsFile, os.Geteuid(), err))
	}
	return settingsError(fmt.Sprintf("Fix %s: %s it failed: %v", settingsFile, op, err))
}

// ParseSettings reads the settings document b: one JSON object, each member once,
// every member known and present, each value of its type and within its bounds. An
// error starts with the fix, names the settings file, and quotes no value and no
// member name of b. Its errors are *machines.Error of the kind defect.
func ParseSettings(b []byte) (Settings, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return Settings{}, settingsError("Write " + settingsFile + " as one JSON object: the file is empty.")
	}
	var doc map[string]jsontext.Value
	err := jsonv2.Unmarshal(b, &doc)
	var syntax *jsontext.SyntacticError
	switch {
	case errors.Is(err, jsontext.ErrDuplicateName):
		return Settings{}, settingsError("Write each member once in " + settingsFile + ".")
	case errors.As(err, &syntax):
		return Settings{}, settingsError(fmt.Sprintf("Fix the JSON in %s at byte offset %d.", settingsFile, syntax.ByteOffset))
	case err != nil || doc == nil:
		return Settings{}, settingsError("Write " + settingsFile + " as one JSON object.")
	}
	for name := range doc {
		if !slices.Contains(members, name) {
			return Settings{}, settingsError("Remove every member other than subnet_id, security_group_id, image_id and sizes from " + settingsFile + ".")
		}
	}
	var missing []string
	for _, name := range members {
		if _, ok := doc[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return Settings{}, settingsError(fmt.Sprintf("Add the missing members to %s: %s.", settingsFile, strings.Join(missing, ", ")))
	}
	var s Settings
	for _, name := range members {
		raw := doc[name]
		var good bool
		switch name {
		case "subnet_id":
			s.SubnetID, good = matching(raw, subnetShape)
		case "security_group_id":
			s.SecurityGroupID, good = matching(raw, securityGroupShape)
		case "image_id":
			s.ImageID, good = matching(raw, imageShape)
		case "sizes":
			s.Sizes, good = sizes(raw)
		}
		if !good {
			return Settings{}, settingsError(fmt.Sprintf("Set %s in %s to %s.", name, settingsFile, rules[name]))
		}
	}
	return s, nil
}

// settingsError is a failure of the kind defect with message.
func settingsError(message string) error {
	return &machines.Error{Kind: machines.KindDefect, Message: message}
}

// matching is a string that shape matches.
func matching(raw jsontext.Value, shape *regexp.Regexp) (string, bool) {
	var s string
	if raw.Kind() != '"' || jsonv2.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, shape.MatchString(s)
}

// sizes is an object of 1 to 20 members, each a name that sizeShape matches set to an
// instance type that instanceTypeShape matches.
func sizes(raw jsontext.Value) (map[string]string, bool) {
	var m map[string]jsontext.Value
	if raw.Kind() != '{' || jsonv2.Unmarshal(raw, &m) != nil || len(m) < 1 || len(m) > maxSizes {
		return nil, false
	}
	out := make(map[string]string, len(m))
	for name, v := range m {
		t, ok := matching(v, instanceTypeShape)
		if !ok || !sizeShape.MatchString(name) {
			return nil, false
		}
		out[name] = t
	}
	return out, true
}
