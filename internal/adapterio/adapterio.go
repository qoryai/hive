// Package machines is version 1 of the machine adapter protocol: the documents that the
// provisioner and a machine adapter exchange, their bounds, strict decoders, a JSON
// Schema for each document, and [Serve], which runs one command of an adapter written
// in Go.
//
// A machine adapter is a separate program that makes, lists, reads and deletes
// machines at one vendor. The provisioner runs it as
//
//	<adapter> <command> --settings FILE
//
// where <command> is check, make, list, log or delete, and FILE is the adapter's
// settings file. The adapter reads one JSON request on standard input and writes one
// JSON answer on standard output. Only a request contains the version:
//
//	check   {"version": 1}                                       {}
//	make    {"version": 1, "key", "pool", "size", "start_data"}  {"id"}
//	list    {"version": 1, "pool"}                               {"machines": [{"id", "key", "state", "started"}]}
//	log     {"version": 1, "id"}                                 {"log"}
//	delete  {"version": 1, "id"}                                 {"state"}
//
// On success the adapter exits with status 0. On a failure, a usage error among them,
// it exits with status 1 and writes the error document {"error": {"kind", "message"}}
// instead of the answer. No other exit status is allowed. The adapter writes its log
// lines to standard error. The start data, which contains the machine's run code,
// travels only on standard input.
//
// Every document is one JSON object in UTF-8. A decoder refuses a member that the
// document does not define, a member of the wrong type, a missing member, a member
// name that appears twice, and a request whose version is not 1.
package machines

import (
	"encoding/json/v2"
	"regexp"
	"time"
)

// Version is the protocol version that every request contains. An answer and the
// error document contain no version.
const Version = 1

// The bounds of version 1. The schemas in [Schemas] state the same bounds, except the
// bytes of a log, see [Schemas]. A length in characters counts Unicode code points, as
// JSON Schema counts maxLength.
const (
	// KeyPattern is the shape of a key, the dispatch id: dp_ and 16 characters of
	// 0-9 and a-z without i, l, o and u. An adapter makes at most one machine for one
	// key.
	KeyPattern = `^dp_[0-9a-hjkmnp-tv-z]{16}$`

	// PoolPattern is the shape of a pool id: np_ and 16 characters of 0-9 and a-z
	// without i, l, o and u.
	PoolPattern = `^np_[0-9a-hjkmnp-tv-z]{16}$`

	// SizePattern is the shape of a size name: 1 to 64 characters of a-z, 0-9 and -,
	// starting with a letter.
	SizePattern = `^[a-z][a-z0-9-]{0,63}$`

	// IDPattern is the shape of a machine id, the vendor's id for the machine: 1 to
	// 128 ASCII characters from ! to ~, with no space. The id "-" alone is not a
	// machine id: see [NoID].
	IDPattern = `^[!-~]{1,128}$`

	// NoID is the one value that matches [IDPattern] and is not a machine id. The
	// provisioner's journal writes it for a dispatch with no machine.
	NoID = "-"

	// StartedPattern is the shape of a machine's start time: an RFC 3339 time in UTC,
	// with an upper-case T and Z, and 0 to 9 digits of a second's fraction. The date
	// must also exist in the calendar.
	StartedPattern = `^[0-9]{4}-[0-9]{2}-[0-9]{2}T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{1,9})?Z$`

	// MaxStartData is the most bytes of start data, after base64 decoding. The start
	// data is 1 to 16384 bytes, written in the make request as standard base64 with
	// padding.
	MaxStartData = 16384

	// MaxLog is the most bytes of a log answer's text, as UTF-8: 768 KiB.
	MaxLog = 768 * 1024

	// MaxMessage is the most characters of an error document's message. A message has
	// at least 1 character.
	MaxMessage = 4096

	// MaxAnswer is the most bytes that an adapter may write on standard output for
	// check, make, list or delete, the answer or the error document: 1 MiB. [Serve]
	// reads at most this many bytes of a request.
	MaxAnswer = 1 << 20

	// MaxLogAnswer is the most bytes that an adapter may write on standard output for
	// log: 5 MiB. A log of [MaxLog] bytes written with every byte escaped as \u00XX
	// takes 4.5 MiB.
	MaxLogAnswer = 5 << 20
)

// The compiled patterns of the bounds.
var (
	keyShape       = regexp.MustCompile(KeyPattern)
	poolShape      = regexp.MustCompile(PoolPattern)
	sizeShape      = regexp.MustCompile(SizePattern)
	idShape        = regexp.MustCompile(IDPattern)
	startedShape   = regexp.MustCompile(StartedPattern)
	startDataShape = regexp.MustCompile(`^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/][AQgw]==|[A-Za-z0-9+/]{2}[AEIMQUYcgkosw048]=)?$`)
)

// Kind is the kind of an adapter's failure, in its error document.
type Kind string

