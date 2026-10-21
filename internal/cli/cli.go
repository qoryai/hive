// Package cli is the command line of qory-provisioner: a command's help, how its flags
// are parsed, and the program's version.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime/debug"
)

// ProgramVersion is the version set with ldflags when there is one, as it is given, or
// else the main module's version from the build info without its leading v: go records
// v0.1.0 for go install ...@v0.1.0, reported as 0.1.0, or a pseudo-version for a
// commit, v0.2.1-0.20261006195344-0f167ba9a536 reported as
// 0.2.1-0.20261006195344-0f167ba9a536. Only a v followed by a digit is dropped; any
// other version is reported as recorded. A build whose module version is (devel) or
// empty is "dev".
func ProgramVersion(ldflags string, read func() (*debug.BuildInfo, bool)) string {
	if ldflags != "" {
		return ldflags
	}
	info, ok := read()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "dev"
	}
	v := info.Main.Version
	if len(v) > 1 && v[0] == 'v' && '0' <= v[1] && v[1] <= '9' {
		return v[1:]
	}
	return v
}

// Help is one command's help. Short is its line in the program's usage; -h prints the
// rest, and the command's flags, on standard error.
type Help struct {
	Short, Use, Long, Example string
}

// Parse parses args into fs. -h prints the whole help; a flag fs refuses prints the
// command's usage and flags alone, after the flag parser's message.
func (h Help) Parse(fs *flag.FlagSet, args []string, stderr io.Writer) error {
	fs.SetOutput(stderr)
	fs.Usage = func() {}
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprintf(stderr, "%s\n\n", h.Long)
		h.Usage(stderr, fs)
		fmt.Fprintf(stderr, "\nexamples:\n%s\n", h.Example)
	case err != nil:
		h.Usage(stderr, fs)
	}
	return err
}

// Usage prints the command's usage line, and the flags of fs when it has any.
func (h Help) Usage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintf(w, "usage:\n  %s\n", h.Use)
	flags := false
	fs.VisitAll(func(*flag.Flag) { flags = true })
	if flags {
		fmt.Fprint(w, "\nflags:\n")
		fs.PrintDefaults()
	}
}
