// Package settings reads the provisioner's settings file: one JSON object, read
// strictly, that contains no secret. The access key is in the state folder. The
// enrolment code is in the environment, or in the file at enrolment_code_file.
package settings

import (
	"bytes"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
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
	// GatewayAddress is the gateway's IP address on the machines' network. It has no
	// zone, and it is not an unspecified, multicast or loopback address.
	GatewayAddress netip.Addr
	// Adapter is the machine adapter: its program and its settings file.
	Adapter Adapter
	// EnrolmentCodeFile is the path of the file that contains the node pool's enrolment
	// code, which Qory Apiary writes for a node pool installed with it, or "" when the
	// member is left out. It is a path as [Adapter]'s paths are.
	EnrolmentCodeFile string
}

// Adapter is the machine adapter's program and its settings file. Each is an absolute
// path that filepath.Clean leaves unchanged, of at most 4096 bytes.
type Adapter struct {
	// Command is the adapter program's path.
	Command string
	// Settings is the path of the adapter's settings file.
	Settings string
}

// members are the settings' members, in the order an error lists the missing ones.
var members = []string{"apiary_url", "gateway_address", "adapter", "enrolment_code_file"}

// optional are the members that may be left out.
var optional = []string{"enrolment_code_file"}

// adapterMembers are the members of adapter, in the order an error lists the missing
// ones. Neither may be left out.
var adapterMembers = []string{"command", "settings"}

// maxFile is the most bytes a settings file may contain.
const maxFile = 64 << 10

// maxPath is the most bytes of adapter.command, adapter.settings and
// enrolment_code_file.
const maxPath = 4096

// pathRule is the rule of adapter.command, adapter.settings and enrolment_code_file,
// before an example.
const pathRule = "an absolute path of at most 4096 bytes, with no . or .. element, no double slash, no trailing slash and no NUL byte, such as "

// The rule of each member, which the error of a wrong value states. An error never
// quotes the value.
var rules = map[string]string{
	"apiary_url":          "an https origin, or for a test an http origin on localhost, 127.0.0.1 or [::1]",
	"gateway_address":     "the gateway's IPv4 or IPv6 address on the machines' network, such as 10.0.1.5, with no zone and no port, and not an unspecified, multicast or loopback address",
	"adapter":             "an object with two members, command and settings",
	"adapter.command":     pathRule + "/usr/local/bin/qory-machines-aws-ec2",
	"adapter.settings":    pathRule + "/etc/qory-provisioner/aws-ec2.json",
	"enrolment_code_file": pathRule + "/run/qory-enrolment/provisioner/code, or leave it out",
}

// Read reads the settings file at path, as [Parse] reads its bytes. A file larger than
// 64 KiB is refused.
func Read(path string) (Settings, error) {
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
		return Settings{}, fmt.Errorf("Shorten the settings file %s to at most 64 KiB.", path)
	}
	return Parse(b)
}

// fileError is the error of opening or reading the settings file, op, that failed with
// err: a missing file is to be created, a permission error is to be fixed for the uid the
// provisioner runs as, and any other error is given with its operation.
func fileError(err error, op string) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("Create the settings file: %w", err)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("Make the settings file readable by uid %d: %w", os.Geteuid(), err)
	}
	return fmt.Errorf("Fix the settings file, then start the provisioner again: %s it failed: %w", op, err)
}

// Parse reads the settings document b: one JSON object, each member once, every member
// known, each value of its type and within its bounds. Settings that contain what looks
// like an access key secret, a run key, a run code or an enrolment code, in a value or a member's
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
	if err := checkNames(doc, members, optional, ""); err != nil {
		return Settings{}, err
	}
	var s Settings
	for _, name := range members {
		raw, ok := doc[name]
		if !ok {
			continue
		}
		var good bool
		switch name {
		case "apiary_url":
			s.ApiaryURL, good = origin(raw)
		case "gateway_address":
			s.GatewayAddress, good = address(raw)
		case "adapter":
			var err error
			if s.Adapter, err = adapter(raw); err != nil {
				return Settings{}, err
			}
			good = true
		case "enrolment_code_file":
			s.EnrolmentCodeFile, good = cleanPath(raw)
		}
		if !good {
			return Settings{}, wrong(name)
		}
	}
	return s, nil
}

// codePrefixes are the prefixes of an enrolment code, one for each kind of key it
// enrols.
var codePrefixes = []string{accesskey.CodePrefixNode, accesskey.CodePrefixPool, accesskey.CodePrefixGateway, accesskey.CodePrefixAdapter}

