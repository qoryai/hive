package adapterio

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/qoryai/integrations/contracts"
)

// Handler runs the commands of one adapter. Each method receives the path of the
// adapter's settings file, from --settings, and a request that passed its decoder.
// A method returns a *[Failure] to choose the kind of a failure; [Serve] writes any
// other error with the kind other and the error's text as the message. A message
// never contains the start data or a secret: [Serve] replaces a message for which
// [ContainsSecret] is true with a fixed message of the kind defect.
type Handler interface {
	// Check shows that the adapter's rights and settings work, makes no machine, and
	// returns the machines the adapter can make with its settings, its offers.
	Check(ctx context.Context, settings string, req contracts.MachinesCheck) (contracts.MachinesCheckAnswer, error)
	// Make makes one machine for the key that fits the request's machine spec, and
	// delivers startData, the request's start data decoded, to it. A second make with
	// the same key returns the first machine's id and makes no new machine. A spec that
	// no machine of the adapter's settings fits fails with contracts.FailureDefect.
	Make(ctx context.Context, settings string, req contracts.MachinesMake, startData []byte) (contracts.MachinesMakeAnswer, error)
	// List lists the machines of the pool.
	List(ctx context.Context, settings string, req contracts.MachinesList) (contracts.MachinesListAnswer, error)
	// Log reads the machine's console text and keeps the last part, as [LogAnswer]
	// does.
	Log(ctx context.Context, settings string, req contracts.MachinesLog) (contracts.MachinesLogAnswer, error)
	// Delete deletes the machine. The delete of an unknown id returns the state gone.
	Delete(ctx context.Context, settings string, req contracts.MachinesDelete) (contracts.MachinesDeleteAnswer, error)
}

// The exit statuses of an adapter, which [Serve] returns.
const (
	// ExitOK means the adapter wrote the answer.
	ExitOK = 0
	// ExitFailure means the adapter wrote the error document. A usage error, such as
	// an unknown command, a missing --settings or an extra argument, is a failure
	// with the kind defect.
	ExitFailure = 1
)

// usage is the message of a usage error.
const usage = "Run the machine adapter as <command> --settings FILE, where <command> is check, make, list, log or delete"

// Serve runs one command of an adapter: args are the program's arguments after its
// name, <command> --settings FILE. Wrong arguments, and a request that fails its
// decoder, fail with contracts.FailureDefect. It reads one request of at most
// contracts.MaxMachinesAnswer bytes from stdin, calls the handler's method for the
// command, and writes the answer, or the error document, as one line on stdout. The
// line, with its line feed, is at most contracts.MaxMachinesAnswer bytes. It returns
// the exit status: [ExitOK] or [ExitFailure]. An adapter's main can be
//
//	func main() {
//		os.Exit(adapterio.Serve(context.Background(), os.Args[1:], os.Stdin, os.Stdout, adapter{}))
//	}
func Serve(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, h Handler) int {
	if len(args) != 3 || !command(args[0]) || args[1] != "--settings" || args[2] == "" {
		fail(stdout, contracts.FailureDefect, usage)
		return ExitFailure
	}
	name, settings := args[0], args[2]
	b, err := io.ReadAll(io.LimitReader(stdin, contracts.MaxMachinesAnswer+1))
	if err != nil {
		fail(stdout, contracts.FailureRetry, "Run the command again: the machine adapter could not read its request on standard input")
		return ExitFailure
	}
	if len(b) > contracts.MaxMachinesAnswer {
		fail(stdout, contracts.FailureDefect, fmt.Sprintf("Report this defect in the provisioner: its %s request to the machine adapter is larger than %d bytes", name, contracts.MaxMachinesAnswer))
		return ExitFailure
	}
	answer, startData, err := dispatch(ctx, name, settings, b, h)
	var bad *badRequest
	var failure *Failure
	switch {
	case errors.As(err, &bad):
		fail(stdout, contracts.FailureDefect, fmt.Sprintf("Report this defect in the provisioner: its %s request to the machine adapter is not valid: %v", name, bad.err))
		return ExitFailure
	case err != nil && ContainsSecret(err.Error(), startData):
		fail(stdout, contracts.FailureDefect, fmt.Sprintf("Fix the machine adapter: its error message for %s contains what looks like a run code or the start data, so the machine adapter wrote this message in its place", name))
		return ExitFailure
	case errors.As(err, &failure):
		fail(stdout, failure.Kind, failure.Message)
		return ExitFailure
	case err != nil:
		fail(stdout, contracts.FailureOther, err.Error())
		return ExitFailure
	}
	out, err := Encode(answer)
	if err != nil {
		fail(stdout, contracts.FailureDefect, fmt.Sprintf("Fix the machine adapter: its %s answer is not valid: %v", name, err))
		return ExitFailure
	}
	if len(out)+1 > contracts.MaxMachinesAnswer {
		fail(stdout, contracts.FailureDefect, fmt.Sprintf("Fix the machine adapter: its %s answer is larger than %d bytes", name, contracts.MaxMachinesAnswer))
		return ExitFailure
	}
	if _, err := stdout.Write(append(out, '\n')); err != nil {
		return ExitFailure
	}
	return ExitOK
}

