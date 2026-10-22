// Command qory-provisioner is the provisioner's program. Its one command,
// `qory-provisioner serve`, claims dispatches from Qory Apiary, makes one machine for
// each through the machine adapter, and deletes each machine once its dispatch ends. It
// runs until SIGTERM or SIGINT. The program writes its log on standard error and nothing
// on standard output.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"

	"github.com/qoryai/hive/internal/adapter"
	"github.com/qoryai/hive/internal/cli"
	"github.com/qoryai/hive/internal/enrol"
	"github.com/qoryai/hive/internal/journal"
	"github.com/qoryai/hive/internal/loop"
	"github.com/qoryai/hive/internal/provisioner"
	"github.com/qoryai/hive/internal/settings"
	"github.com/qoryai/hive/internal/state"
)

func main() {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
	reset := func() { signal.Stop(stop) }
	os.Exit(run(os.Args[1:], env{stderr: os.Stderr, getenv: os.Getenv, stop: stop, reset: reset}))
}

// version is the program's version, set when it is built with
// -ldflags "-X main.version=...", or else by init to the version of the module it was
// built from, see [cli.ProgramVersion].
var version string

func init() {
	version = cli.ProgramVersion(version, debug.ReadBuildInfo)
}

// env is what the program reads from its process: its error stream, its environment
// and the signals that stop it. reset is called after the first signal, so that the
// next one ends the process with the signal's default action. A test sets each one.
type env struct {
	stderr io.Writer
	getenv func(string) string
	stop   <-chan os.Signal
	reset  func()
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

// run is the command, with its process's parts in e, so a test runs it whole. It
// returns the exit status. The program writes nothing on standard output.
func run(args []string, e env) int {
	if len(args) == 0 || args[0] != "serve" {
		fmt.Fprintln(e.stderr, usage())
		return 2
	}
	return serve(args[1:], e)
}

// The defaults of serve's flags.
const (
	// defaultSettings is the settings file when --settings is not set.
	defaultSettings = "/etc/qory-provisioner/settings.json"
	// defaultStateDir is the state folder when --state-dir is not set.
	defaultStateDir = state.DefaultDir
)

// serveHelp is the help of serve.
var serveHelp = cli.Help{
	Short: "Run the provisioner until SIGTERM",
	Use:   "qory-provisioner serve [--settings FILE] [--state-dir DIR]",
	Long: `serve claims dispatches from Qory Apiary, makes one machine for each through the
machine adapter, and deletes each machine once its dispatch ends. It writes its log on
standard error.

On the first start, while the state folder has no access key, serve enrols one with
the node pool's enrolment code (qecp_…) from QORY_ENROLMENT_CODE.

Run one provisioner per node pool. Every 60 seconds, serve lists the node pool's
machines and deletes each machine whose dispatch has no entry in the journal, with the
log line "found a machine with no journal entry". Two provisioners of one node pool
therefore delete each other's machines, also machines that run a dispatch. For a key
rotation, a new provisioner with a new access key for the node pool, stop the old
provisioner before you start the new one.

serve runs until SIGTERM or SIGINT. It then stops claiming, leaves every machine as it
is, and exits with status 0. A second SIGTERM or SIGINT ends serve at once. serve exits
with status 1 when a failure stops it, also one while it stops, and with status 2 on a
usage error.`,
	Example: `  QORY_ENROLMENT_CODE=qecp_… qory-provisioner serve
  qory-provisioner serve --settings /etc/qory-provisioner/settings.json --state-dir /var/lib/qory-provisioner`,
}

// serveFlags are serve's flags.
type serveFlags struct {
	settings, stateDir string
}

// parseServe parses serve's flags from args. It prints the help for -h, and the usage
// for a flag it does not know or an argument after the flags; ok is false for each.
func parseServe(args []string, stderr io.Writer) (f serveFlags, ok bool) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.StringVar(&f.settings, "settings", defaultSettings, "the provisioner's settings `FILE`")
	fs.StringVar(&f.stateDir, "state-dir", defaultStateDir, "the state folder `DIR`: the access key, the enrolment and the journal of live dispatches")
	if err := serveHelp.Parse(fs, args, stderr); err != nil {
		return serveFlags{}, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "Remove the arguments after the flags: serve takes no arguments.")
		serveHelp.Usage(stderr, fs)
		return serveFlags{}, false
	}
	return f, true
}