// runKeyPrefix starts a run key.
const runKeyPrefix = "qrk_"

// runCodePrefix starts a run code, which a machine redeems for its run key.
const runCodePrefix = "qecr_"

var (
	errSecret  = errors.New("Remove the access key secret (qak_) from the settings file: the provisioner stores its access key in the state folder.")
	errRunKey  = errors.New("Remove the run key (qrk_) from the settings file: a run key belongs in no file.")
	errRunCode = errors.New("Remove the run code (qecr_) from the settings file: a run code belongs in no file.")
	errCode    = errors.New("Remove the enrolment code (qecn_, qecp_, qecg_ or qeci_) from the settings file: pass a node pool's code in QORY_ENROLMENT_CODE, or in the file at enrolment_code_file.")
)

// secretIn is the error of settings b that contain what looks like an access key
// secret, a run key, a run code or an enrolment code, in any case, in their bytes or in a member's
// name or a string value as JSON decodes it, or nil. The decoded strings are checked
// too, since a JSON escape such as \u0071 hides a prefix from the bytes. The token
// reader takes duplicate members and invalid UTF-8, so the check reads every string
// up to the first syntax error, before the strict decoder refuses the document.
func secretIn(b []byte) error {
	var secret, runKey, runCode, code bool
	check := func(v string) {
		lower := strings.ToLower(v)
		secret = secret || strings.Contains(lower, accesskey.SecretPrefix)
		runKey = runKey || strings.Contains(lower, runKeyPrefix)
		runCode = runCode || strings.Contains(lower, runCodePrefix)
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
	case runCode:
		return errRunCode
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

// checkNames is the error of the object doc, at the path prefix, that has a member
// not in names, or lacks a member of names that is not in optional, or nil. It quotes
// the unknown names, which the check for a secret has already read.
func checkNames(doc map[string]jsontext.Value, names, optional []string, prefix string) error {
	var unknown, missing []string
	for name := range doc {
		if !slices.Contains(names, name) {
			unknown = append(unknown, strconv.Quote(prefix+name))
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return fmt.Errorf("Remove the unknown members from the settings file: %s.", strings.Join(unknown, ", "))
	}
	for _, name := range names {
		if _, ok := doc[name]; !ok && !slices.Contains(optional, name) {
			missing = append(missing, prefix+name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("Add the missing members to the settings file: %s.", strings.Join(missing, ", "))
	}
	return nil
}

// wrong is the error of the member name, such as adapter.command, whose value breaks
// its rule. It states the rule, never the value.
func wrong(name string) error {
	return fmt.Errorf("Set %s in the settings file to %s.", name, rules[name])
}

// address is an IPv4 or IPv6 address with no zone and no port. An unspecified,
// multicast or loopback address is refused, also when it is an IPv4 address mapped
// into IPv6.
func address(raw jsontext.Value) (netip.Addr, bool) {
	s, ok := text(raw)
	if !ok {
		return netip.Addr{}, false
	}
	a, err := netip.ParseAddr(s)
	u := a.Unmap()
	if err != nil || a.Zone() != "" || u.IsUnspecified() || u.IsMulticast() || u.IsLoopback() {
		return netip.Addr{}, false
	}
	return a, true
}

// adapter is the object of exactly the members command and settings, each a path
// cleanPath takes.
func adapter(raw jsontext.Value) (Adapter, error) {
	var doc map[string]jsontext.Value
	if raw.Kind() != '{' || jsonv2.Unmarshal(raw, &doc) != nil {
		return Adapter{}, wrong("adapter")
	}
	if err := checkNames(doc, adapterMembers, nil, "adapter."); err != nil {
		return Adapter{}, err
	}
	var a Adapter
	var good bool
	if a.Command, good = cleanPath(doc["command"]); !good {
		return Adapter{}, wrong("adapter.command")
	}
	if a.Settings, good = cleanPath(doc["settings"]); !good {
		return Adapter{}, wrong("adapter.settings")
	}
	return a, nil
}

// cleanPath is an absolute path of at most maxPath bytes that filepath.Clean leaves
// unchanged, with no NUL byte, which no file's path contains.
func cleanPath(raw jsontext.Value) (string, bool) {
	s, ok := text(raw)
	return s, ok && len(s) <= maxPath && filepath.IsAbs(s) && filepath.Clean(s) == s && !strings.ContainsRune(s, 0)
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
// contains what looks like a secret, a run code or an enrolment code is refused.
func CheckOrigin(server string) (string, error) {
	if accesskey.ContainsSecret(server) || HasCode(server) || strings.Contains(strings.ToLower(server), runCodePrefix) {
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
