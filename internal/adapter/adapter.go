// Package adapter runs the commands of a machine adapter, the separate program that
// makes, lists, reads and deletes machines at one vendor, as machines/v1 of the
// integrations contracts states them. Package adapterio checks each request and answer.
//
// The runner starts the adapter as <command> <name> --settings <file> in a new process
// group, writes the request on its standard input and reads at most
// contracts.MaxMachinesAnswer bytes, 1 MiB, of its standard output for every command.
// The adapter's standard error goes to the runner's stderr writer line by line: a line
// that contains what looks like a run code or a piece of the start data is replaced by
// a fixed text, and a line is cut at 4096 bytes. The adapter's environment is the
// provisioner's environment without any variable whose name starts with QORY_. The
// start data travels only on standard input, and the runner clears its copy of the
// request after the adapter ends. The runner's errors never contain the adapter's
// standard output or standard error, except the message of a valid error document.
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
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/integrations/contracts"
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
// when the adapter exits with status 1 and writes a valid one. Any other failure of
// the adapter has the kind defect and a message that starts with the fix and states
// the exit status or the rule that failed. Two failures return a plain error, not an
// *Error: a context that ends before the adapter finishes, which wraps the context's
// error, and a request that fails a rule of version 1, for which the runner does not
// start the adapter.
type Error struct {
	// Command is the command: check, make, list, log or delete.
	Command string
	// Kind is the kind of failure.
	Kind string
	// Message is the failure for the operator.
	Message string
}

// Error returns the message, then the command and the kind, on one line: each control
// character, line separator and paragraph separator of the message becomes a space.
func (e *Error) Error() string {
	return fmt.Sprintf("%s (machine adapter command %s, kind %s)", oneLine(e.Message), e.Command, e.Kind)
}

// oneLine is s with each control character, U+2028 and U+2029 replaced by a space.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, s)
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
// The adapter's standard error goes to stderr line by line, filtered as the package
// documentation states.
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
	_, err := run(ctx, r, "check", contracts.MachinesCheck{Version: adapterio.Version}, nil, adapterio.DecodeCheckAnswer)
	return err
}

// Make runs make: the adapter makes one machine of size in pool for key, the dispatch
// id, and delivers startData to it. startData contains the machine's run code, and it
// travels only on the adapter's standard input.
func (r *Runner) Make(ctx context.Context, key, pool, size string, startData []byte) (contracts.MachinesMakeAnswer, error) {
	req := contracts.MachinesMake{Version: adapterio.Version, Key: key, Pool: pool, Size: size, StartData: base64.StdEncoding.EncodeToString(startData)}
	return run(ctx, r, "make", req, startData, adapterio.DecodeMakeAnswer)
}

// List runs list: the adapter lists the machines of pool.
func (r *Runner) List(ctx context.Context, pool string) (contracts.MachinesListAnswer, error) {
	return run(ctx, r, "list", contracts.MachinesList{Version: adapterio.Version, Pool: pool}, nil, adapterio.DecodeListAnswer)
}

// Log runs log: the adapter reads the console text of the machine id.
func (r *Runner) Log(ctx context.Context, id string) (contracts.MachinesLogAnswer, error) {
	return run(ctx, r, "log", contracts.MachinesLog{Version: adapterio.Version, ID: id}, nil, adapterio.DecodeLogAnswer)
}

// Delete runs delete: the adapter deletes the machine id.
func (r *Runner) Delete(ctx context.Context, id string) (contracts.MachinesDeleteAnswer, error) {
	return run(ctx, r, "delete", contracts.MachinesDelete{Version: adapterio.Version, ID: id}, nil, adapterio.DecodeDeleteAnswer)
}

