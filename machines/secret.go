package machines

import (
	"encoding/base64"
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

// ContainsSecret is whether text contains what looks like a run code or the start
// data: qecr_ in any case; qecr_ in base64 at any byte offset, which a part of the
// start data in base64 contains; or startData, raw or as standard base64. An error
// message that contains a secret is replaced, by [Serve] and by the provisioner, so
// that no log line or error shows it.
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
	return strings.Contains(text, string(startData)) || strings.Contains(text, base64.StdEncoding.EncodeToString(startData))
}
