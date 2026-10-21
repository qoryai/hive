// Command qory-provisioner is the provisioner's program. Its one command,
// `qory-provisioner serve`, is not built in this commit: it prints one error line on
// standard error and exits with status 2.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/qoryai/hive/internal/cli"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// version is the program's version, set when it is built with
// -ldflags "-X main.version=...", or else by init to the version of the module it was
// built from, see [cli.ProgramVersion].
var version string

func init() {
	version = cli.ProgramVersion(version, debug.ReadBuildInfo)
}

// usage is what the program prints when it is run without a command it knows, or with
// -h or --help alone.
func usage() string {
	return `usage:
  qory-provisioner <command> [flags]

commands:
  serve   ` + serveHelp.Short + `

qory-provisioner <command> -h prints a command's help.`
}

// run is the command, with its error stream, so a test runs it whole. It returns the
// exit status. The program writes nothing on standard output.
func run(args []string, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "serve" {
		fmt.Fprintln(stderr, usage())
		return 2
	}
	return serve(args[1:], stderr)
}

// serveHelp is the help of serve.
var serveHelp = cli.Help{
	Short: "Print that serve is not built, and exit with status 2",
	Use:   "qory-provisioner serve",
	Long: `serve is not built in this commit. It prints one error line on standard error, and
exits with status 2.`,
	Example: `  qory-provisioner serve`,
}

// notBuilt is the line serve prints. This commit has no provisioner loop for serve to
// run, so serve exits with status 2 after it.
const notBuilt = "Build this provisioner from a later commit: serve is not built in this one."

// serve prints its help for -h, its usage for a flag it does not know, and otherwise
// notBuilt. Each exits with status 2.
func serve(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	if err := serveHelp.Parse(fs, args, stderr); err != nil {
		return 2
	}
	fmt.Fprintln(stderr, notBuilt)
	return 2
}
