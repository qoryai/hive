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
	"unicode/utf8"

	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/hive/internal/machine"
	"github.com/qoryai/integrations/contracts"
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
	// Tags maps a tag key to its value, such as qory:stack to example-stack: at most 20
	// entries. Each machine and its volume get these tags after qory:managed,
	// qory:pool and qory:key. Tags is nil when the member tags is
	// absent or an empty object.
	Tags map[string]string
}

// members are the required members, in the order an error lists the missing ones.
var members = []string{"subnet_id", "security_group_id", "image_id"}

// tagsMember is the one optional member.
const tagsMember = "tags"

// maxFile is the most bytes a settings file may contain.
const maxFile = 64 << 10

// maxTags is the most entries of tags. EC2 accepts at most 50 tags on a resource, and
// the adapter adds 3: qory:managed, qory:pool and qory:key.
const maxTags = 20

// The bounds of a tag key and a tag value, in characters, as EC2 accepts them.
const (
	maxTagKey   = 128
	maxTagValue = 256
)

// settingsFile is how an error refers to the adapter's settings file.
const settingsFile = "aws-ec2.json"

// The rule of each member, which the error of a wrong value states. An error never
// quotes a value.
var rules = map[string]string{
	"subnet_id":         "subnet- and 8 or 17 lower-case hexadecimal digits",
	"security_group_id": "sg- and 8 or 17 lower-case hexadecimal digits",
	"image_id":          "ami- and 8 or 17 lower-case hexadecimal digits",
	tagsMember:          "an object of at most 20 members, each a tag key of 1 to 128 characters set to a string of 0 to 256 characters",
}

// The errors of tags beyond its rule, in the order tags checks them, so a document
// with several faults gets the same error on every read.
const (
	msgTagText    = "Write the keys and values of tags in " + settingsFile + " with only letters, digits, spaces and _ . : / = + - @."
	msgTagAWS     = "Remove every key that starts with aws: from tags in " + settingsFile + ": AWS reserves that prefix."
	msgTagAdapter = "Remove qory:managed, qory:pool and qory:key, in any case, from tags in " + settingsFile + ": the adapter sets these tags on every machine."
)

// msgUnknownMember is the error of a member that is neither required nor tags.
const msgUnknownMember = "Remove every member other than subnet_id, security_group_id, image_id and tags from " + settingsFile + "."

var (
	subnetShape        = regexp.MustCompile(`^subnet-([0-9a-f]{8}|[0-9a-f]{17})$`)
	securityGroupShape = regexp.MustCompile(`^sg-([0-9a-f]{8}|[0-9a-f]{17})$`)
	imageShape         = regexp.MustCompile(`^ami-([0-9a-f]{8}|[0-9a-f]{17})$`)
	// tagText is the characters that EC2 accepts in a tag key and a tag value.
	tagText = regexp.MustCompile(`^[\p{L}\p{Z}\p{N}_.:/=+\-@]*$`)
)

// ReadSettings reads the settings file at path, as [ParseSettings] reads its bytes. A
// file larger than 64 KiB is refused. Its errors are *adapterio.Failure of the kind
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
// uid the adapter runs as, and any other error is given with its operation. The file's
// path from a *fs.PathError comes last, so the cut of a long message to
// adapterio.MaxMessage characters removes the end of the path, not the cause.
func fileError(err error, op string) error {
	cause, path := err, ""
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		cause, path = pathErr.Err, ": "+pathErr.Path
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return settingsError(fmt.Sprintf("Create %s at the path in the provisioner's adapter.settings: %s it failed: %v%s", settingsFile, op, cause, path))
	case errors.Is(err, fs.ErrPermission):
		return settingsError(fmt.Sprintf("Make %s readable by uid %d: %s it failed: %v%s", settingsFile, os.Geteuid(), op, cause, path))
	}
	return settingsError(fmt.Sprintf("Fix %s: %s it failed: %v%s", settingsFile, op, cause, path))
}

// ParseSettings reads the settings document b: one JSON object, each member once,
// every member known, every required member present, each value of its type and
// within its bounds. An error starts with the fix, names the settings file, and
// quotes no value and no member name of b. Its errors are *adapterio.Failure of the
// kind defect.
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
		if name != tagsMember && !slices.Contains(members, name) {
			return Settings{}, settingsError(msgUnknownMember)
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
		}
		if !good {
			return Settings{}, settingsError(ruleError(name))
		}
	}
	if raw, ok := doc[tagsMember]; ok {
		var msg string
		if s.Tags, msg = tags(raw); msg != "" {
			return Settings{}, settingsError(msg)
		}
	}
	return s, nil
}

// settingsError is a failure of the kind defect with message.
func settingsError(message string) error {
	return &adapterio.Failure{Kind: contracts.FailureDefect, Message: message}
}

// matching is a string that shape matches.
func matching(raw jsontext.Value, shape *regexp.Regexp) (string, bool) {
	var s string
	if raw.Kind() != '"' || jsonv2.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, shape.MatchString(s)
}

// tags reads the member tags: an object of at most maxTags members, each a key of 1
// to maxTagKey characters set to a string of 0 to maxTagValue characters, both of the
// characters of tagText. A key may not start with aws: in any case, and may not be a
// tag key of the adapter in any case. It returns the tags, nil for an empty object,
// and "" or the error message. Of several faults, the message is the one of the first
// check in that order.
func tags(raw jsontext.Value) (map[string]string, string) {
	var m map[string]jsontext.Value
	if raw.Kind() != '{' || jsonv2.Unmarshal(raw, &m) != nil || len(m) > maxTags {
		return nil, ruleError(tagsMember)
	}
	out := make(map[string]string, len(m))
	for key, v := range m {
		var value string
		if v.Kind() != '"' || jsonv2.Unmarshal(v, &value) != nil {
			return nil, ruleError(tagsMember)
		}
		if n := utf8.RuneCountInString(key); n < 1 || n > maxTagKey || utf8.RuneCountInString(value) > maxTagValue {
			return nil, ruleError(tagsMember)
		}
		out[key] = value
	}
	for _, check := range []struct {
		fails func(key, value string) bool
		msg   string
	}{
		{func(k, v string) bool { return !tagText.MatchString(k) || !tagText.MatchString(v) }, msgTagText},
		{func(k, _ string) bool { return len(k) >= 4 && strings.EqualFold(k[:4], "aws:") }, msgTagAWS},
		{func(k, _ string) bool { return adapterTag(k) }, msgTagAdapter},
	} {
		for k, v := range out {
			if check.fails(k, v) {
				return nil, check.msg
			}
		}
	}
	if len(out) == 0 {
		return nil, ""
	}
	return out, ""
}

// adapterTag reports whether key is qory:managed, qory:pool or qory:key in any case.
func adapterTag(key string) bool {
	for _, t := range []string{machine.TagManaged, machine.TagPool, machine.TagKey} {
		if strings.EqualFold(key, t) {
			return true
		}
	}
	return false
}

// ruleError is the error message of a wrong value of the member name, which states
// the member's rule.
func ruleError(name string) string {
	return fmt.Sprintf("Set %s in %s to %s.", name, settingsFile, rules[name])
}
