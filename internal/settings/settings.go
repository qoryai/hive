// Package settings reads the provisioner's settings file: one JSON object, read
// strictly, that contains no secret. The access key is in the state folder, and the
// enrolment code is in the environment.
package settings

import (
	"bytes"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/qoryai/forager/accesskey"
)

// Settings are the provisioner's settings.
type Settings struct {
	// ApiaryURL is Qory Apiary's origin: https, or for a test http on localhost,
	// 127.0.0.1 or [::1]. The scheme and the host are in lower case, with no trailing
	// slash.
	ApiaryURL string
	// RunSubnetID is the subnet each machine starts in, subnet- and 8 or 17 hexadecimal
	// digits.
	RunSubnetID string
	// RunSecurityGroupID is the security group of each machine, sg- and 8 or 17
	// hexadecimal digits.
	RunSecurityGroupID string
	// MachineImageID is the image each machine starts from, ami- and 8 or 17
	// hexadecimal digits.
	MachineImageID string
	// MachineTypes maps a machine type's name to an EC2 instance type, such as small to
	// t3.medium: 1 to 20 entries.
	MachineTypes map[string]string
	// GatewayName is the DNS name of the gateway a machine's run connects to.
	GatewayName string
	// GatewayPort is the gateway's port, 1 to 65535.
	GatewayPort int
	// MaxMachines is the most machines that run at once, 1 to 100.
	MaxMachines int
	// MaxLifetimeMinutes is the most minutes a machine runs before it powers off, 5 to
	// 720.
	MaxLifetimeMinutes int
}

// The defaults of the members that may be left out.
const (
	// DefaultMaxMachines is max_machines when the member is left out.
	DefaultMaxMachines = 1
	// DefaultMaxLifetimeMinutes is max_lifetime_minutes when the member is left out.
	DefaultMaxLifetimeMinutes = 60
)

// members are the settings' members, in the order an error lists the missing ones.
var members = []string{
	"apiary_url", "run_subnet_id", "run_security_group_id", "machine_image_id", "machine_types",
	"gateway_name", "gateway_port", "max_machines", "max_lifetime_minutes",
}

// optional are the members that may be left out.
var optional = []string{"max_machines", "max_lifetime_minutes"}

// maxFile is the most bytes a settings file may contain.
const maxFile = 64 << 10

// The rule of each member, which the error of a wrong value states. An error never
// quotes the value.
var rules = map[string]string{
	"apiary_url":            "an https origin, or for a test an http origin on localhost, 127.0.0.1 or [::1]",
	"run_subnet_id":         "subnet- and 8 or 17 lower-case hexadecimal digits",
	"run_security_group_id": "sg- and 8 or 17 lower-case hexadecimal digits",
	"machine_image_id":      "ami- and 8 or 17 lower-case hexadecimal digits",
	"machine_types":         "an object of 1 to 20 members, each a name of 1 to 40 lower-case letters, digits and dashes that starts with a letter, set to an EC2 instance type such as t3.medium",
	"gateway_name":          "a DNS name in lower case, such as gateway.example.com",
	"gateway_port":          "a whole number from 1 to 65535",
	"max_machines":          "a whole number from 1 to 100, or leave it out for 1",
	"max_lifetime_minutes":  "a whole number from 5 to 720, or leave it out for 60",
}

var (
	subnetShape        = regexp.MustCompile(`^subnet-([0-9a-f]{8}|[0-9a-f]{17})$`)
	securityGroupShape = regexp.MustCompile(`^sg-([0-9a-f]{8}|[0-9a-f]{17})$`)
	imageShape         = regexp.MustCompile(`^ami-([0-9a-f]{8}|[0-9a-f]{17})$`)
	typeNameShape      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	instanceTypeShape  = regexp.MustCompile(`^[a-z][a-z0-9-]*\.[a-z0-9]+$`)
	labelShape         = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	digits             = regexp.MustCompile(`^[0-9]+$`)
	integerShape       = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
)

