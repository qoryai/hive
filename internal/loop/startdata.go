package loop

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"regexp"

	"github.com/qoryai/hive/internal/journal"
	"github.com/qoryai/integrations/contracts"
)

// hiddenRunCode is the text of a [RunCode] in every format.
const hiddenRunCode = "[run code]"

// RunCode is the run code of a claim answer, which the machine redeems once. It
// formats, logs and encodes as "[run code]", so no log line or error that contains a
// RunCode shows its text. Only the start data contains the text.
//
// The text is a Go string, which no code can overwrite: it stays in memory until the
// garbage collector reuses its bytes. The loop clears the start data, a byte slice,
// at the end of the claim that built it, and the adapter runner clears its request
// body after the adapter ends. The make request's base64 copy of the start data is a
// string too, and is not cleared.
type RunCode struct{ text string }

// NewRunCode returns the run code s.
func NewRunCode(s string) RunCode { return RunCode{text: s} }

// String returns "[run code]".
func (RunCode) String() string { return hiddenRunCode }

// GoString returns "[run code]".
func (RunCode) GoString() string { return hiddenRunCode }

// Format writes "[run code]" for every verb.
func (RunCode) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, hiddenRunCode) }

// LogValue returns "[run code]".
func (RunCode) LogValue() slog.Value { return slog.StringValue(hiddenRunCode) }

// MarshalJSON returns "[run code]" as a JSON string.
func (RunCode) MarshalJSON() ([]byte, error) { return []byte(`"` + hiddenRunCode + `"`), nil }

// MarshalText returns "[run code]".
func (RunCode) MarshalText() ([]byte, error) { return []byte(hiddenRunCode), nil }

// runCodeShape is the shape of a run code in the provisioner contract.
var runCodeShape = regexp.MustCompile(`^qecr_[0-9A-HJKMNP-TV-Z]{26}(\.[A-Za-z0-9_-]{22}){1,2}$`)

// startData is version 1 of the start data, which the machine's start unit reads.
type startData struct {
	Version            int     `json:"version"`
	RunCode            string  `json:"run_code"`
	Gateway            gateway `json:"gateway"`
	MaxLifetimeMinutes int     `json:"max_lifetime_minutes"`
}

// gateway is the gateway member of the start data.
type gateway struct {
	URL     string `json:"url"`
	Address string `json:"address"`
}

// errStartData is the error of a claim whose values do not make valid start data.
var errStartData = errors.New("the claim's run code, gateway URL or lifetime does not fit in the start data, which takes a run code of the contract's shape, a lifetime of 15 to 1470 minutes, no backslash and at most 16384 bytes")

// buildStartData is the start data of the claim c, with the gateway's address a:
// {"version":1,"run_code":…,"gateway":{"url":…,"address":…},"max_lifetime_minutes":…}.
// An IPv4 address mapped into IPv6 is written in its IPv4 form. The start data
// contains no backslash, since the start unit refuses one, and is at most
// [contracts.MaxStartData] bytes.
func buildStartData(c Claim, a netip.Addr) ([]byte, error) {
	if !runCodeShape.MatchString(c.RunCode.text) || c.MaxLifetimeMinutes < journal.MinLifetime || c.MaxLifetimeMinutes > journal.MaxLifetime {
		return nil, errStartData
	}
	b, err := jsonv2.Marshal(startData{
		Version:            1,
		RunCode:            c.RunCode.text,
		Gateway:            gateway{URL: c.GatewayURL, Address: a.Unmap().String()},
		MaxLifetimeMinutes: c.MaxLifetimeMinutes,
	})
	if err != nil || bytes.ContainsRune(b, '\\') || len(b) > contracts.MaxStartData {
		return nil, errStartData
	}
	return b, nil
}