// serve runs the provisioner with the flags in args until a signal arrives on e.stop,
// and returns the exit status: 0 after the signal, 1 on a failure that stops it, and 2
// on a usage error.
func serve(args []string, e env) int {
	f, ok := parseServe(args, e.stderr)
	if !ok {
		return 2
	}
	log := slog.New(slog.NewTextHandler(e.stderr, nil))
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		select {
		case s := <-e.stop:
			log.Info("received a stop signal: the provisioner stops claiming", slog.String("signal", s.String()))
			if e.reset != nil {
				e.reset()
			}
			cancel()
		case <-ctx.Done():
		}
	})
	code := start(ctx, f, e, log)
	cancel()
	wg.Wait()
	return code
}

// start reads the settings and the state folder, enrols the access key on the first
// start, opens the journal, and runs the loop until ctx ends. It returns the exit
// status.
func start(ctx context.Context, f serveFlags, e env, log *slog.Logger) int {
	s, err := settings.Read(f.settings)
	if err != nil {
		return failed(ctx, log, err)
	}
	st, err := enrol.Start(ctx, enrol.Config{Dir: f.stateDir, ApiaryURL: s.ApiaryURL, Version: version, Log: log, Getenv: e.getenv})
	if err != nil {
		return failed(ctx, log, err)
	}
	client, err := provisioner.New(provisioner.Config{URL: st.ApiaryURL, KeyID: st.AccessKeyID, Key: st.Key, Pin: st.Pin, InstanceID: st.InstanceID})
	if err != nil {
		return failed(ctx, log, err)
	}
	client.Log = log
	j, err := journal.Open(f.stateDir)
	if err != nil {
		return failed(ctx, log, err)
	}
	if ctx.Err() != nil {
		log.Info("the provisioner stopped")
		return 0
	}
	gateway := s.GatewayAddress.Unmap()
	log.Info("started the provisioner",
		slog.String("version", version), slog.String("apiary_url", st.ApiaryURL), slog.String("pool_id", st.PoolID),
		slog.String("access_key_id", st.AccessKeyID), slog.String("instance_id", st.InstanceID),
		slog.Int("max_machines", s.MaxMachines), slog.String("gateway_address", gateway.String()),
		slog.String("state_dir", f.stateDir), slog.String("adapter", s.Adapter.Command))
	err = loop.Run(ctx, loop.Config{
		Apiary:         apiary{client: client},
		Machines:       adapter.New(s.Adapter.Command, s.Adapter.Settings, e.stderr),
		Journal:        j,
		PoolID:         st.PoolID,
		MaxMachines:    s.MaxMachines,
		GatewayAddress: gateway,
		Log:            log,
	})
	if err != nil {
		return failed(ctx, log, err)
	}
	log.Info("the provisioner stopped")
	return 0
}

// failed logs err, a failure that stops the provisioner, and returns the exit status
// 1. After the stop signal, an error of a step that the signal cancelled, such as the
// machine adapter's check, is logged at level INFO and returns 0.
func failed(ctx context.Context, log *slog.Logger, err error) int {
	if ctx.Err() != nil && cancelled(err) {
		log.Info("the stop signal cancelled a step", slog.String("error", err.Error()))
		log.Info("the provisioner stopped")
		return 0
	}
	log.Error("the provisioner stopped on an error", slog.String("error", err.Error()))
	return 1
}

// cancelled reports whether err is a step that ended because its context was
// cancelled. The loop's errors keep the text of their cause but not the cause itself,
// so the text is checked too.
func cancelled(err error) bool {
	return errors.Is(err, context.Canceled) || strings.Contains(err.Error(), context.Canceled.Error())
}