// Read reads the settings file at path, as [Parse] reads its bytes. A file larger than
// 64 KiB is refused.
func Read(path string) (Settings, error) {
	f, err := os.Open(path)
	if err != nil {
		return Settings{}, fmt.Errorf("Create the settings file: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return Settings{}, fmt.Errorf("Make the settings file readable: %w", err)
	}
	if len(b) > maxFile {
		return Settings{}, fmt.Errorf("Shorten the settings file %s to at most 64 KiB.", path)
	}
	return Parse(b)
}

// Parse reads the settings document b: one JSON object, each member once, every member
// known, each value of its type and within its bounds. Settings that contain what looks
// like an access key secret, a run key or an enrolment code, in a value or a member's
// name, are refused before anything else. An error starts with the fix, and never
// quotes a value; it quotes a member's name only after the check for a secret.
func Parse(b []byte) (Settings, error) {
	if err := secretIn(b); err != nil {
		return Settings{}, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return Settings{}, errors.New("Write the settings file as one JSON object: the file is empty.")
	}
	var doc map[string]jsontext.Value
	err := jsonv2.Unmarshal(b, &doc)
	var syntax *jsontext.SyntacticError
	switch {
	case errors.Is(err, jsontext.ErrDuplicateName) && errors.As(err, &syntax):
		return Settings{}, fmt.Errorf("Write each member once in the settings file: %s appears more than once.", strconv.Quote(string(syntax.JSONPointer)))
	case errors.As(err, &syntax):
		return Settings{}, fmt.Errorf("Fix the JSON in the settings file at byte offset %d.", syntax.ByteOffset)
	case err != nil || doc == nil:
		return Settings{}, errors.New("Write the settings file as one JSON object.")
	}
	var unknown, missing []string
	for name := range doc {
		if !slices.Contains(members, name) {
			unknown = append(unknown, strconv.Quote(name))
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return Settings{}, fmt.Errorf("Remove the unknown members from the settings file: %s.", strings.Join(unknown, ", "))
	}
	for _, name := range members {
		if _, ok := doc[name]; !ok && !slices.Contains(optional, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return Settings{}, fmt.Errorf("Add the missing members to the settings file: %s.", strings.Join(missing, ", "))
	}
	s := Settings{MaxMachines: DefaultMaxMachines, MaxLifetimeMinutes: DefaultMaxLifetimeMinutes}
	for _, name := range members {
		raw, ok := doc[name]
		if !ok {
			continue
		}
		var good bool
		switch name {
		case "apiary_url":
			s.ApiaryURL, good = origin(raw)
		case "run_subnet_id":
			s.RunSubnetID, good = matching(raw, subnetShape)
		case "run_security_group_id":
			s.RunSecurityGroupID, good = matching(raw, securityGroupShape)
		case "machine_image_id":
			s.MachineImageID, good = matching(raw, imageShape)
		case "machine_types":
			s.MachineTypes, good = machineTypes(raw)
		case "gateway_name":
			s.GatewayName, good = dnsName(raw)
		case "gateway_port":
			s.GatewayPort, good = integer(raw, 1, 65535)
		case "max_machines":
			s.MaxMachines, good = integer(raw, 1, 100)
		case "max_lifetime_minutes":
			s.MaxLifetimeMinutes, good = integer(raw, 5, 720)
		}
		if !good {
			return Settings{}, fmt.Errorf("Set %s in the settings file to %s.", name, rules[name])
		}
	}
	return s, nil
}

// codePrefixes are the prefixes of an enrolment code, one for each kind of key it
// enrols.
var codePrefixes = []string{accesskey.CodePrefixNode, accesskey.CodePrefixPool, accesskey.CodePrefixGateway, accesskey.CodePrefixAdapter}

// runKeyPrefix starts a run key.
const runKeyPrefix = "qrk_"

var (
	errSecret = errors.New("Remove the access key secret (qak_) from the settings file: the provisioner stores its access key in the state folder.")
	errRunKey = errors.New("Remove the run key (qrk_) from the settings file: a run key belongs in no file.")
	errCode   = errors.New("Remove the enrolment code (qecn_, qecp_, qecg_ or qeci_) from the settings file: pass a node pool's code in QORY_ENROLMENT_CODE.")
)

// secretIn is the error of settings b that contain what looks like an access key
// secret, a run key or an enrolment code, in any case, in their bytes or in a member's
// name or a string value as JSON decodes it, or nil. The decoded strings are checked
// too, since a JSON escape such as \u0071 hides a prefix from the bytes. The token
// reader takes duplicate members and invalid UTF-8, so the check reads every string
// up to the first syntax error, before the strict decoder refuses the document.
func secretIn(b []byte) error {
	var secret, runKey, code bool
	check := func(v string) {
		lower := strings.ToLower(v)
		secret = secret || strings.Contains(lower, accesskey.SecretPrefix)
		runKey = runKey || strings.Contains(lower, runKeyPrefix)
		code = code || HasCode(v)
	}
	check(string(b))
	dec := jsontext.NewDecoder(bytes.NewReader(b), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	for {
		tok, err := dec.ReadToken()
		if err != nil {
			break
		}
		if tok.Kind() == '"' {
			check(tok.String())
		}
	}
	switch {
	case secret:
		return errSecret
	case runKey:
		return errRunKey
	case code:
		return errCode
	}
	return nil
}

// HasCode reports whether v contains what starts an enrolment code of any kind, qecn_,
// qecp_, qecg_ or qeci_, in any case.
func HasCode(v string) bool {
	lower := strings.ToLower(v)
	for _, p := range codePrefixes {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// text is a value that is a string that is not empty.
func text(raw jsontext.Value) (string, bool) {
	var s string
	if raw.Kind() != '"' || jsonv2.Unmarshal(raw, &s) != nil || s == "" {
		return "", false
	}
	return s, true
}

// matching is a string that shape matches.
func matching(raw jsontext.Value, shape *regexp.Regexp) (string, bool) {
	s, ok := text(raw)
	return s, ok && shape.MatchString(s)
}

// integer is a JSON number written as a whole number, with no fraction and no
// exponent, from lo to hi.
func integer(raw jsontext.Value, lo, hi int) (int, bool) {
	s := string(bytes.TrimSpace(raw))
	if raw.Kind() != '0' || !integerShape.MatchString(s) {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && lo <= n && n <= hi
}

// machineTypes is an object of 1 to 20 members, each a name typeNameShape matches set
// to an instance type instanceTypeShape matches.
func machineTypes(raw jsontext.Value) (map[string]string, bool) {
	var m map[string]jsontext.Value
	if raw.Kind() != '{' || jsonv2.Unmarshal(raw, &m) != nil || len(m) < 1 || len(m) > 20 {
		return nil, false
	}
	types := make(map[string]string, len(m))
	for name, v := range m {
		t, ok := matching(v, instanceTypeShape)
		if !ok || !typeNameShape.MatchString(name) {
			return nil, false
		}
		types[name] = t
	}
	return types, true
}

// dnsName is a DNS name in lower case: at most 253 characters, labels of 1 to 63
// letters, digits and dashes that neither start nor end with a dash, and a last label
// that is not all digits, so an IPv4 address is no name.
func dnsName(raw jsontext.Value) (string, bool) {
	s, ok := text(raw)
	if !ok || len(s) > 253 {
		return "", false
	}
	labels := strings.Split(s, ".")
	for _, l := range labels {
		if !labelShape.MatchString(l) {
			return "", false
		}
	}
	return s, !digits.MatchString(labels[len(labels)-1])
}

// origin is Qory Apiary's origin, as [CheckOrigin] takes it, with one trailing slash
// dropped.
func origin(raw jsontext.Value) (string, bool) {
	s, ok := text(raw)
	if !ok {
		return "", false
	}
	o, err := CheckOrigin(strings.TrimSuffix(s, "/"))
	return o, err == nil
}

// errOrigin is the error of a value that is no origin CheckOrigin takes. It names the
// rule, never the value.
var errOrigin = errors.New("not an https origin, or for a test an http origin on localhost, 127.0.0.1 or [::1]")

// CheckOrigin takes Qory Apiary's origin, as accesskey's enrolment posts to it: an
// https URL of a host and an optional port, or for a test an http one to localhost,
// 127.0.0.1 or [::1], with nothing after. A host is taken in ASCII alone, an
// international name in its xn-- form. It returns the origin with its scheme and the
// ASCII letters of its host in lower case, an IPv6 zone kept as it is. An origin that
// contains what looks like a secret or an enrolment code is refused.
func CheckOrigin(server string) (string, error) {
	if accesskey.ContainsSecret(server) || HasCode(server) {
		return "", errOrigin
	}
	u, err := url.Parse(server)
	if err != nil || u.Opaque != "" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(server, "#") {
		return "", errOrigin
	}
	for i := 0; i < len(u.Host); i++ {
		if u.Host[i] >= utf8.RuneSelf {
			return "", errOrigin
		}
	}
	if !hostShape(u.Host) {
		return "", errOrigin
	}
	o := (&url.URL{Scheme: u.Scheme, Host: lowerHost(u.Host)}).String()
	switch h := strings.ToLower(u.Hostname()); {
	case u.Scheme == "https":
		return o, nil
	case u.Scheme == "http" && (h == "localhost" || h == "127.0.0.1" || h == "::1"):
		return o, nil
	}
	return "", errOrigin
}

// zoneShape is an IPv6 zone, the name of an interface.
var zoneShape = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// hostShape reports whether host, as url.Parse decodes it, is a host and an optional
// port: a name or an IPv4 address of letters, digits, dots and dashes, or an IPv6
// address in brackets with an optional zone, and after a colon a port from 1 to 65535
// written as Go writes the number.
func hostShape(host string) bool {
	name := host
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.HasSuffix(host, "]") {
		port := host[i+1:]
		name = host[:i]
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return false
		}
	}
	if strings.HasPrefix(name, "[") {
		addr, ok := strings.CutSuffix(name[1:], "]")
		if !ok {
			return false
		}
		if a, zone, found := strings.Cut(addr, "%"); found {
			if !zoneShape.MatchString(zone) {
				return false
			}
			addr = a
		}
		ip := net.ParseIP(addr)
		return ip != nil && strings.Contains(addr, ":")
	}
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

// lowerHost is an ASCII host with its letters in lower case, but for the zone of an
// IPv6 address, from its % to its ], which names an interface as the system spells it.
func lowerHost(host string) string {
	b := []byte(host)
	zone := false
	for i, c := range b {
		switch {
		case c == '%' && strings.HasPrefix(host, "["):
			zone = true
		case c == ']':
			zone = false
		case !zone && 'A' <= c && c <= 'Z':
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
