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
	if want := "long\n\nusage:\n  prog cmd --x X\n\nflags:\n  -x string\n    \tthe x\n\nexamples:\n  prog cmd --x 1\n"; errs.String() != want {
		t.Errorf("-h prints %q, want %q", errs.String(), want)
	}
	errs.Reset()
	if err := h.Parse(newFlags(), []string{"--y"}, &errs); err == nil || err == flag.ErrHelp {
		t.Errorf("--y: %v", err)
	}
	if want := "flag provided but not defined: -y\nusage:\n  prog cmd --x X\n\nflags:\n  -x string\n    \tthe x\n"; errs.String() != want {
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
