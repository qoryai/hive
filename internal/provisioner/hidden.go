package provisioner

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

// hiddenText is how a [Hidden] prints, logs and marshals.
const hiddenText = "[hidden]"

// Hidden keeps the bytes of a value out of every log, error and printout: a handover's
// run key and inputs, and a work item's title. It prints as [hidden] for every verb, and
// logs and marshals as [hidden] too. The bytes sit behind two pointers, so a Hidden
// inside another value, even in a field whose methods fmt cannot call, prints as an
// address: fmt follows one pointer to a slice when a field does not take the verb, and
// no pointer to a pointer. A printer that follows pointers by reflection shows the
// bytes. Copies of a Hidden share the bytes, and [Hidden.Clear] clears them for every
// copy. The zero Hidden is empty.
type Hidden struct{ p **[]byte }

// Hide is a Hidden of b. Hide takes b: the caller keeps no other copy, and clears the
// bytes with [Hidden.Clear] once they are used.
func Hide(b []byte) Hidden {
	p := &b
	return Hidden{&p}
}

// Len is the number of bytes, 0 once cleared.
func (h Hidden) Len() int {
	if h.p == nil {
		return 0
	}
	return len(**h.p)
}

// WriteTo writes the bytes to w. It is the one way the bytes leave the Hidden.
func (h Hidden) WriteTo(w io.Writer) (int64, error) {
	if h.p == nil {
		return 0, nil
	}
	n, err := w.Write(**h.p)
	return int64(n), err
}

// Clear overwrites the bytes with zeros and drops them, for this Hidden and every copy
// of it.
func (h Hidden) Clear() {
	if h.p == nil {
		return
	}
	clear(**h.p)
	**h.p = nil
}

// String is [hidden].
func (h Hidden) String() string { return hiddenText }

// GoString is [hidden].
func (h Hidden) GoString() string { return hiddenText }

// Format writes [hidden] for every verb.
func (h Hidden) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, hiddenText) }

// LogValue is [hidden].
func (h Hidden) LogValue() slog.Value { return slog.StringValue(hiddenText) }

// MarshalJSON is [hidden], as a JSON string.
func (h Hidden) MarshalJSON() ([]byte, error) { return json.Marshal(hiddenText) }

// MarshalText is [hidden].
func (h Hidden) MarshalText() ([]byte, error) { return []byte(hiddenText), nil }
