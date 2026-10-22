package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// usageText is the program's usage, word for word.
const usageText = `usage:
  qory-provisioner <command> [flags]

commands:
  serve   Run the provisioner until SIGTERM

qory-provisioner <command> -h prints a command's help.
`

// serveUsageText is serve's usage and flags, word for word.
const serveUsageText = `usage:
  qory-provisioner serve [--settings FILE] [--state-dir DIR]

flags:
  --settings FILE
    	the provisioner's settings FILE (default "/etc/qory-provisioner/settings.json")
  --state-dir DIR
    	the state folder DIR: the access key, the enrolment and the journal of live dispatches (default "/var/lib/qory-provisioner")
`

// serveHelpText is serve's help, word for word.
const serveHelpText = `serve claims dispatches from Qory Apiary, makes one machine for each through the
machine adapter, and deletes each machine once its dispatch ends. It writes its log on
standard error.

On the first start, while the state folder has no access key, serve enrols one with
the node pool's enrolment code (qecp_…) from QORY_ENROLMENT_CODE.

Run one provisioner per node pool. Every 60 seconds, serve lists the node pool's
machines and deletes each machine whose dispatch has no entry in the journal, with the
log line "found a machine with no journal entry". Two provisioners of one node pool
therefore delete each other's machines, also machines that run a dispatch.

When you rotate the key, which means a new provisioner with a new access key for the
node pool, stop the old provisioner before you start the new one. A new provisioner
with a new state folder starts with an empty journal, so it deletes the machines that
the old one made, also those that run a dispatch.

serve runs until SIGTERM or SIGINT. It then stops claiming, leaves every machine as it
is, and exits with status 0. A second SIGTERM or SIGINT ends serve at once. serve exits
with status 1 when a failure stops it, also one while it stops, and with status 2 on a
usage error.

` + serveUsageText + `
examples:
  QORY_ENROLMENT_CODE=qecp_… qory-provisioner serve
  qory-provisioner serve --settings /etc/qory-provisioner/settings.json --state-dir /var/lib/qory-provisioner
`

// buffer is a bytes.Buffer safe for concurrent use: the log and the adapter's standard
// error write to it from several goroutines.
type buffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// noEnv reads an environment with no variable set.
func noEnv(string) string { return "" }

// runArgs runs the program with args, with no environment variable and no signal, and
// returns its exit status and what it wrote on standard error.
func runArgs(args ...string) (int, string) {
	var errs buffer
	code := run(args, env{stderr: &errs, getenv: noEnv})
	return code, errs.String()
}

// TestUsageIsPinned pins the usage, word for word: with no command, with -h or --help
// alone, and with a command the program does not know. Each exits with status 2.
func TestUsageIsPinned(t *testing.T) {
	for _, args := range [][]string{nil, {"-h"}, {"--help"}, {"-help"}, {"help"}, {"start"}, {"-h", "serve"}} {
		code, errs := runArgs(args...)
		if code != 2 || errs != usageText {
			t.Errorf("%q: exit %d, stderr\n%s\nwant\n%s", args, code, errs, usageText)
		}
	}
}

// TestServeHelpIsPinned pins serve's help, word for word, with exit status 2.
func TestServeHelpIsPinned(t *testing.T) {
	for _, args := range [][]string{{"serve", "-h"}, {"serve", "--help"}} {
		code, errs := runArgs(args...)
		if code != 2 || errs != serveHelpText {
			t.Errorf("%q: exit %d, stderr\n%s\nwant\n%s", args, code, errs, serveHelpText)
		}
	}
}

// TestServeUnknownFlag pins that a flag serve does not know prints the flag parser's
// message and serve's usage, with exit status 2.
func TestServeUnknownFlag(t *testing.T) {
	want := "flag provided but not defined: --bogus\n" + serveUsageText
	code, errs := runArgs("serve", "--bogus")
	if code != 2 || errs != want {
		t.Errorf("exit %d, stderr %q, want %q", code, errs, want)
	}
}