// The kinds of failure.
const (
	// KindLimit means the vendor refused because of a limit, such as a quota.
	KindLimit Kind = "limit"
	// KindRetry means a retry may clear the failure.
	KindRetry Kind = "retry"
	// KindDefect means the request, the settings or the command line is wrong, so a
	// retry fails the same way. A request with another version or an unknown member,
	// and a make request with a size that the adapter does not know, fail with this
	// kind.
	KindDefect Kind = "defect"
	// KindOther is any other failure.
	KindOther Kind = "other"
)

// State is the state of a machine.
type State string

// The states of a machine. A list answer contains starting, running, stopped and
// deleting. A delete answer contains deleting and gone.
const (
	// StateStarting means the vendor is starting the machine.
	StateStarting State = "starting"
	// StateRunning means the machine runs.
	StateRunning State = "running"
	// StateStopped means the machine has stopped and still exists.
	StateStopped State = "stopped"
	// StateDeleting means the vendor is deleting the machine.
	StateDeleting State = "deleting"
	// StateGone means the machine no longer exists.
	StateGone State = "gone"
)

// CheckRequest is the request of check: the adapter shows that its rights and settings
// work, and makes no machine.
type CheckRequest struct {
	// Version is [Version].
	Version int `json:"version"`
}

// CheckAnswer is the answer of check, the empty object.
type CheckAnswer struct{}

// MakeRequest is the request of make: the adapter makes one machine of the size, in
// the pool, and delivers the start data to it. One key makes at most one machine: a
// second make with the same key returns the first machine's id and makes no new
// machine. A size that the adapter does not know fails with [KindDefect].
type MakeRequest struct {
	// Version is [Version].
	Version int `json:"version"`
	// Key is the dispatch id, see [KeyPattern].
	Key string `json:"key"`
	// Pool is the pool id, see [PoolPattern].
	Pool string `json:"pool"`
	// Size is a size name of the adapter's settings, see [SizePattern].
	Size string `json:"size"`
	// StartData is the start data, 1 to [MaxStartData] bytes. It contains the
	// machine's run code.
	StartData []byte `json:"start_data"`
}

// MakeAnswer is the answer of make.
type MakeAnswer struct {
	// ID is the vendor's id for the machine, see [IDPattern].
	ID string `json:"id"`
}

// ListRequest is the request of list: the adapter lists the machines of the pool.
type ListRequest struct {
	// Version is [Version].
	Version int `json:"version"`
	// Pool is the pool id, see [PoolPattern].
	Pool string `json:"pool"`
}

// ListAnswer is the answer of list.
type ListAnswer struct {
	// Machines are the pool's machines, none or more.
	Machines []Machine `json:"machines"`
}

// Machine is one machine of a list answer.
type Machine struct {
	// ID is the vendor's id for the machine, see [IDPattern].
	ID string `json:"id"`
	// Key is the key of the make request that made the machine, see [KeyPattern].
	Key string `json:"key"`
	// State is starting, running, stopped or deleting.
	State State `json:"state"`
	// Started is when the vendor started the machine, see [StartedPattern]. It is
	// written in UTC.
	Started time.Time `json:"started"`
}

// MarshalJSON writes m with its start time in UTC.
func (m Machine) MarshalJSON() ([]byte, error) {
	type wire struct {
		ID      string `json:"id"`
		Key     string `json:"key"`
		State   State  `json:"state"`
		Started string `json:"started"`
	}
	return json.Marshal(wire{m.ID, m.Key, m.State, m.Started.UTC().Format(time.RFC3339Nano)})
}

// LogRequest is the request of log: the adapter reads the machine's console text and
// keeps the last part, within [MaxLog] bytes.
type LogRequest struct {
	// Version is [Version].
	Version int `json:"version"`
	// ID is the machine id, see [IDPattern].
	ID string `json:"id"`
}

// LogAnswer is the answer of log.
type LogAnswer struct {
	// Log is the machine's console text, at most [MaxLog] bytes as UTF-8.
	Log string `json:"log"`
}

// DeleteRequest is the request of delete: the adapter deletes the machine. The delete
// of an unknown id returns the state gone.
type DeleteRequest struct {
	// Version is [Version].
	Version int `json:"version"`
	// ID is the machine id, see [IDPattern].
	ID string `json:"id"`
}

// DeleteAnswer is the answer of delete.
type DeleteAnswer struct {
	// State is deleting or gone.
	State State `json:"state"`
}

// Error is an adapter's failure: its kind and its message. A handler of [Serve] returns
// an *Error to choose the kind.
type Error struct {
	// Kind is the kind of failure.
	Kind Kind `json:"kind"`
	// Message is 1 to [MaxMessage] characters for the operator. It starts with the
	// fix, and it never contains the start data or a secret.
	Message string `json:"message"`
}

// Error returns the message.
func (e *Error) Error() string { return e.Message }

// ErrorAnswer is the error document that an adapter writes on standard output when it
// exits with status 1.
type ErrorAnswer struct {
	// Error is the failure.
	Error Error `json:"error"`
}
