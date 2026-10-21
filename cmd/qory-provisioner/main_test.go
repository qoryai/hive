package main

import (
	"bytes"
	"testing"
)

// usageText is the program's usage, word for word.
const usageText = `usage:
  qory-provisioner <command> [flags]

commands:
  serve   Print that serve is not built, and exit with status 2

qory-provisioner <command> -h prints a command's help.
`

// serveHelpText is serve's help, word for word.
const serveHelpText = `serve is not built in this commit. It prints one error line on standard error, and
exits with status 2.

usage:
  qory-provisioner serve

examples:
  qory-provisioner serve
`

// runArgs runs the program with args and returns its exit status and what it wrote on
// standard error.
func runArgs(args ...string) (int, string) {
	var errs bytes.Buffer
	code := run(args, &errs)
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

// TestServeIsNotBuilt pins serve's one line, word for word, with exit status 2: with no
// argument, as the image's CMD runs it, and with an argument.
func TestServeIsNotBuilt(t *testing.T) {
	const want = "Build this provisioner from a later commit: serve is not built in this one.\n"
	for _, args := range [][]string{{"serve"}, {"serve", "extra"}} {
		code, errs := runArgs(args...)
		if code != 2 || errs != want {
			t.Errorf("%q: exit %d, stderr %q, want %q", args, code, errs, want)
		}
	}
}

// TestServeUnknownFlag pins that a flag serve does not know prints the flag parser's
// message and serve's usage, with exit status 2.
func TestServeUnknownFlag(t *testing.T) {
	const want = "flag provided but not defined: -bogus\nusage:\n  qory-provisioner serve\n"
	code, errs := runArgs("serve", "--bogus")
	if code != 2 || errs != want {
		t.Errorf("exit %d, stderr %q, want %q", code, errs, want)
	}
}
