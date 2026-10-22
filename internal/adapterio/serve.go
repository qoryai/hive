package machines

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Handler runs the commands of one adapter. Each method receives the path of the
// adapter's settings file, from --settings, and a request that passed its decoder.
// A method returns an *[Error] to choose the kind of a failure; [Serve] writes any
// other error with the kind other and the error's text as the message. A message
// never contains the start data or a secret: [Serve] replaces a message for which
// [ContainsSecret] is true with a fixed message of the kind defect.
type Handler interface {
	// Check shows that the adapter's rights and settings work, and makes no machine.
	Check(ctx context.Context, settings string, req CheckRequest) (CheckAnswer, error)
	// Make makes one machine for the key. A second make with the same key returns the
	// first machine's id and makes no new machine. A size that the adapter does not
	// know fails with [KindDefect].
	Make(ctx context.Context, settings string, req MakeRequest) (MakeAnswer, error)
	// List lists the machines of the pool.
	List(ctx context.Context, settings string, req ListRequest) (ListAnswer, error)
	// Log reads the machine's console text and keeps the last part, within [MaxLog]
	// bytes.
	Log(ctx context.Context, settings string, req LogRequest) (LogAnswer, error)
	// Delete deletes the machine. The delete of an unknown id returns the state gone.
	Delete(ctx context.Context, settings string, req DeleteRequest) (DeleteAnswer, error)
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

// MaxOutput is the most bytes that an adapter may write on standard output for the
// command name: [MaxLogAnswer] for log, and [MaxAnswer] for any other command.
func MaxOutput(name string) int {
	if name == "log" {
		return MaxLogAnswer
	}
	return MaxAnswer
}

// Serve runs one command of an adapter: args are the program's arguments after its
// name, <command> --settings FILE. Wrong arguments, and a request with another version
// or an unknown member, fail with [KindDefect]. It reads one request of at most [MaxAnswer] bytes
// from stdin, calls the handler's method for the command, and writes the answer, or
// the error document, as one line on stdout. It returns the exit status: [ExitOK] or
// [ExitFailure]. An adapter's main can be
//
//	func main() {
//		os.Exit(machines.Serve(context.Background(), os.Args[1:], os.Stdin, os.Stdout, adapter{}))
//	}
func Serve(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, h Handler) int {
	if len(args) != 3 || !command(args[0]) || args[1] != "--settings" || args[2] == "" {
		fail(stdout, KindDefect, usage)
		return ExitFailure
	}
	name, settings := args[0], args[2]
	b, err := io.ReadAll(io.LimitReader(stdin, MaxAnswer+1))
	if err != nil {
		fail(stdout, KindRetry, "Run the command again: the machine adapter could not read its request on standard input")
		return ExitFailure
	}
	if len(b) > MaxAnswer {
		fail(stdout, KindDefect, fmt.Sprintf("Report this defect in the provisioner: its %s request to the machine adapter is larger than %d bytes", name, MaxAnswer))
		return ExitFailure
	}
	answer, startData, err := dispatch(ctx, name, settings, b, h)
	var bad *badRequest
	var failure *Error
	switch {
	case errors.As(err, &bad):
		fail(stdout, KindDefect, fmt.Sprintf("Report this defect in the provisioner: its %s request to the machine adapter is not valid: %v", name, bad.err))
		return ExitFailure
	case err != nil && ContainsSecret(err.Error(), startData):
		fail(stdout, KindDefect, fmt.Sprintf("Fix the machine adapter: its error message for %s contains what looks like a run code or the start data, so the machine adapter wrote this message in its place", name))
		return ExitFailure
	case errors.As(err, &failure):
		fail(stdout, failure.Kind, failure.Message)
		return ExitFailure
	case err != nil:
		fail(stdout, KindOther, err.Error())
		return ExitFailure
	}
	out, err := Encode(answer)
	if err != nil {
		fail(stdout, KindDefect, fmt.Sprintf("Fix the machine adapter: its %s answer is not valid: %v", name, err))
		return ExitFailure
	}
	if len(out)+1 > MaxOutput(name) {
		fail(stdout, KindDefect, fmt.Sprintf("Fix the machine adapter: its %s answer is larger than %d bytes", name, MaxOutput(name)))
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
func dispatch(ctx context.Context, name, settings string, b []byte, h Handler) (Document, []byte, error) {
	switch name {
	case "check":
		req, err := DecodeCheckRequest(b)
		if err != nil {
			return nil, nil, &badRequest{err}
		}
		a, err := h.Check(ctx, settings, req)
		return a, nil, err
	case "make":
		req, err := DecodeMakeRequest(b)
		if err != nil {
			return nil, nil, &badRequest{err}
		}
		a, err := h.Make(ctx, settings, req)
		return a, req.StartData, err
	case "list":
		req, err := DecodeListRequest(b)
		if err != nil {
			return nil, nil, &badRequest{err}
		}
		a, err := h.List(ctx, settings, req)
		return a, nil, err
	case "log":
		req, err := DecodeLogRequest(b)
		if err != nil {
			return nil, nil, &badRequest{err}
		}
		a, err := h.Log(ctx, settings, req)
		return a, nil, err
	default:
		req, err := DecodeDeleteRequest(b)
		if err != nil {
			return nil, nil, &badRequest{err}
		}
		a, err := h.Delete(ctx, settings, req)
		return a, nil, err
	}
}

// fail writes the error document with kind and message as one line on w. A kind that
// is not of version 1 becomes other. A message is made valid UTF-8 and cut to
// [MaxMessage] characters, and an empty message is replaced.
func fail(w io.Writer, kind Kind, message string) {
	if checkKind(kind) != nil {
		kind = KindOther
	}
	message = strings.ToValidUTF8(message, string(utf8.RuneError))
	if utf8.RuneCountInString(message) > MaxMessage {
		message = string([]rune(message)[:MaxMessage])
	}
	if message == "" {
		message = "Fix the machine adapter: it failed with an empty message"
	}
	out, err := Encode(ErrorAnswer{Error: Error{Kind: kind, Message: message}})
	if err != nil {
		return
	}
	_, _ = w.Write(append(out, '\n'))
}
