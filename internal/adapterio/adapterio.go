// Package adapterio reads and writes the documents of the machine adapter protocol,
// machines/v1 of github.com/qoryai/integrations/contracts, for the provisioner and for
// a machine adapter written in Go. It also has [Serve], which runs one command of such
// an adapter, and [ContainsSecret], which finds a run code or a piece of the start data
// in a text.
//
// A machine adapter is a separate program that makes, lists, reads and deletes
// machines at one vendor. The provisioner runs it as
//
//	<adapter> <command> --settings FILE
//
// where <command> is check, make, list, log or delete, and FILE is the adapter's
// settings file. The adapter reads one JSON request on standard input and writes one
// JSON answer, or the error document, on standard output. The types of the documents
// are those of package contracts, and the schemas in contracts.MachinesFS state their
// rules.
//
// A decoder of this package checks a document against its schema, compiled with
// contracts.CompileMachines, and refuses four more things that the schemas accept: a
// member name that appears twice in one object, start data of 0 bytes, start data in
// base64 whose padding bits are not zero, and a log of more than [MaxLog] bytes as
// UTF-8. [Encode] checks the same rules before it writes a document. No error of this
// package contains a value of the document, so an error never shows the start data or
// a secret.
package adapterio

import (
	"time"

	"github.com/qoryai/integrations/contracts"
)

// Version is the version that every request of machines/v1 contains, the const of the
// member version in its schemas. An answer and the error document contain no version.
const Version = 1

// NoID is the one value that the id pattern of machines/v1 matches and that is not a
// machine id: the schemas refuse it as an id. The provisioner's journal writes it for
// a dispatch with no machine.
const NoID = "-"

// MaxLog is the most bytes of a log answer's text, as UTF-8: 768 KiB. The schema
// log-answer.schema.json states the bound in characters; the decoders also refuse a
// text of more bytes.
const MaxLog = 768 << 10

// MaxMessage is the most characters of an error document's message, the maxLength of
// message in error.schema.json. [Serve] cuts a longer message to this length.
const MaxMessage = 200

// Failure is an adapter's failure: its kind, one of contracts.FailureLimit,
// contracts.FailureRetry, contracts.FailureDefect and contracts.FailureOther, and its
// message. A handler of [Serve] returns a *Failure to choose the kind.
type Failure struct {
	// Kind is the kind of failure.
	Kind string
	// Message is the failure for the operator. It starts with the fix, and it never
	// contains the start data or a secret.
	Message string
}

// Error returns the message.
func (e *Failure) Error() string { return e.Message }

// Started is the start time of the machine m of a list answer that a decoder of this
// package returned. It is the zero time when m.Started is not an RFC 3339 time.
func Started(m contracts.Machine) time.Time {
	t, err := time.Parse(time.RFC3339, m.Started)
	if err != nil {
		return time.Time{}
	}
	return t
}
