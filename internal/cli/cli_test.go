package cli

import (
	"bytes"
	"flag"
	"runtime/debug"
	"testing"
)

// TestProgramVersionFallsBackToTheModuleVersion pins the program's version: the one
// set with ldflags first, as it is given, then the module version go install records,
// a release or a pseudo-version, without its leading v, and "dev" for a build whose
// module version is (devel) or empty, or with no build info.
func TestProgramVersionFallsBackToTheModuleVersion(t *testing.T) {
	pseudo := "0.2.1-0.20261006195344-0f167ba9a536"
	for _, tc := range []struct {
		ldflags, module string
		info            bool
		want            string
	}{
		{"1.2.3", "v0.1.0", true, "1.2.3"},
		{"v1.2.3", "v0.1.0", true, "v1.2.3"},
		{"sha-0123456", "", false, "sha-0123456"},
		{"", "v0.1.0", true, "0.1.0"},
		{"", "v" + pseudo, true, pseudo},
		{"", "v1", true, "1"},
		{"", "0.1.0", true, "0.1.0"},
		{"", "vv0.1.0", true, "vv0.1.0"},
		{"", "version", true, "version"},
		{"", "v", true, "v"},
		{"", "(devel)", true, "dev"},
		{"", "", true, "dev"},
		{"", "", false, "dev"},
	} {
		read := func() (*debug.BuildInfo, bool) {
			if !tc.info {
				return nil, false
			}
			return &debug.BuildInfo{Main: debug.Module{Path: "github.com/qoryai/hive", Version: tc.module}}, true
		}
		if got := ProgramVersion(tc.ldflags, read); got != tc.want {
			t.Errorf("ldflags %q, module %q, build info %v: %q, want %q", tc.ldflags, tc.module, tc.info, got, tc.want)
		}
	}
}

// TestHelpParse pins what a command's help prints: with -h, the whole help; with a flag
// it does not know, its usage and flags alone; and with no flags defined, no flags
// section.
func TestHelpParse(t *testing.T) {
	h := Help{Short: "short", Use: "prog cmd --x X", Long: "long", Example: "  prog cmd --x 1"}
	newFlags := func() *flag.FlagSet {
		fs := flag.NewFlagSet("cmd", flag.ContinueOnError)
		fs.String("x", "", "the x")
		return fs
	}
	var errs bytes.Buffer
	if err := h.Parse(newFlags(), []string{"-h"}, &errs); err != flag.ErrHelp {
		t.Errorf("-h: %v", err)
	}
	if want := "long\n\nusage:\n  prog cmd --x X\n\nflags:\n  --x string\n    \tthe x\n\nexamples:\n  prog cmd --x 1\n"; errs.String() != want {
		t.Errorf("-h prints %q, want %q", errs.String(), want)
	}
	errs.Reset()
	if err := h.Parse(newFlags(), []string{"--y"}, &errs); err == nil || err == flag.ErrHelp {
		t.Errorf("--y: %v", err)
	}
	if want := "flag provided but not defined: --y\nusage:\n  prog cmd --x X\n\nflags:\n  --x string\n    \tthe x\n"; errs.String() != want {
		t.Errorf("--y prints %q, want %q", errs.String(), want)
	}
	errs.Reset()
	if err := h.Parse(flag.NewFlagSet("cmd", flag.ContinueOnError), []string{"--help"}, &errs); err != flag.ErrHelp {
		t.Errorf("--help with no flags: %v", err)
	}
	if want := "long\n\nusage:\n  prog cmd --x X\n\nexamples:\n  prog cmd --x 1\n"; errs.String() != want {
		t.Errorf("--help with no flags prints %q, want %q", errs.String(), want)
	}
	errs.Reset()
	if err := h.Parse(flag.NewFlagSet("cmd", flag.ContinueOnError), []string{"extra"}, &errs); err != nil || errs.Len() != 0 {
		t.Errorf("an argument: %v, prints %q", err, errs.String())
	}
}

// TestFlagsUseTwoDashes pins that the flag list and the flag parser's messages write
// each flag as --name, and that the list shows a default only when it is not the zero
// value, a string default quoted, as flag.PrintDefaults does.
func TestFlagsUseTwoDashes(t *testing.T) {
	h := Help{Use: "prog cmd"}
	newFlags := func() *flag.FlagSet {
		fs := flag.NewFlagSet("cmd", flag.ContinueOnError)
		fs.String("file", "/etc/a.json", "the settings `FILE`")
		fs.String("empty", "", "a string\nover two lines")
		fs.Bool("on", false, "a bool")
		fs.Bool("yes", true, "a bool set")
		fs.Int("n", 0, "a number")
		fs.Int("m", 3, "a number set")
		return fs
	}
	flags := "usage:\n  prog cmd\n\nflags:\n" +
		"  --empty string\n    \ta string\n    \tover two lines\n" +
		"  --file FILE\n    \tthe settings FILE (default \"/etc/a.json\")\n" +
		"  --m int\n    \ta number set (default 3)\n" +
		"  --n int\n    \ta number\n" +
		"  --on\n    \ta bool\n" +
		"  --yes\n    \ta bool set (default true)\n"
	for _, tc := range []struct {
		args []string
		line string
	}{
		{[]string{"-bogus"}, "flag provided but not defined: --bogus"},
		{[]string{"--file"}, "flag needs an argument: --file"},
		{[]string{"-n=x"}, `invalid value "x" for flag --n: parse error`},
		{[]string{"--on=x"}, `invalid boolean value "x" for --on: parse error`},
	} {
		var errs bytes.Buffer
		if err := h.Parse(newFlags(), tc.args, &errs); err == nil {
			t.Errorf("%q: no error", tc.args)
		}
		if want := tc.line + "\n" + flags; errs.String() != want {
			t.Errorf("%q prints %q, want %q", tc.args, errs.String(), want)
		}
	}
}
