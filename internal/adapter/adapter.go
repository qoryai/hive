// Package adapter runs the commands of a machine adapter, the separate program that
// makes, lists, reads and deletes machines at one vendor, as version 1 of the protocol
// in package machines states them.
//
// The runner starts the adapter as <command> <name> --settings <file> in a new process
// group, writes the request on its standard input and reads at most
// [machines.MaxOutput] bytes of its standard output: 5 MiB for log, 1 MiB for any other
// command. The adapter's standard error goes
// to the runner's stderr writer unchanged. The adapter's environment is the
// provisioner's environment without any variable whose name starts with QORY_. The
// start data travels only on standard input. The runner's errors never contain the
// adapter's standard output or standard error, except the message of a valid error
// document.
package adapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/qoryai/hive/machines"
)

// The time limits of the commands. When a command runs longer, the runner kills the
// adapter's process group.
const (
	// MakeTimeout is the time limit of make.
	MakeTimeout = 2 * time.Minute
	// CommandTimeout is the time limit of check, list, log and delete.
	CommandTimeout = time.Minute
)

// waitDelay bounds the wait for the adapter's output pipes to close after the adapter
// exits or is killed, when a process that it started keeps them open.
const waitDelay = 5 * time.Second

// Error is a failed command. Kind and Message come from the adapter's error document
// when the adapter exits with status 1 and writes a valid one. Any other failure has
// the kind defect and a message that starts with the fix and states the exit status or
// the rule that failed.
type Error struct {
	// Command is the command: check, make, list, log or delete.
	Command string
	// Kind is the kind of failure.
	Kind machines.Kind
	// Message is the failure for the operator.
	Message string
}

// Error returns the message, then the command and the kind.
func (e *Error) Error() string {
	return fmt.Sprintf("%s (machine adapter command %s, kind %s)", e.Message, e.Command, e.Kind)
}

// Runner runs the commands of one machine adapter.
type Runner struct {
	command  string
	settings string
	stderr   io.Writer
	// timeout is the time limit of a command, and waitDelay the wait for the output
	// pipes; tests shorten them.
	timeout   func(name string) time.Duration
	waitDelay time.Duration
}

// New returns a runner of the adapter program command with its settings file
// settings, from adapter.command and adapter.settings of the provisioner's settings.
// The adapter's standard error goes to stderr unchanged.
func New(command, settings string, stderr io.Writer) *Runner {
	return &Runner{command: command, settings: settings, stderr: stderr, timeout: timeout, waitDelay: waitDelay}
}

// timeout is the time limit of the command name.
func timeout(name string) time.Duration {
	if name == "make" {
		return MakeTimeout
	}
	return CommandTimeout
}

// Check runs check: the adapter shows that its rights and settings work, and makes no
// machine.
func (r *Runner) Check(ctx context.Context) error {
	_, err := run(ctx, r, "check", machines.CheckRequest{Version: machines.Version}, nil, machines.DecodeCheckAnswer)
	return err
}

// Make runs make: the adapter makes one machine of size in pool for key, the dispatch
// id, and delivers startData to it. startData contains the machine's run code, and it
// travels only on the adapter's standard input.
func (r *Runner) Make(ctx context.Context, key, pool, size string, startData []byte) (machines.MakeAnswer, error) {
	req := machines.MakeRequest{Version: machines.Version, Key: key, Pool: pool, Size: size, StartData: startData}
	return run(ctx, r, "make", req, startData, machines.DecodeMakeAnswer)
}

// List runs list: the adapter lists the machines of pool.
func (r *Runner) List(ctx context.Context, pool string) (machines.ListAnswer, error) {
	return run(ctx, r, "list", machines.ListRequest{Version: machines.Version, Pool: pool}, nil, machines.DecodeListAnswer)
}

// Log runs log: the adapter reads the console text of the machine id.
func (r *Runner) Log(ctx context.Context, id string) (machines.LogAnswer, error) {
	return run(ctx, r, "log", machines.LogRequest{Version: machines.Version, ID: id}, nil, machines.DecodeLogAnswer)
}

// Delete runs delete: the adapter deletes the machine id.
func (r *Runner) Delete(ctx context.Context, id string) (machines.DeleteAnswer, error) {
	return run(ctx, r, "delete", machines.DeleteRequest{Version: machines.Version, ID: id}, nil, machines.DecodeDeleteAnswer)
}

