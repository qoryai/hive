package machines

import (
	"encoding/base64"
	"encoding/json/v2"
	"strings"
)

// runCodePrefix starts every run code.
const runCodePrefix = "qecr_"

// runCodeBase64 are the parts of runCodePrefix in base64 that do not depend on the
// bytes around it, one for each of the three byte offsets in a group of 3 bytes, in
// the standard and the URL alphabet.
var runCodeBase64 = func() []string {
	var out []string
	n := len(runCodePrefix)
	for offset := range 3 {
		b := append(make([]byte, offset), runCodePrefix...)
		enc := base64.StdEncoding.EncodeToString(append(b, 0, 0))
		// Character i encodes bits 6i to 6i+5, so the prefix alone fills characters
		// ceil(8*offset/6) up to, not including, floor(8*(offset+n)/6).
		part := enc[(offset*8+5)/6 : (offset+n)*8/6]
		out = append(out, part)
		if url := strings.NewReplacer("+", "-", "/", "_").Replace(part); url != part {
			out = append(out, url)
		}
	}
	return out
}()

// The window lengths of [ContainsSecret].
const (
	// StartDataWindow is the length of a piece of the start data in base64 that
	// [ContainsSecret] finds.
	StartDataWindow = 16
	// RunCodeWindow is the length of a piece of the run code after qecr_ that
	// [ContainsSecret] finds.
	RunCodeWindow = 12
)

// ContainsSecret is whether text contains what looks like a run code or a piece of
// the start data:
//   - qecr_ in any case;
//   - qecr_ in base64 at any of the three byte offsets;
//   - startData, raw;
//   - any [StartDataWindow] characters in a row of startData in standard base64, the
//     string that the make request contains, or all of it when it is shorter;
//   - any [RunCodeWindow] characters in a row of the run code after qecr_, or all of
//     it when it is shorter, when startData is a JSON object with a run_code string.
//
// An error message that contains a secret is replaced, by [Serve] and by the
// provisioner, so that no log line or error shows it.
func ContainsSecret(text string, startData []byte) bool {
	if strings.Contains(strings.ToLower(text), runCodePrefix) {
		return true
	}
	for _, part := range runCodeBase64 {
		if strings.Contains(text, part) {
			return true
		}
	}
	if len(startData) == 0 {
		return false
	}
	if strings.Contains(text, string(startData)) {
		return true
	}
	if containsWindow(text, base64.StdEncoding.EncodeToString(startData), StartDataWindow) {
		return true
	}
	var doc struct {
		RunCode string `json:"run_code"`
	}
	if json.Unmarshal(startData, &doc) != nil {
		return false
	}
	body := strings.TrimPrefix(doc.RunCode, runCodePrefix)
	return body != "" && containsWindow(text, body, RunCodeWindow)
}

// containsWindow is whether text contains any n bytes in a row of secret, or all of
// secret when it is shorter than n.
func containsWindow(text, secret string, n int) bool {
	if len(secret) <= n {
		return strings.Contains(text, secret)
	}
	if len(text) < n {
		return false
	}
	pieces := make(map[string]struct{}, len(text)-n+1)
	for i := 0; i+n <= len(text); i++ {
		pieces[text[i:i+n]] = struct{}{}
	}
	for i := 0; i+n <= len(secret); i++ {
		if _, ok := pieces[secret[i:i+n]]; ok {
			return true
		}
	}
	return false
}
