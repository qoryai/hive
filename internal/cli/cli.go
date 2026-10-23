// Package cli is the command line of qory-provisioner: a command's help, how its flags
// are parsed, and the program's version.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"
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
// flag parser's message and the command's usage and flags alone. The help and the
// messages write each flag as --name; the parser accepts -name and --name.
func (h Help) Parse(fs *flag.FlagSet, args []string, stderr io.Writer) error {
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprintf(stderr, "%s\n\n", h.Long)
		h.Usage(stderr, fs)
		fmt.Fprintf(stderr, "\nexamples:\n%s\n", h.Example)
	case err != nil:
		fmt.Fprintln(stderr, twoDashes.Replace(err.Error()))
		h.Usage(stderr, fs)
	}
	return err
}

// twoDashes rewrites the flag names in the messages of package flag, which write
// -name, to --name.
var twoDashes = strings.NewReplacer(
	"not defined: -", "not defined: --",
	"needs an argument: -", "needs an argument: --",
	"for flag -", "for flag --",
	"\" for -", "\" for --",
)

// Usage prints the command's usage line, and the flags of fs when it has any, each
// as --name with its value name, its usage and its default when that is not the
// zero value, in the layout of flag.PrintDefaults.
func (h Help) Usage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintf(w, "usage:\n  %s\n", h.Use)
	first := true
	fs.VisitAll(func(f *flag.Flag) {
		if first {
			fmt.Fprint(w, "\nflags:\n")
			first = false
		}
		name, usage := flag.UnquoteUsage(f)
		line := "  --" + f.Name
		if name != "" {
			line += " " + name
		}
		line += "\n    \t" + strings.ReplaceAll(usage, "\n", "\n    \t")
		if def, ok := defaultText(f); ok {
			line += " (default " + def + ")"
		}
		fmt.Fprintln(w, line)
	})
}

// defaultText is the default of f as flag.PrintDefaults writes it, with a string
// quoted, and false when the default is the zero value of the flag's type.
func defaultText(f *flag.Flag) (string, bool) {
	if t := reflect.TypeOf(f.Value); t.Kind() == reflect.Pointer {
		if zero, ok := reflect.New(t.Elem()).Interface().(flag.Value); ok && f.DefValue == zero.String() {
			return "", false
		}
	}
	if g, ok := f.Value.(flag.Getter); ok {
		if _, ok := g.Get().(string); ok {
			return strconv.Quote(f.DefValue), true
		}
	}
	return f.DefValue, true
}