// run runs the command name with req on standard input and decodes the answer.
// startData is the start data of a make request, which an error message must not
// contain.
func run[A any](ctx context.Context, r *Runner, name string, req machines.Document, startData []byte, decode func([]byte) (A, error)) (A, error) {
	var zero A
	body, err := machines.Encode(req)
	if err != nil {
		return zero, fmt.Errorf("Report this defect in the provisioner: its %s request to the machine adapter is not valid: %w", name, err)
	}
	limit := r.timeout(name)
	cmdCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	out := &capped{limit: machines.MaxOutput(name), cancel: cancel}
	cmd := exec.CommandContext(cmdCtx, r.command, name, "--settings", r.settings)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Stdout = out
	cmd.Stderr = r.stderr
	cmd.Env = environment()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = r.waitDelay

	err = cmd.Run()
	defect := func(format string, args ...any) error {
		return &Error{Command: name, Kind: machines.KindDefect, Message: fmt.Sprintf(format, args...)}
	}
	var exit *exec.ExitError
	switch {
	case out.over:
		return zero, defect("Fix the machine adapter: it wrote more than %d bytes on standard output for %s, so the provisioner killed its process group", out.limit, name)
	case ctx.Err() != nil && cmd.Process == nil:
		return zero, fmt.Errorf("the provisioner cancelled the machine adapter's %s before it started: %w", name, ctx.Err())
	case ctx.Err() != nil:
		return zero, fmt.Errorf("the provisioner stopped the machine adapter's %s and killed its process group: %w", name, ctx.Err())
	case cmd.Process == nil:
		return zero, defect("Fix adapter.command in the provisioner's settings: the machine adapter did not start: %v", err)
	case errors.Is(cmdCtx.Err(), context.DeadlineExceeded):
		return zero, defect("Fix the machine adapter: %s ran longer than its time limit of %s, so the provisioner killed its process group", name, limit)
	case errors.Is(err, exec.ErrWaitDelay):
		return zero, defect("Fix the machine adapter: a process that it started kept its standard output or standard error open after %s exited", name)
	case errors.As(err, &exit):
		return zero, failure(name, exit, out.Bytes(), startData)
	case err != nil:
		return zero, defect("Fix the machine adapter: its output for %s could not be read: %v", name, err)
	}
	answer, err := decode(out.Bytes())
	if err != nil {
		return zero, defect("Fix the machine adapter: its answer to %s fails a rule of version 1: %v", name, err)
	}
	return answer, nil
}

// failure is the error of an adapter that exited with exit, after it wrote stdout.
func failure(name string, exit *exec.ExitError, stdout, startData []byte) error {
	defect := func(format string, args ...any) error {
		return &Error{Command: name, Kind: machines.KindDefect, Message: fmt.Sprintf(format, args...)}
	}
	if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return defect("Fix the machine adapter: it ended on the signal %s during %s", status.Signal(), name)
	}
	if code := exit.ExitCode(); code != machines.ExitFailure {
		return defect("Fix the machine adapter: it exited with status %d for %s; only status 0 and status 1 are allowed", code, name)
	}
	doc, err := machines.DecodeErrorAnswer(stdout)
	if err != nil {
		return defect("Fix the machine adapter: it exited with status 1 for %s, and its standard output fails a rule of the error document: %v", name, err)
	}
	if secret(doc.Error.Message, startData) {
		return defect("Fix the machine adapter: its error message for %s contains what looks like a run code or the start data, so the provisioner does not show it", name)
	}
	return &Error{Command: name, Kind: doc.Error.Kind, Message: doc.Error.Message}
}

// secret is whether message contains what looks like a run code, qecr_ in any case, or
// the start data, raw or as base64.
func secret(message string, startData []byte) bool {
	if strings.Contains(strings.ToLower(message), "qecr_") {
		return true
	}
	if len(startData) == 0 {
		return false
	}
	return strings.Contains(message, string(startData)) || strings.Contains(message, base64.StdEncoding.EncodeToString(startData))
}

// environment is the provisioner's environment without any variable whose name
// starts with QORY_, such as the secret QORY_ENROLMENT_CODE.
func environment() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "QORY_") {
			out = append(out, kv)
		}
	}
	return out
}

// capped keeps at most limit bytes. On the first byte over the bound it marks itself
// over and calls cancel, which kills the adapter's process group; it discards that
// byte and every later one. It has no ReadFrom method, so io.Copy goes through Write.
type capped struct {
	buf    bytes.Buffer
	limit  int
	over   bool
	cancel context.CancelFunc
}

func (c *capped) Write(p []byte) (int, error) {
	if c.over {
		return len(p), nil
	}
	if c.buf.Len()+len(p) > c.limit {
		c.over = true
		c.cancel()
		return len(p), nil
	}
	return c.buf.Write(p)
}

// Bytes returns the bytes kept.
func (c *capped) Bytes() []byte { return c.buf.Bytes() }