// TestServeArgument pins that an argument after the flags prints one line and serve's
// usage, with exit status 2, and that the line does not quote the argument.
func TestServeArgument(t *testing.T) {
	want := "Remove the arguments after the flags: serve takes no arguments.\n" + serveUsageText
	for _, args := range [][]string{{"serve", "extra"}, {"serve", "--settings", "/tmp/s.json", "qecp_MARKER"}} {
		code, errs := runArgs(args...)
		if code != 2 || errs != want {
			t.Errorf("%q: exit %d, stderr %q, want %q", args, code, errs, want)
		}
	}
}

// TestParseServe checks the flags' defaults, and each flag in both forms.
func TestParseServe(t *testing.T) {
	for _, c := range []struct {
		args []string
		want serveFlags
	}{
		{nil, serveFlags{settings: "/etc/qory-provisioner/settings.json", stateDir: "/var/lib/qory-provisioner"}},
		{[]string{"--settings", "/srv/p/settings.json"}, serveFlags{settings: "/srv/p/settings.json", stateDir: "/var/lib/qory-provisioner"}},
		{[]string{"--state-dir=/srv/p/state"}, serveFlags{settings: "/etc/qory-provisioner/settings.json", stateDir: "/srv/p/state"}},
		{[]string{"-settings=/a.json", "-state-dir", "/b"}, serveFlags{settings: "/a.json", stateDir: "/b"}},
	} {
		var errs bytes.Buffer
		got, ok := parseServe(c.args, &errs)
		if !ok || got != c.want || errs.Len() != 0 {
			t.Errorf("%q: %+v, %v, stderr %q; want %+v", c.args, got, ok, errs.String(), c.want)
		}
	}
}

// writeFile writes s to the file name in dir, 0600, and returns its path.
func writeFile(t *testing.T, dir, name, s string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestServeSettingsErrors checks that a settings file serve cannot use stops it with
// exit status 1 and one error line that starts with the fix.
func TestServeSettingsErrors(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	for name, c := range map[string]struct {
		path string
		want string
	}{
		"missing":    {filepath.Join(dir, "none.json"), `error="Create the settings file: open ` + filepath.Join(dir, "none.json") + `: no such file or directory"`},
		"not JSON":   {writeFile(t, dir, "bad.json", "{"), `error="Fix the JSON in the settings file at byte offset 1."`},
		"a secret":   {writeFile(t, dir, "secret.json", `{"apiary_url":"qak_MARKER"}`), `error="Remove the access key secret (qak_) from the settings file: the provisioner stores its access key in the state folder."`},
		"no adapter": {writeFile(t, dir, "short.json", `{"apiary_url":"https://apiary.example.com","gateway_address":"10.0.1.5"}`), `error="Add the missing members to the settings file: adapter."`},
	} {
		code, errs := runArgs("serve", "--settings", c.path, "--state-dir", state)
		lines := strings.Split(strings.TrimSuffix(errs, "\n"), "\n")
		if code != 1 || len(lines) != 1 || !strings.Contains(lines[0], `level=ERROR msg="the provisioner stopped on an error" `+c.want) || strings.Contains(errs, "MARKER") {
			t.Errorf("%s: exit %d, stderr %q, want one error line with %s", name, code, errs, c.want)
		}
	}
	if _, err := os.Stat(state); err == nil {
		t.Error("serve made the state folder although the settings failed")
	}
}

// TestServeNoEnrolmentCode checks that a first start with no QORY_ENROLMENT_CODE stops
// serve with exit status 1 and the line that says to set it.
func TestServeNoEnrolmentCode(t *testing.T) {
	dir := t.TempDir()
	settings := writeFile(t, dir, "settings.json", `{"apiary_url":"https://apiary.example.com","gateway_address":"10.0.1.5","adapter":{"command":"/usr/local/bin/qory-machines-aws-ec2","settings":"/etc/qory-provisioner/aws-ec2.json"}}`)
	state := filepath.Join(dir, "state")
	code, errs := runArgs("serve", "--settings", settings, "--state-dir", state)
	want := `level=ERROR msg="the provisioner stopped on an error" error="Set QORY_ENROLMENT_CODE to an enrolment code from the node pool's Access key tab in Qory Apiary, then start the provisioner again: the state folder ` + state + ` has no access key."`
	if code != 1 || !strings.Contains(errs, want) || strings.Count(errs, "\n") != 1 {
		t.Errorf("exit %d, stderr %q, want one line with %s", code, errs, want)
	}
}