// badRequest is a request that fails its decoder.
type badRequest struct{ err error }

func (e *badRequest) Error() string { return e.err.Error() }

// command is whether name is a command of version 1.
func command(name string) bool {
	switch name {
	case "check", "make", "list", "log", "delete":
		return true
	}
	return false
}

// dispatch decodes the request b of the command name and calls the handler. It also
// returns the start data of a make request.
func dispatch(ctx context.Context, name, settings string, b []byte, h Handler) (any, []byte, error) {
	switch name {
	case "check":
		req, err := DecodeCheck(b)
		if err != nil {
			return nil, nil, &badRequest{err}
		}
		a, err := h.Check(ctx, settings, req)
		return a, nil, err
	case "make":
		req, data, err := DecodeMake(b)
		if err != nil {
			return nil, nil, &badRequest{err}
		}
		a, err := h.Make(ctx, settings, req, data)
		return a, data, err
	case "list":
		req, err := DecodeList(b)
		if err != nil {
			return nil, nil, &badRequest{err}
		}
		a, err := h.List(ctx, settings, req)
		return a, nil, err
	case "log":
		req, err := DecodeLog(b)
		if err != nil {
			return nil, nil, &badRequest{err}
		}
		a, err := h.Log(ctx, settings, req)
		return a, nil, err
	default:
		req, err := DecodeDelete(b)
		if err != nil {
			return nil, nil, &badRequest{err}
		}
		a, err := h.Delete(ctx, settings, req)
		return a, nil, err
	}
}

// shortMessage is message as an error document contains it: valid UTF-8, each U+0000
// replaced by U+FFFD, and cut to its first [MaxMessage] characters. An empty message
// is replaced.
func shortMessage(message string) string {
	message = strings.ToValidUTF8(message, string(utf8.RuneError))
	message = strings.ReplaceAll(message, "\x00", string(utf8.RuneError))
	if utf8.RuneCountInString(message) > MaxMessage {
		message = string([]rune(message)[:MaxMessage])
	}
	if message == "" {
		message = "Fix the machine adapter: it failed with an empty message"
	}
	return message
}

// fail writes the error document with kind and message as one line on w. A kind that
// is not of version 1 becomes other, and the message is written as shortMessage gives it.
func fail(w io.Writer, kind, message string) {
	switch kind {
	case contracts.FailureLimit, contracts.FailureRetry, contracts.FailureDefect, contracts.FailureOther:
	default:
		kind = contracts.FailureOther
	}
	out, err := Encode(contracts.MachinesError{Error: contracts.MachinesFailure{Kind: kind, Message: shortMessage(message)}})
	if err != nil {
		return
	}
	_, _ = w.Write(append(out, '\n'))
}

// LogAnswer is the log answer of the console text: valid UTF-8, each U+0000 replaced
// by U+FFFD, and the longest end of the text that fits two bounds. The text is at most
// [MaxLog] bytes, and the answer as [Serve] writes it, with its line feed, is at most
// contracts.MaxMachinesAnswer bytes. Each cut falls at the start of a character.
func LogAnswer(text string) contracts.MachinesLogAnswer {
	text = strings.ToValidUTF8(text, string(utf8.RuneError))
	text = strings.ReplaceAll(text, "\x00", string(utf8.RuneError))
	text = lastPart(text, MaxLog)
	// fits is whether the answer with the text from byte i on fits; it is false for
	// every i below the first that fits, and true from there on.
	fits := func(i int) bool {
		out, err := json.Marshal(contracts.MachinesLogAnswer{Log: lastPart(text, len(text)-i)})
		return err == nil && len(out)+1 <= contracts.MaxMachinesAnswer
	}
	i := sort.Search(len(text), fits)
	return contracts.MachinesLogAnswer{Log: lastPart(text, len(text)-i)}
}

// lastPart returns the last bytes of text, valid UTF-8, at most limit, cut at the start
// of a character.
func lastPart(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	text = text[len(text)-limit:]
	for len(text) > 0 && !utf8.RuneStart(text[0]) {
		text = text[1:]
	}
	return text
}