// run runs the command name with req on standard input and decodes the answer.
// startData is the start data of a make request, which an error message must not
// contain.
func run[A any](ctx context.Context, r *Runner, name string, req any, startData []byte, decode func([]byte) (A, error)) (A, error) {
	var zero A
	body, err := adapterio.Encode(req)
	if err != nil {
		return zero, fmt.Errorf("Report this defect in the provisioner: its %s request to the machine adapter is not valid: %w", name, err)
	}
	// The body of a make request contains the start data in base64.
	defer clear(body)
	limit := r.timeout(name)
	cmdCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	out := &capped{limit: contracts.MaxMachinesAnswer, cancel: cancel}
	cmd := exec.CommandContext(cmdCtx, r.command, name, "--settings", r.settings)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Stdout = out
	if r.stderr != nil {
		stderr := &stderrFilter{w: r.stderr, startData: startData}
		defer stderr.close()
		cmd.Stderr = stderr
	}
	cmd.Env = environment()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// killed records a kill of the process group that reached at least one process,
	// so that an error claims the kill only when it happened.
	var killed atomic.Bool
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		if err == nil {
			killed.Store(true)
		}
		return err
	}
	cmd.WaitDelay = r.waitDelay

	err = cmd.Run()
	if cmd.Process != nil {
		// A process that the adapter started may outlive it. ESRCH means none is left,
		// and no other error leaves anything to do.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	defect := func(format string, args ...any) error {
		return &Error{Command: name, Kind: contracts.FailureDefect, Message: fmt.Sprintf(format, args...)}
	}
	ps := cmd.ProcessState
	exited := ps != nil && ps.Exited()
	switch {
	case cmd.Process == nil && ctx.Err() != nil:
		return zero, fmt.Errorf("the provisioner cancelled the machine adapter's %s before it started: %w", name, ctx.Err())
	case cmd.Process == nil:
		return zero, defect("Fix adapter.command in the provisioner's settings: the machine adapter did not start: %v", err)
	case out.over && killed.Load():
		return zero, defect("Fix the machine adapter: it wrote more than %d bytes on standard output for %s, so the provisioner killed its process group", out.limit, name)
	case out.over:
		return zero, defect("Fix the machine adapter: it wrote more than %d bytes on standard output for %s", out.limit, name)
	case exited && ps.ExitCode() == 0:
		// The adapter exited with status 0 before any kill, so a valid answer counts
		// even when the context ended or a child held the output open after the exit:
		// a made machine's id must not be lost.
		answer, decodeErr := decode(out.Bytes())
		switch {
		case decodeErr == nil:
			return answer, nil
		case errors.Is(err, exec.ErrWaitDelay):
			return zero, defect("Fix the machine adapter: a process that it started kept its standard output or standard error open after %s exited", name)
		}
		return zero, defect("Fix the machine adapter: its answer to %s fails a rule of version 1: %v", name, decodeErr)
	case exited:
		return zero, failure(name, ps.ExitCode(), out.Bytes(), startData)
	case killed.Load() && ctx.Err() != nil:
		return zero, fmt.Errorf("the provisioner stopped the machine adapter's %s and killed its process group: %w", name, ctx.Err())
	case killed.Load():
		return zero, defect("Fix the machine adapter: %s ran longer than its time limit of %s, so the provisioner killed its process group", name, limit)
	case ps != nil:
		if status, ok := ps.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return zero, defect("Fix the machine adapter: it ended on the signal %s during %s", status.Signal(), name)
		}
		return zero, defect("Fix the machine adapter: it ended during %s with neither an exit status nor a signal", name)
	}
	return zero, defect("Fix the machine adapter: its output for %s could not be read: %v", name, err)
}

// failure is the error of an adapter that exited with the status code, after it
// wrote stdout.
func failure(name string, code int, stdout, startData []byte) error {
	defect := func(format string, args ...any) error {
		return &Error{Command: name, Kind: contracts.FailureDefect, Message: fmt.Sprintf(format, args...)}
	}
	if code != adapterio.ExitFailure {
		return defect("Fix the machine adapter: it exited with status %d for %s; only status 0 and status 1 are allowed", code, name)
	}
	doc, err := adapterio.DecodeError(stdout)
	if err != nil {
		return defect("Fix the machine adapter: it exited with status 1 for %s, and its standard output fails a rule of the error document: %v", name, err)
	}
	if adapterio.ContainsSecret(doc.Error.Message, startData) {
		return defect("Fix the machine adapter: its error message for %s contains what looks like a run code or the start data, so the provisioner does not show it", name)
	}
	return &Error{Command: name, Kind: doc.Error.Kind, Message: doc.Error.Message}
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

// maxStderrLine is the most bytes of one line of the adapter's standard error that
// the runner passes on.
const maxStderrLine = 4096

// The texts that [stderrFilter] writes in place of a line or after a cut line.
const (
	hiddenStderrLine = "[hidden: the machine adapter's line contained what looks like a run code or the start data]"
	cutStderrLine    = " [cut: the machine adapter's line is longer than 4096 bytes]"
)

// stderrFilter passes the adapter's standard error to w one line at a time. It keeps
// the first [maxStderrLine] bytes of a line and drops the rest. When the bytes kept
// contain what looks like a run code or a piece of startData, as
// [adapterio.ContainsSecret] finds it, it writes [hiddenStderrLine] in place of the
// line. Each line goes out with a line feed, also the last one, which close writes.
// It drops the errors of w, so that exec keeps reading the adapter's standard error.
// The mutex orders close with a write that still runs after cmd.Wait returned
// [exec.ErrWaitDelay].
type stderrFilter struct {
	mu        sync.Mutex
	w         io.Writer
	startData []byte
	line      []byte
	cut       bool
	closed    bool
}

func (f *stderrFilter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(p)
	if f.closed {
		return n, nil
	}
	for {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			f.keep(p)
			return n, nil
		}
		f.keep(p[:i])
		f.flush()
		p = p[i+1:]
	}
}

// keep adds b to the line, up to [maxStderrLine] bytes.
func (f *stderrFilter) keep(b []byte) {
	if room := maxStderrLine - len(f.line); len(b) > room {
		b, f.cut = b[:room], true
	}
	f.line = append(f.line, b...)
}

// flush writes the line kept, filtered, with a line feed, and starts a new line.
func (f *stderrFilter) flush() {
	text := string(f.line)
	switch {
	case adapterio.ContainsSecret(text, f.startData):
		text = hiddenStderrLine
	case f.cut:
		text += cutStderrLine
	}
	_, _ = io.WriteString(f.w, text+"\n")
	clear(f.line)
	f.line, f.cut = f.line[:0], false
}

// close writes the last line when it has no line feed, and drops every later write.
func (f *stderrFilter) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.line) > 0 || f.cut {
		f.flush()
	}
	f.closed = true
}

// capped keeps at most limit bytes. On the first write that would cross the bound it
// marks itself over and calls cancel, which kills the adapter's process group. It
// discards that whole write and every later one. It has no ReadFrom method, so
// io.Copy goes through Write.
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
