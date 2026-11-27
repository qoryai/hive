// Package machine_test has the tests of the machine's start unit, qory-machine.service
// and the script qory-machine-start, and of the start-data unit for EC2,
// qory-machine-data-aws-ec2.service and the script qory-machine-data-aws-ec2. The tests
// run each script under a temporary root folder, with stubs in place of every command
// that changes the computer.
package machine_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	// runCode has the form of a run code, and its parts are markers that no output may
	// contain.
	runCode = "qecr_MARKER00000000000000000000.MARKERruncode000000000"
	// token is the IMDSv2 token that the curl stub returns.
	token = "MARKERtoken"
	// gatewayURL and gatewayAddress are the gateway in the start data.
	gatewayURL     = "https://gateway.example.com:8443"
	gatewayAddress = "10.0.1.5"
	// handOff is the start data file under the root folder.
	handOff = "/run/qory-machine-start/start-data"
	// uptime is the content of /proc/uptime under the root folder: the kernel started
	// 12.34 seconds before the script.
	uptime = "12.34 45.67\n"
	// stepsFile is the steps file under the root folder.
	stepsFile = "/var/lib/qory-machine/steps.jsonl"
)

// dangerous are the commands that change the computer, start a service, or reach the
// network. The stub folder has a stub for each, and the test stops before it runs the
// script if any of them resolves outside the stub folder.
var dangerous = []string{
	"chgrp", "chown", "curl", "docker", "halt", "ip", "ip6tables", "iptables", "journalctl", "nft",
	"poweroff", "qory", "reboot", "runuser", "shutdown", "su", "sudo", "systemctl",
	"systemd-notify", "wget",
}

// tools are the real commands that the script and the stubs run. The tools folder has a
// link to each.
var tools = []string{"base64", "chmod", "head", "jq", "ln", "ls", "mkdir", "mktemp", "mv", "rm", "tr"}

// stub is the script behind every stub. It appends its name and arguments to
// $STUB_LOG, one line per call. The curl stub returns $STUB_TOKEN for the token PUT and
// the file $STUB_USERDATA for the user data, and logs the header line it reads on
// stdin. With $STUB_TERM set, the curl stub of the user data sends SIGTERM to the
// script. The runuser stub logs HOME, NOTIFY_SOCKET, QORY_STATUS_FD, the working
// directory, whether the start data file exists and whether the steps file exists, and
// links the run code file to
// $STUB_KEEP, so the test can read it after the script removes it. With
// $STUB_QORY_OUTPUT set, it writes one line to the descriptor in QORY_STATUS_FD and one
// line to stderr, as qory run does. With $STUB_TERM set, it sends SIGTERM to the
// script. The journalctl stub prints $STUB_JOURNAL. A STUB_FAIL_ variable makes one
// stub exit with an error. With $STUB_UPTIME_MS set, every stub adds $STUB_UPTIME_TICK
// milliseconds to the number in that file and writes the sum to /proc/uptime under the
// root folder, so the uptime moves forward between the script's reads.
const stub = `
name=${0##*/}
{ printf '%s' "$name"; printf ' %s' "$@"; printf '\n'; } >>"$STUB_LOG"
if [[ -n ${STUB_UPTIME_MS:-} ]]; then
	ms=$(($(<"$STUB_UPTIME_MS") + STUB_UPTIME_TICK))
	printf '%s' "$ms" >"$STUB_UPTIME_MS"
	printf '%d.%03d 1.00\n' $((ms / 1000)) $((ms % 1000)) >"$QORY_MACHINE_ROOT/proc/uptime"
fi
case $name in
curl)
	case " $* " in
	*" PUT "*)
		[[ -z ${STUB_FAIL_TOKEN:-} ]] || exit 22
		printf '%s' "$STUB_TOKEN"
		;;
	*)
		IFS= read -r header || true
		printf 'curl-stdin %s\n' "$header" >>"$STUB_LOG"
		[[ -z ${STUB_FAIL_USERDATA:-} ]] || exit 22
		printf '%s' "$(<"$STUB_USERDATA")"
		[[ -z ${STUB_TERM:-} ]] || kill -TERM "$PPID"
		;;
	esac
	;;
iptables)
	[[ -z ${STUB_FAIL_IPTABLES:-} ]] || exit 4
	;;
ip6tables)
	[[ -z ${STUB_FAIL_IP6TABLES:-} ]] || exit 4
	;;
systemctl)
	[[ $1 != start || -z ${STUB_FAIL_DOCKER:-} ]] || exit 1
	;;
shutdown)
	[[ -z ${STUB_FAIL_SHUTDOWN:-} ]] || exit 1
	;;
journalctl)
	[[ -z ${STUB_FAIL_JOURNALCTL:-} ]] || exit 1
	printf '%s\n' "${STUB_JOURNAL:-}"
	;;
systemd-notify | chown) ;;
runuser)
	printf 'runuser-home %s\n' "$HOME" >>"$STUB_LOG"
	printf 'runuser-notify-socket %s\n' "${NOTIFY_SOCKET-unset}" >>"$STUB_LOG"
	printf 'runuser-pwd %s\n' "$PWD" >>"$STUB_LOG"
	printf 'runuser-status-fd %s\n' "${QORY_STATUS_FD-unset}" >>"$STUB_LOG"
	if [[ -e $QORY_MACHINE_ROOT/run/qory-machine-start/start-data ]]; then
		printf 'runuser-start-data present\n' >>"$STUB_LOG"
	else
		printf 'runuser-start-data absent\n' >>"$STUB_LOG"
	fi
	if [[ -f $QORY_MACHINE_ROOT/var/lib/qory-machine/steps.jsonl ]]; then
		printf 'runuser-steps present\n' >>"$STUB_LOG"
	else
		printf 'runuser-steps absent\n' >>"$STUB_LOG"
	fi
	if [[ -n ${STUB_QORY_OUTPUT:-} ]]; then
		printf '%s\n' "qory run: a status line" >&"$QORY_STATUS_FD"
		printf '%s\n' "agent: a line of the agent's output" >&2
	fi
	ln "$7" "$STUB_KEEP" 2>/dev/null || true
	[[ -z ${STUB_TERM:-} ]] || kill -TERM "$PPID"
	exit "${STUB_QORY_EXIT:-0}"
	;;
*)
	exit 99
	;;
esac
`

// bash returns a bash of version 5 or later: Homebrew's on macOS, or the one on PATH.
// Without one, the test is skipped.
func bash(t *testing.T) string {
	t.Helper()
	path := "/opt/homebrew/bin/bash"
	if _, err := os.Stat(path); err != nil {
		if path, err = exec.LookPath("bash"); err != nil {
			t.Skipf("install bash 5 to run this test: %v", err)
		}
	}
	out, err := exec.Command(path, "-c", `printf %s "${BASH_VERSINFO[0]}"`).Output()
	if err != nil {
		t.Skipf("install bash 5 to run this test: %s: %v", path, err)
	}
	if string(out) < "5" || len(out) != 1 {
		t.Skipf("install bash 5 to run this test: %s is version %s", path, out)
	}
	return path
}

// resolve returns the first file named name in the folders of path, or "".
func resolve(path, name string) string {
	for _, dir := range filepath.SplitList(path) {
		f := filepath.Join(dir, name)
		if info, err := os.Stat(f); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return f
		}
	}
	return ""
}

// commands makes the stub folder and the tools folder, and returns the PATH that lists
// only them. It skips the test if a tool is not on PATH, and stops the test if a
// dangerous command resolves outside the stub folder.
// The stubs read their settings from the environment, so every run shares the folders.
func commands(t *testing.T, sh string) string {
	t.Helper()
	dir := t.TempDir()
	stubs, toolDir := filepath.Join(dir, "stubs"), filepath.Join(dir, "tools")
	for _, d := range []string{stubs, toolDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range dangerous {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte("#!"+sh+"\n"+stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range tools {
		if slices.Contains(dangerous, name) {
			t.Fatalf("not running the script: the tool %s is a dangerous command", name)
		}
		real, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("install %s to run this test: %v", name, err)
		}
		if err := os.Symlink(real, filepath.Join(toolDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	path := stubs + string(filepath.ListSeparator) + toolDir
	for _, name := range dangerous {
		if got, want := resolve(path, name), filepath.Join(stubs, name); got != want {
			t.Fatalf("not running the script: %s resolves to %q, not to the stub %s", name, got, want)
		}
	}
	return path
}

// env is one run of a script: its root folder, the start data file under it, the files
// of the stubs, and PATH.
type env struct {
	root, data, userData, keep, log, path string
	// dataStays is set for a run after which the start data file stays.
	dataStays bool
}

// newEnv makes a root folder with /home/qory, /etc/hosts, /dev/urandom, /proc/uptime
// and /var/lib, and the start data file with valid start data, mode 0600, in a folder of mode 0700.
// It also makes a user data file for the curl stub, with valid start data.
func newEnv(t *testing.T, path string) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{
		root:     filepath.Join(dir, "root"),
		data:     filepath.Join(dir, "root"+handOff),
		userData: filepath.Join(dir, "user-data"),
		keep:     filepath.Join(dir, "kept-run-code"),
		log:      filepath.Join(dir, "stub.log"),
		path:     path,
	}
	for _, d := range []string{"/home/qory", "/etc", "/run", "/var/lib", "/dev", "/proc"} {
		if err := os.MkdirAll(e.root+d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Dir(e.data), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, e.root+"/etc/hosts", "127.0.0.1 localhost\n")
	write(t, e.root+"/dev/urandom", string(randomBytes())+"more bytes after the 32 that head reads")
	write(t, e.root+"/proc/uptime", uptime)
	e.writeData(t, startData(runCode, gatewayURL, gatewayAddress, 60))
	write(t, e.userData, startData(runCode, gatewayURL, gatewayAddress, 60))
	return e
}

// writeData writes content to the start data file, mode 0600.
func (e *env) writeData(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(e.data, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// randomBytes are the 32 bytes that the test's /dev/urandom starts with. Their
// base64url form, the password, starts with the marker MARKERpassword.
func randomBytes() []byte {
	b, err := base64.RawURLEncoding.DecodeString("MARKERpasswordMARKERpasswordMARKERpassword0")
	if err != nil {
		panic(err)
	}
	return b
}

// password is the engine proxy password that the script makes from randomBytes.
var password = base64.RawURLEncoding.EncodeToString(randomBytes())

func startData(code, url, address string, lifetime int) string {
	b, err := json.Marshal(map[string]any{
		"version":              1,
		"run_code":             code,
		"gateway":              map[string]any{"url": url, "address": address},
		"max_lifetime_minutes": lifetime,
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// stages are the stages that the script prints to stderr, in order.
var stages = []string{"start data read", "firewall set", "Docker started", "run code written", "qory run started"}

// stageLine is a stage line: the script's name, the time in UTC and the stage.
var stageLine = regexp.MustCompile(`^qory-machine-start: ([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z) (.+)$`)

// result is what one run of the script left behind. stderr has the lines that are no
// stage lines, and stages the stages of the stage lines, in order.
type result struct {
	status         int
	stdout, stderr string
	stages         []string
	log            []string
}

// environ is the environment of a run of the script, with vars added.
func (e *env) environ(vars ...string) []string {
	return append([]string{
		"PATH=" + e.path,
		"QORY_MACHINE_ROOT=" + e.root,
		"STUB_LOG=" + e.log,
		"STUB_TOKEN=" + token,
		"STUB_USERDATA=" + e.userData,
		"STUB_KEEP=" + e.keep,
		"NOTIFY_SOCKET=/run/systemd/notify",
		// Local time is 5 hours ahead of UTC, so a stage line in local time fails.
		"TZ=XXX-5",
	}, vars...)
}

// run runs qory-machine-start once and checks what every run must leave: no secret
// marker in stdout, stderr or the stubs' arguments, no token marker in stdout or stderr,
// no run code file, no start data file unless dataStays is set, and systemctl poweroff
// as the last command. A folder at the start data file's path may stay, since rm -f
// does not remove a folder.
func (e *env) run(t *testing.T, sh string, vars ...string) result {
	t.Helper()
	r := e.runScript(t, sh, "qory-machine-start", vars...)
	if _, err := os.Lstat(e.root + "/run/qory-machine/run-code"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the run code file exists after the script exits: %v", err)
	}
	if info, err := os.Lstat(e.data); !e.dataStays && !errors.Is(err, fs.ErrNotExist) && (err != nil || !info.IsDir()) {
		t.Errorf("the start data file exists after the script exits: %v", err)
	}
	if got := r.log[len(r.log)-1]; got != "systemctl poweroff" {
		t.Errorf("the last command is %q, want systemctl poweroff", got)
	}
	return r
}

// runScript runs script once and checks that no secret marker is in stdout, stderr or
// the stubs' arguments, and that no token marker is in stdout or stderr.
func (e *env) runScript(t *testing.T, sh, script string, vars ...string) result {
	t.Helper()
	cmd := exec.Command(sh, script)
	cmd.Env = e.environ(vars...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	r := result{}
	before := time.Now().UTC().Truncate(time.Second)
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		r.status = exit.ExitCode()
	}
	after := time.Now().UTC()
	r.stdout = stdout.String()
	var rest strings.Builder
	for _, line := range strings.SplitAfter(stderr.String(), "\n") {
		m := stageLine.FindStringSubmatch(strings.TrimSuffix(line, "\n"))
		if m == nil {
			rest.WriteString(line)
			continue
		}
		r.stages = append(r.stages, m[2])
		at, err := time.Parse(time.RFC3339, m[1])
		if err != nil || at.Before(before) || at.After(after) {
			t.Errorf("the stage line %q has a time that is not the UTC time of the run, from %s to %s", line, before.Format(time.RFC3339), after.Format(time.RFC3339))
		}
	}
	r.stderr = rest.String()
	b, err := os.ReadFile(e.log)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	r.log = strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	for _, s := range []string{runCode, "MARKER00000000000000000000", "MARKERruncode", password, "MARKERpassword"} {
		for name, out := range map[string]string{"stdout": r.stdout, "stderr": r.stderr, "the stubs' arguments": string(b)} {
			if strings.Contains(out, s) {
				t.Errorf("%s contains the secret marker %s:\n%s", name, s, out)
			}
		}
	}
	// The curl stub logs the token header that it reads on stdin, so only stdout and
	// stderr are checked for the token.
	for name, out := range map[string]string{"stdout": r.stdout, "stderr": r.stderr} {
		if strings.Contains(out, token) {
			t.Errorf("%s contains the token marker %s:\n%s", name, token, out)
		}
	}
	return r
}

// inside reports whether path is dir or inside dir.
func inside(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func mode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestStartUnitRunsQory follows one boot: the script reads the start data file and
// deletes it, drops the metadata service's packets, writes the hosts line, schedules the
// power-off at the end of the lifetime, writes the engine proxy files, starts
// docker.service, runs qory with the run code file in the working folder, and powers the
// machine off.
func TestStartUnitRunsQory(t *testing.T) {
	sh := bash(t)
	path := commands(t, sh)
	cases := []struct {
		name, url, address, hostsLine, daemonJSON string
		lifetime                                  int
		wantDaemon                                map[string]any
	}{
		{name: "IPv4", url: gatewayURL, address: gatewayAddress, hostsLine: "10.0.1.5 gateway.example.com"},
		{name: "lifetime 15", url: gatewayURL, address: gatewayAddress, hostsLine: "10.0.1.5 gateway.example.com", lifetime: 15},
		{name: "lifetime 10080", url: gatewayURL, address: gatewayAddress, hostsLine: "10.0.1.5 gateway.example.com", lifetime: 10080},
		{name: "IPv6, no port", url: "https://gateway.example.com", address: "fd00:ec2::5", hostsLine: "fd00:ec2::5 gateway.example.com"},
		{name: "IPv4 in IPv6", url: gatewayURL, address: "::ffff:10.0.1.5", hostsLine: "::ffff:10.0.1.5 gateway.example.com"},
		{
			name: "empty daemon.json", url: gatewayURL, address: gatewayAddress, hostsLine: "10.0.1.5 gateway.example.com",
			daemonJSON: "\n",
		},
		{
			name: "existing daemon.json", url: gatewayURL, address: gatewayAddress, hostsLine: "10.0.1.5 gateway.example.com",
			daemonJSON: `{"log-driver": "journald", "proxies": {"http-proxy": "http://old.example.com"}}`,
			wantDaemon: map[string]any{"log-driver": "journald"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, path)
			lifetime := c.lifetime
			if lifetime == 0 {
				lifetime = 60
			}
			e.writeData(t, startData(runCode, c.url, c.address, lifetime))
			if c.daemonJSON != "" {
				if err := os.MkdirAll(e.root+"/etc/docker", 0o755); err != nil {
					t.Fatal(err)
				}
				write(t, e.root+"/etc/docker/daemon.json", c.daemonJSON)
			}
			// A writer that was killed before its rename left this temporary file.
			write(t, filepath.Dir(e.data)+"/.start-data.Ab12Cd", "left by a killed writer")
			r := e.run(t, sh, "STUB_QORY_OUTPUT=1")
			if got := entries(t, filepath.Dir(e.data)); len(got) != 0 {
				t.Errorf("the start data folder contains %q after the run, want no file", got)
			}
			// qory run's status line reaches the script's stderr, and its stderr the
			// script's stdout, which reaches the journal only.
			if r.status != 0 || r.stdout != "agent: a line of the agent's output\n" || r.stderr != "qory run: a status line\n" {
				t.Fatalf("status %d, stdout %q, stderr %q; want 0, qory run's stderr line on stdout, and its status line and the stage lines on stderr", r.status, r.stdout, r.stderr)
			}
			equalStages(t, r.stages, stages)
			want := []string{
				"iptables -w -t raw -I PREROUTING -d 169.254.169.254 -j DROP",
				"ip6tables -w -t raw -I PREROUTING -d fd00:ec2::254 -j DROP",
				"iptables -w -t raw -I OUTPUT -d 169.254.169.254 -j DROP",
				"ip6tables -w -t raw -I OUTPUT -d fd00:ec2::254 -j DROP",
				"shutdown -P +" + strconv.Itoa(lifetime),
				"chown qory " + e.root + "/etc/qory/engine-proxy.json",
				"chown -R qory " + e.root + "/etc/docker/certs.d",
				"systemd-notify --ready",
				"systemctl start docker.service",
				"chown qory " + e.root + "/run/qory-machine/run-code " + e.root + "/run/qory-machine",
				"chown qory " + e.root + "/var/lib/qory-machine/work",
				"runuser -u qory -- qory run --run-code-file " + e.root + "/run/qory-machine/run-code --machine-steps-file " + e.root + stepsFile + " --gateway " + c.url,
				"runuser-home " + e.root + "/home/qory",
				"runuser-notify-socket unset",
				"runuser-pwd " + e.root + "/var/lib/qory-machine/work",
				"runuser-status-fd 3",
				"runuser-start-data absent",
				"runuser-steps present",
				"systemctl poweroff",
			}
			if !slices.Equal(r.log, want) {
				t.Errorf("the commands are\n%s\nwant\n%s", strings.Join(r.log, "\n"), strings.Join(want, "\n"))
			}

			if got, want := read(t, e.root+"/etc/hosts"), "127.0.0.1 localhost\n"+c.hostsLine+"\n"; got != want {
				t.Errorf("/etc/hosts is %q, want %q", got, want)
			}

			proxy := e.root + "/etc/qory/engine-proxy.json"
			if got, want := read(t, proxy), `{"version": 1, "address": "127.0.0.1:3129", "password": "`+password+`"}`+"\n"; got != want {
				t.Errorf("engine-proxy.json is %q, want %q", got, want)
			}
			if got := mode(t, proxy); got != 0o400 {
				t.Errorf("engine-proxy.json has mode %v, want 0400", got)
			}
			if got := mode(t, e.root+"/etc/qory"); got != fs.ModeDir|0o755 {
				t.Errorf("/etc/qory has mode %v, want 0755", got)
			}

			daemon := e.root + "/etc/docker/daemon.json"
			var got map[string]any
			if err := json.Unmarshal([]byte(read(t, daemon)), &got); err != nil {
				t.Fatalf("daemon.json is not JSON: %v", err)
			}
			wantDaemon := map[string]any{"proxies": map[string]any{
				"http-proxy":  "http://qory:" + password + "@127.0.0.1:3129",
				"https-proxy": "http://qory:" + password + "@127.0.0.1:3129",
				"no-proxy":    "localhost,127.0.0.1",
			}}
			for k, v := range c.wantDaemon {
				wantDaemon[k] = v
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(wantDaemon)
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Errorf("daemon.json is %s, want %s", gotJSON, wantJSON)
			}
			if got := mode(t, daemon); got != 0o600 {
				t.Errorf("daemon.json has mode %v, want 0600", got)
			}
			if got := mode(t, e.root+"/etc/docker/certs.d"); got != fs.ModeDir|0o755 {
				t.Errorf("certs.d has mode %v, want 0755", got)
			}

			if got := read(t, e.keep); got != runCode {
				t.Errorf("the run code file that qory read is not the run code")
			}
			if got := mode(t, e.keep); got != 0o600 {
				t.Errorf("the run code file had mode %v, want 0600", got)
			}
			if got := mode(t, e.root+"/run/qory-machine"); got != fs.ModeDir|0o700 {
				t.Errorf("/run/qory-machine has mode %v, want 0700", got)
			}

			// qory run exits with an error when its home, under HOME, is inside the folder
			// it runs in.
			work := e.root + "/var/lib/qory-machine/work"
			if got := mode(t, work); got != fs.ModeDir|0o700 {
				t.Errorf("/var/lib/qory-machine/work has mode %v, want 0700", got)
			}
			if got := mode(t, e.root+"/var/lib/qory-machine"); got != fs.ModeDir|0o755 {
				t.Errorf("/var/lib/qory-machine has mode %v, want 0755", got)
			}
			if work == "/" || inside(e.root+"/home/qory", work) || inside(work, e.root+"/home/qory") {
				t.Errorf("qory runs in %s, which is / or is inside or contains HOME %s", work, e.root+"/home/qory")
			}
		})
	}
}

// equalStages fails the test when the stages of the stage lines are not want.
func equalStages(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("the stage lines have the stages %q, want %q", got, want)
	}
}

// TestStartUnitRunsWithStderrClosed runs the script with stderr closed. The stage
// lines cannot be written, and the script still runs qory and exits with 0.
func TestStartUnitRunsWithStderrClosed(t *testing.T) {
	sh := bash(t)
	e := newEnv(t, commands(t, sh))
	cmd := exec.Command(sh, "-c", `exec "$0" qory-machine-start 2>&-`, sh)
	cmd.Env = e.environ()
	if out, err := cmd.Output(); err != nil || len(out) != 0 {
		t.Fatalf("the script returned %v with stdout %q; want no error and no output", err, out)
	}
	log := strings.Split(strings.TrimSuffix(read(t, e.log), "\n"), "\n")
	run := "runuser -u qory -- qory run --run-code-file " + e.root + "/run/qory-machine/run-code --machine-steps-file " + e.root + stepsFile + " --gateway " + gatewayURL
	if !slices.Contains(log, run) || log[len(log)-1] != "systemctl poweroff" {
		t.Errorf("the commands do not contain %q, or do not end with systemctl poweroff:\n%s", run, strings.Join(log, "\n"))
	}
}

// readOnly replaces path with an empty folder of mode 0555. Appending to a folder fails
// for every user; creating a file in it fails for every user except root, so a test
// run by root is skipped unless path is the file that the script appends to.
func readOnly(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 && !strings.HasSuffix(path, "/etc/hosts") {
		t.Skip("root can write into a folder of mode 0555")
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o755) })
}

// link makes a symbolic link at path to target, and the folders that contain path.
func link(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

// remove removes path, a file or an empty folder.
func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

// skipRoot skips the test when it runs as root, who can read and write a file whatever
// its mode.
func skipRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root can read and write a file whatever its mode")
	}
}

// otherOwner replaces the start data file with a file that the user who runs the test
// does not own, see [otherOwnerFile].
func otherOwner(t *testing.T, root string) {
	t.Helper()
	otherOwnerFile(t, root+handOff)
}

// otherOwnerFile replaces the file at path with a file that the user who runs the test
// does not own. Run as root, the test changes the file's owner to uid 65534. Otherwise,
// it makes path a hard link to a root-owned file, and skips when no such link can be
// made. The script removes only the link.
func otherOwnerFile(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		if err := os.Chown(path, 65534, 65534); err != nil {
			t.Fatal(err)
		}
		return
	}
	remove(t, path)
	for _, f := range []string{"/etc/hosts", "/private/etc/hosts", "/etc/passwd", "/etc/protocols", "/etc/services"} {
		if info, err := os.Lstat(f); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if os.Link(f, path) == nil {
			return
		}
	}
	t.Skip("make a file that another user owns to run this test: no hard link to a root-owned file could be made")
}

// otherOwnerFolder changes the owner of the folder dir to uid 65534. Only root can do
// that, and a folder has no hard link, so the test is skipped when it does not run as
// root.
func otherOwnerFolder(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("run this test as root: only root can make a folder that another user owns, and a folder cannot be hard linked")
	}
	if err := os.Chown(dir, 65534, 65534); err != nil {
		t.Fatal(err)
	}
}

// linkStays fails the test unless path is a link to target and target does not exist.
func linkStays(t *testing.T, target, path string) {
	t.Helper()
	if got, err := os.Readlink(path); err != nil || got != target {
		t.Errorf("%s links to %q (%v), want a link to %s", path, got, err, target)
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s exists after the run: %v", target, err)
	}
}

// fileStays fails the test unless path is a file with content.
func fileStays(t *testing.T, path, content string) {
	t.Helper()
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("%s is not a regular file after the run: %v", path, err)
	}
	if got := read(t, path); got != content {
		t.Errorf("%s contains %q, want %q", path, got, content)
	}
}

// linkFolder replaces the start data folder under root with a link to the folder
// root/elsewhere, which contains a start data file and a temporary file .start-data.x.
func linkFolder(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Dir(root + handOff)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root+"/elsewhere", 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, root+"/elsewhere/start-data", startData(runCode, gatewayURL, gatewayAddress, 60))
	write(t, root+"/elsewhere/.start-data.x", "a temporary file")
	link(t, root+"/elsewhere", dir)
}

// runCodeLinesFile is the optional file of the machine image under the root folder.
const runCodeLinesFile = "/etc/qory/run-code-lines"

// paddedRegistry returns a registry line that, with the run code and the newline between
// them, makes a run code file of size bytes, with or without a newline after the line.
func paddedRegistry(size int, newline bool) string {
	line := "registry "
	end := ""
	if newline {
		end = "\n"
	}
	pad := size - len(runCode) - 1 - len(line) - len(".example") - len(end)
	return line + strings.Repeat("a", pad) + ".example" + end
}

// TestStartUnitRunCodeLines checks the lines after the run code: without the file
// /etc/qory/run-code-lines the run code file is the run code alone, and with it the file
// is the run code, a newline and the lines of the file.
func TestStartUnitRunCodeLines(t *testing.T) {
	sh := bash(t)
	path := commands(t, sh)
	registry, apiary, runtimes := "registry registry.example.com:5000", "apiary https://apiary.example.com", "runtimes /var/lib/qory/runtimes"
	cases := []struct{ name, content string }{
		{"no file", ""},
		{"registry and apiary", registry + "\n" + apiary + "\n"},
		{"registry and apiary, no newline at the end", registry + "\n" + apiary},
		{"one line with a newline", registry + "\n"},
		{"one line without a newline", apiary},
		{"all three lines", registry + "\n" + apiary + "\n" + runtimes + "\n"},
		{"the lines in another order", runtimes + "\n" + apiary + "\n" + registry},
		{"registry without a port", "registry registry.example.com"},
		{"registry localhost", "registry localhost"},
		{"registry with a colon and no dot", "registry registry:5000"},
		{"apiary with a port", "apiary https://apiary.example.com:8443"},
		{"apiary on http for localhost", "apiary http://localhost:4000"},
		{"apiary on http for a loopback address", "apiary http://127.0.0.1"},
		{"runtimes at the root", "runtimes /"},
		{"exactly 1 KiB with a newline", paddedRegistry(1024, true)},
		{"exactly 1 KiB without a newline", paddedRegistry(1024, false)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, path)
			if c.name != "no file" {
				if err := os.MkdirAll(e.root+"/etc/qory", 0o755); err != nil {
					t.Fatal(err)
				}
				write(t, e.root+runCodeLinesFile, c.content)
			}
			r := e.run(t, sh)
			if r.status != 0 || r.stdout != "" || r.stderr != "" {
				t.Fatalf("status %d, stdout %q, stderr %q; want 0 and only stage lines", r.status, r.stdout, r.stderr)
			}
			equalStages(t, r.stages, stages)
			want := runCode
			if c.content != "" {
				want += "\n" + c.content
			}
			if got := read(t, e.keep); got != want {
				t.Errorf("the run code file that qory read has %d bytes and is not the run code and the lines of the file, %d bytes", len(got), len(want))
			}
			if got := mode(t, e.keep); got != 0o600 {
				t.Errorf("the run code file had mode %v, want 0600", got)
			}
			if got := read(t, e.root+"/etc/hosts"); strings.Contains(got, "registry") {
				t.Errorf("/etc/hosts contains a line of the file: %q", got)
			}
			// The chown call is in the log. The stage line is checked with the other lines.
			wantChown := "chown qory " + e.root + "/run/qory-machine/run-code " + e.root + "/run/qory-machine"
			if !slices.Contains(r.log, wantChown) {
				t.Errorf("the commands do not contain %q:\n%s", wantChown, strings.Join(r.log, "\n"))
			}
		})
	}
}

// TestStartUnitRunCodeLinesFails checks each file that qory would refuse: the script
// prints one line that starts with the fix and contains no line of the file, exits 1, and
// writes no run code file, and qory does not run.
func TestStartUnitRunCodeLinesFails(t *testing.T) {
	sh := bash(t)
	path := commands(t, sh)
	registry, apiary := "registry registry.example.com", "apiary https://apiary.example.com"
	const (
		other   = "Write each line of {file} as registry <host>, apiary <url> or runtimes <folder>. Line %d has another form."
		blank   = "Remove the blank line, line %d, from {file}. The run code file may have no blank line."
		badReg  = "Fix line %d of {file}. The registry value must be a host of letters, digits, . and - with a dot, a colon or the name localhost, and an optional port from 1 to 65535."
		badAp   = "Fix line %d of {file}. The apiary value must be https://<host> or https://<host>:<port>, or http:// for localhost or a 127.x.x.x address, with the scheme in lowercase, a port from 1 to 65535 and no user, path, query or fragment."
		badRun  = "Fix line %d of {file}. The runtimes value must be an absolute path with no //, ., .. or trailing /, except the root /."
		twice   = "Remove the second %s line, line %d, from {file}. Each of the lines registry, apiary and runtimes may appear once."
		tooMany = "Keep at most 3 lines in {file}, one each of registry, apiary and runtimes. The file has more."
		control = "Remove every control character and every byte above 126 from {file}. It may contain printable ASCII lines only."
		secret  = "Remove the text qecr_, qak_ or qrk_, in any letter case, from {file}. It looks like a run code, a run key or an access key secret."
		empty   = "Write the lines registry, apiary or runtimes into {file}, or remove it. The file is empty."
		tooLong = "Shorten {file} so that the run code, a newline and the file make at most 1024 bytes. The run code file would be longer."
	)
	cases := []struct{ name, content, want string }{
		{"empty file", "", empty},
		{"a blank line", registry + "\n\n" + apiary + "\n", fmt.Sprintf(blank, 2)},
		{"a blank line first", "\n" + registry + "\n", fmt.Sprintf(blank, 1)},
		{"two newlines at the end", registry + "\n\n", fmt.Sprintf(blank, 2)},
		{"only a newline", "\n", fmt.Sprintf(blank, 1)},
		{"the same line twice", registry + "\n" + registry + "\n", fmt.Sprintf(twice, "registry", 2)},
		{"the same key with another value", apiary + "\n" + registry + "\napiary https://other.example.com\n", fmt.Sprintf(twice, "apiary", 3)},
		{"a fourth line", registry + "\n" + apiary + "\nruntimes /srv/runtimes\nruntimes /srv/other\n", tooMany},
		{"an unknown key", "gateway https://gateway.example.com\n", fmt.Sprintf(other, 1)},
		{"a key with no value", "registry\n", fmt.Sprintf(badReg, 1)},
		{"a key in capitals", "REGISTRY registry.example.com\n", fmt.Sprintf(other, 1)},
		{"a line with a leading space", " " + registry + "\n", fmt.Sprintf(other, 1)},
		{"a registry with a path", "registry registry.example.com/v2\n", fmt.Sprintf(badReg, 1)},
		{"a registry with a user", "registry user@registry.example.com\n", fmt.Sprintf(badReg, 1)},
		{"a registry with an https scheme", "registry https://registry.example.com\n", fmt.Sprintf(badReg, 1)},
		{"a registry with no dot or colon", "registry registry\n", fmt.Sprintf(badReg, 1)},
		{"a registry with port 0", "registry registry.example.com:0\n", fmt.Sprintf(badReg, 1)},
		{"a registry with port 65536", "registry registry.example.com:65536\n", fmt.Sprintf(badReg, 1)},
		{"a registry with two spaces", "registry  registry.example.com\n", fmt.Sprintf(badReg, 1)},
		{"an apiary with a path", "apiary https://apiary.example.com/v1\n", fmt.Sprintf(badAp, 1)},
		{"an apiary with a slash at the end", "apiary https://apiary.example.com/\n", fmt.Sprintf(badAp, 1)},
		{"an apiary with a query", "apiary https://apiary.example.com?a=b\n", fmt.Sprintf(badAp, 1)},
		{"an apiary with a fragment", "apiary https://apiary.example.com#a\n", fmt.Sprintf(badAp, 1)},
		{"an apiary with a user", "apiary https://user@apiary.example.com\n", fmt.Sprintf(badAp, 1)},
		{"an apiary on http for a remote host", "apiary http://apiary.example.com\n", fmt.Sprintf(badAp, 1)},
		{"an apiary with another scheme", "apiary ftp://apiary.example.com\n", fmt.Sprintf(badAp, 1)},
		{"an apiary with a scheme in capitals", "apiary HTTPS://apiary.example.com\n", fmt.Sprintf(badAp, 1)},
		{"an apiary with a scheme in mixed case", "apiary Https://apiary.example.com\n", fmt.Sprintf(badAp, 1)},
		{"an apiary with no scheme", "apiary apiary.example.com\n", fmt.Sprintf(badAp, 1)},
		{"an apiary with port 0", "apiary https://apiary.example.com:0\n", fmt.Sprintf(badAp, 1)},
		{"a runtimes folder that is relative", "runtimes var/runtimes\n", fmt.Sprintf(badRun, 1)},
		{"a runtimes folder with a slash at the end", "runtimes /var/runtimes/\n", fmt.Sprintf(badRun, 1)},
		{"a runtimes folder with //", "runtimes /var//runtimes\n", fmt.Sprintf(badRun, 1)},
		{"a runtimes folder with ..", "runtimes /var/../runtimes\n", fmt.Sprintf(badRun, 1)},
		{"a runtimes folder with .", "runtimes /var/./runtimes\n", fmt.Sprintf(badRun, 1)},
		{"a runtimes folder that ends in ..", "runtimes /var/..\n", fmt.Sprintf(badRun, 1)},
		{"a bad line after a good one", registry + "\n" + "apiary http://apiary.example.com\n", fmt.Sprintf(badAp, 2)},
		{"a line ending with CR", registry + "\r\n", control},
		{"a tab", "registry\tregistry.example.com\n", control},
		{"a NUL byte", registry + "\x00\n", control},
		{"a byte above 126", "runtimes /srv/caf\xc3\xa9\n", control},
		{"a run code in the file", "registry " + runCode + "\n", secret},
		{"a run code in capitals", "registry QECR_FOO.example.com\n", secret},
		{"an access key secret", "registry qak_abc.example.com\n", secret},
		{"a run key", "apiary https://qrk_abc.example.com\n", secret},
		{"a secret in a mixed case", "registry x.QaK_1.example.com\n", secret},
		{"1 KiB and 1 byte with a newline", paddedRegistry(1025, true), tooLong},
		{"1 KiB and 1 byte without a newline", paddedRegistry(1025, false), tooLong},
		{"far above 1 KiB", strings.Repeat(registry+"\n", 100), tooLong},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, path)
			if err := os.MkdirAll(e.root+"/etc/qory", 0o755); err != nil {
				t.Fatal(err)
			}
			write(t, e.root+runCodeLinesFile, c.content)
			e.expectRefused(t, sh, strings.ReplaceAll(c.want, "{file}", e.root+runCodeLinesFile), c.content)
		})
	}

	file := func(e *env) string { return e.root + runCodeLinesFile }
	t.Run("a symbolic link", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, path)
		write(t, e.root+"/elsewhere", registry+"\n")
		link(t, e.root+"/elsewhere", file(e))
		e.expectRefused(t, sh, "Write "+file(e)+" as a regular file, or remove it. The path is a symbolic link.", registry)
	})
	t.Run("a link to nothing", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, path)
		link(t, e.root+"/missing", file(e))
		e.expectRefused(t, sh, "Write "+file(e)+" as a regular file, or remove it. The path is a symbolic link.", registry)
	})
	t.Run("a folder", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, path)
		if err := os.MkdirAll(file(e), 0o755); err != nil {
			t.Fatal(err)
		}
		e.expectRefused(t, sh, "Write "+file(e)+" as a regular file, or remove it. The path is not a regular file.", registry)
	})
	t.Run("another owner", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, path)
		if err := os.MkdirAll(e.root+"/etc/qory", 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, file(e), registry+"\n")
		otherOwnerFile(t, file(e))
		e.expectRefused(t, sh, "Write "+file(e)+" as root, or remove it. Another user owns the file.", registry)
	})
	t.Run("unreadable", func(t *testing.T) {
		t.Parallel()
		skipRoot(t)
		e := newEnv(t, path)
		if err := os.MkdirAll(e.root+"/etc/qory", 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, file(e), registry+"\n")
		if err := os.Chmod(file(e), 0); err != nil {
			t.Fatal(err)
		}
		e.expectRefused(t, sh, "Check that root can read "+file(e)+". Reading the file failed.", registry)
	})
}

// watchHead returns path with a folder before the others that has a head command. It logs
// to $STUB_LOG one line "head-watch <state> <arguments>" for each call, where state is
// present or absent for the file $HEAD_WATCH_FILE at the time of the call, and then runs
// the real head. A test that sets HEAD_WATCH_FILE to the run code file sees whether the
// script wrote the file before a call.
func watchHead(t *testing.T, sh, path string) string {
	t.Helper()
	real := resolve(path, "head")
	if real == "" {
		t.Skip("install head to run this test")
	}
	dir := t.TempDir()
	body := "#!" + sh + "\n" +
		"if [[ -e $HEAD_WATCH_FILE ]]; then state=present; else state=absent; fi\n" +
		"printf 'head-watch %s %s\\n' \"$state\" \"$*\" >>\"$STUB_LOG\"\n" +
		"exec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "head"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir + string(filepath.ListSeparator) + path
}

// watched returns the lines that watchHead logged for calls that read file.
func watched(r result, file string) []string {
	var calls []string
	for _, line := range r.log {
		if strings.HasPrefix(line, "head-watch ") && strings.HasSuffix(line, " "+file) {
			calls = append(calls, line)
		}
	}
	return calls
}

// TestStartUnitRunCodeLinesOrder checks that the script reads and validates the file
// before it writes the run code file: at the time of the read, the run code file does not
// exist, for a file that the script accepts and for one that it refuses.
func TestStartUnitRunCodeLinesOrder(t *testing.T) {
	sh := bash(t)
	path := watchHead(t, sh, commands(t, sh))
	for _, c := range []struct {
		name, content string
		status        int
	}{
		{"accepted", "registry registry.example.com\n", 0},
		{"refused", "registry registry\n", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, path)
			if err := os.MkdirAll(e.root+"/etc/qory", 0o755); err != nil {
				t.Fatal(err)
			}
			write(t, e.root+runCodeLinesFile, c.content)
			r := e.run(t, sh, "HEAD_WATCH_FILE="+e.root+"/run/qory-machine/run-code")
			if r.status != c.status {
				t.Errorf("status %d, want %d", r.status, c.status)
			}
			calls := watched(r, e.root+runCodeLinesFile)
			if len(calls) != 1 || !strings.HasPrefix(calls[0], "head-watch absent ") {
				t.Errorf("the script read the file with %q, want one call made while the run code file does not exist", calls)
			}
		})
	}
}

// TestStartUnitRunCodeLinesBoundedRead checks that the script reads at most 1025 bytes of
// the file, one byte more than qory's limit of 1024, and so refuses a file of 1 GiB in
// the time of a small one. The file is sparse, so it takes no disk space.
func TestStartUnitRunCodeLinesBoundedRead(t *testing.T) {
	sh := bash(t)
	path := watchHead(t, sh, commands(t, sh))
	e := newEnv(t, path)
	if err := os.MkdirAll(e.root+"/etc/qory", 0o755); err != nil {
		t.Fatal(err)
	}
	file := e.root + runCodeLinesFile
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil {
		f.Close()
		t.Skipf("the file system cannot make a sparse file of 1 GiB: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	want := "Shorten " + file + " so that the run code, a newline and the file make at most 1024 bytes. The run code file would be longer."
	r := e.expectRefused(t, sh, want, "", "HEAD_WATCH_FILE="+e.root+"/run/qory-machine/run-code")
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("refusing a file of 1 GiB took %v, want at most 30s: the read is not bounded", took)
	}
	calls := watched(r, file)
	if len(calls) != 1 || !strings.HasSuffix(calls[0], " -c 1025 -- "+file) {
		t.Errorf("the script read the file with %q, want one call: head -c 1025 -- %s", calls, file)
	}
}

// expectRefused runs the script with vars added and returns the result. It checks that
// the script refuses with the line want and no other line, exits 1, prints no stage after
// "Docker started", runs no qory, and prints none of the lines of the file, content.
func (e *env) expectRefused(t *testing.T, sh, want, content string, vars ...string) result {
	t.Helper()
	r := e.run(t, sh, append([]string{"STUB_QORY_OUTPUT=1"}, vars...)...)
	if r.status != 1 || r.stdout != "" || r.stderr != want+"\n" {
		t.Errorf("status %d, stdout %q, stderr %q; want 1, no stdout and the line %q", r.status, r.stdout, r.stderr, want)
	}
	equalStages(t, r.stages, stages[:3])
	for _, line := range r.log {
		if strings.HasPrefix(line, "runuser") {
			t.Errorf("the script ran qory after a refusal: %q", line)
		}
	}
	if _, err := os.Lstat(e.keep); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("qory read a run code file after a refusal: %v", err)
	}
	for _, line := range strings.Split(content, "\n") {
		_, value, _ := strings.Cut(line, " ")
		if len(value) > 9 && strings.Contains(r.stderr+r.stdout, value) {
			t.Errorf("the output contains the value %q of a line of the file", value)
		}
	}
	return r
}

// TestStartUnitFails checks each invalid input and each failed step: the script prints
// one line to stderr, runs no qory after an invalid input, and powers the machine off.
func TestStartUnitFails(t *testing.T) {
	sh := bash(t)
	path := commands(t, sh)
	const (
		json1     = "Launch this machine with start data that is one JSON object. The start data is not one JSON object."
		escape    = "Write the start data without a backslash. Valid start data has no escape sequence, and the start data contains a backslash."
		member    = "Remove the unknown members from the start data. Version 1 has only version, run_code, gateway and max_lifetime_minutes, and gateway has only url and address."
		version   = "Set version to 1 in the start data. This script reads only start data version 1."
		lifetime  = "Set max_lifetime_minutes in the start data to an integer from 15 to 10080. The value is missing, out of range or not a plain integer."
		code      = "Set run_code in the start data to the run code from Qory Apiary. The value does not have the form qecr_<26 characters>.<22 characters>, with an optional second .<22 characters>."
		gateway   = "Set gateway in the start data to an object with url and address. The gateway value is not an object."
		url       = "Set gateway.url in the start data to https://<host> or https://<host>:<port>. The value has another scheme, a user name, a path or a query, or its host or port is not valid."
		address   = "Set gateway.address in the start data to an IPv4 or IPv6 address. The value is not an IP address."
		missing   = "Install the start-data unit for this machine's vendor. {root}/run/qory-machine-start/start-data does not exist or is empty."
		folder    = "Remove {root}/run/qory-machine-start. The path is a link, not a folder, or a folder that another user owns."
		symlink   = "Write the start data to {root}/run/qory-machine-start/start-data as a regular file. The path is a symbolic link."
		irregular = "Write the start data to {root}/run/qory-machine-start/start-data as a regular file. The path is not a regular file."
	)
	good := func(code, url, address string) string { return startData(code, url, address, 60) }
	// doc is start data with the raw JSON texts of version and max_lifetime_minutes.
	doc := func(version, lifetime string) string {
		return `{"version": ` + version + `, "run_code": "` + runCode + `", "gateway": {"url": "https://gateway.example.com", "address": "10.0.1.5"}, "max_lifetime_minutes": ` + lifetime + `}`
	}
	cases := []struct {
		name, data string
		daemonJSON string
		// readOnly is a folder under the root that the script cannot write.
		readOnly string
		vars     []string
		status   int
		ran      string // a command the script ran before it stopped, or ""
		want     string
		// orWant is a second accepted line, for input that jq versions parse differently.
		orWant string
		// setup changes the root folder before the run.
		setup func(t *testing.T, root string)
		// stage is the stage of the last stage line that the script prints before it
		// stops, or "" when it stops before the first stage line. A case sets both ran
		// and stage, or neither.
		stage string
		// stdout is the script's stdout, which has qory run's stdout and stderr.
		stdout string
		// dataStays is set when the start data file stays after the run.
		dataStays bool
		// check checks the root folder after the run.
		check func(t *testing.T, root string)
	}{
		{name: "not JSON", data: `{"version": 1,`, want: json1},
		{name: "empty", data: ``, want: missing},
		{name: "only a newline", data: "\n", want: json1},
		{name: "two objects", data: good(runCode, gatewayURL, gatewayAddress) + good(runCode, gatewayURL, gatewayAddress), want: json1},
		{name: "an array", data: `[1]`, want: json1},
		{name: "an escaped duplicate key", data: doc("1", `60, "max_lifetime_minute\u0073": 9999`), want: escape},
		{name: "an escaped slash", data: good(runCode, `https:\/\/gateway.example.com`, gatewayAddress), want: escape},
		{name: "an unknown member", data: `{"version": 1, "run_code": "` + runCode + `", "gateway": {"url": "https://gateway.example.com", "address": "10.0.1.5"}, "max_lifetime_minutes": 60, "port": 1}`, want: member},
		{name: "a gateway member", data: `{"version": 1, "run_code": "` + runCode + `", "gateway": {"url": "https://gateway.example.com", "address": "10.0.1.5", "port": 1}, "max_lifetime_minutes": 60}`, want: member},
		{name: "version 2", data: doc("2", "60"), want: version},
		{name: "version 1.0", data: doc("1.0", "60"), want: version},
		{name: "version 1e0", data: doc("1e0", "60"), want: version},
		{name: "version 10e-1", data: doc("10e-1", "60"), want: version},
		{name: "version 01", data: doc("01", "60"), want: version},
		{name: "version twice", data: doc(`1, "version": 1`, "60"), want: version},
		{name: "version as text", data: doc(`"1"`, "60"), want: version},
		{name: "no lifetime", data: `{"version": 1, "run_code": "` + runCode + `", "gateway": {"url": "https://gateway.example.com", "address": "10.0.1.5"}}`, want: lifetime},
		{name: "lifetime 14", data: doc("1", "14"), want: lifetime},
		{name: "lifetime 10081", data: doc("1", "10081"), want: lifetime},
		{name: "lifetime 100000", data: doc("1", "100000"), want: lifetime},
		{name: "lifetime 0", data: doc("1", "0"), want: lifetime},
		{name: "lifetime 60.0", data: doc("1", "60.0"), want: lifetime},
		{name: "lifetime 6e1", data: doc("1", "6e1"), want: lifetime},
		{name: "lifetime 060", data: doc("1", "060"), want: lifetime},
		{name: "lifetime -60", data: doc("1", "-60"), want: lifetime},
		// jq 1.8 reads +60 as the number 60.
		{name: "lifetime +60", data: doc("1", "+60"), want: lifetime, orWant: json1},
		{name: "lifetime as text", data: doc("1", `"60"`), want: lifetime},
		{name: "lifetime twice", data: doc("1", `60, "max_lifetime_minutes": 60`), want: lifetime},
		{name: "no version", data: `{"run_code": "` + runCode + `", "gateway": {"url": "https://gateway.example.com", "address": "10.0.1.5"}, "max_lifetime_minutes": 60}`, want: version},
		{name: "no run code", data: `{"version": 1, "gateway": {"url": "https://gateway.example.com", "address": "10.0.1.5"}, "max_lifetime_minutes": 60}`, want: code},
		{name: "run code as a number", data: `{"version": 1, "run_code": 5, "gateway": {"url": "https://gateway.example.com", "address": "10.0.1.5"}, "max_lifetime_minutes": 60}`, want: code},
		{name: "run code in lower case", data: good(strings.ToLower(runCode), gatewayURL, gatewayAddress), want: code},
		{name: "run code with I", data: good("qecr_IARKER00000000000000000000.MARKERruncode000000000", gatewayURL, gatewayAddress), want: code},
		{name: "run code with three parts", data: good(runCode+".MARKERruncode000000000.MARKERruncode000000000", gatewayURL, gatewayAddress), want: code},
		{name: "run code too short", data: good(runCode[:len(runCode)-1], gatewayURL, gatewayAddress), want: code},
		// JSON writes the newline as \n, so the backslash check stops the script.
		{name: "run code with a newline", data: good(runCode+"\n", gatewayURL, gatewayAddress), want: escape},
		{name: "gateway as text", data: `{"version": 1, "run_code": "` + runCode + `", "gateway": "https://gateway.example.com", "max_lifetime_minutes": 60}`, want: gateway},
		{name: "URL with a path", data: good(runCode, gatewayURL+"/v1", gatewayAddress), want: url},
		{name: "URL with a slash", data: good(runCode, gatewayURL+"/", gatewayAddress), want: url},
		{name: "URL with a query", data: good(runCode, gatewayURL+"?a=1", gatewayAddress), want: url},
		{name: "URL with a fragment", data: good(runCode, gatewayURL+"#a", gatewayAddress), want: url},
		{name: "URL with a user", data: good(runCode, "https://user:MARKERruncode@gateway.example.com", gatewayAddress), want: url},
		{name: "URL over http", data: good(runCode, "http://gateway.example.com", gatewayAddress), want: url},
		{name: "URL with port 0", data: good(runCode, "https://gateway.example.com:0", gatewayAddress), want: url},
		{name: "URL with port 70000", data: good(runCode, "https://gateway.example.com:70000", gatewayAddress), want: url},
		{name: "URL with no host", data: good(runCode, "https://:8443", gatewayAddress), want: url},
		{name: "URL with an IPv6 host", data: good(runCode, "https://[fd00::1]:8443", gatewayAddress), want: url},
		{name: "URL with a space", data: good(runCode, "https://gateway example.com", gatewayAddress), want: url},
		{name: "address as a name", data: good(runCode, gatewayURL, "gateway.example.com"), want: address},
		{name: "address over 255", data: good(runCode, gatewayURL, "10.0.1.256"), want: address},
		{name: "address with 3 parts", data: good(runCode, gatewayURL, "10.0.1"), want: address},
		{name: "address with a leading zero", data: good(runCode, gatewayURL, "010.0.1.5"), want: address},
		{name: "address with a zone", data: good(runCode, gatewayURL, "fd00::1%eth0"), want: address},
		{name: "address with two ::", data: good(runCode, gatewayURL, "fd00::1::2"), want: address},
		{name: "address with 9 groups", data: good(runCode, gatewayURL, "1:2:3:4:5:6:7:8:9"), want: address},
		{name: "empty address", data: good(runCode, gatewayURL, ""), want: address},
		{name: "no start data file", setup: func(t *testing.T, root string) { remove(t, root+handOff) }, want: missing},
		{
			name: "no start data folder", want: missing,
			setup: func(t *testing.T, root string) {
				if err := os.RemoveAll(filepath.Dir(root + handOff)); err != nil {
					t.Fatal(err)
				}
			},
		},
		// The script removes the link, and the file it links to stays.
		{
			name: "a symbolic link", want: symlink,
			setup: func(t *testing.T, root string) {
				remove(t, root+handOff)
				write(t, root+"/target", startData(runCode, gatewayURL, gatewayAddress, 60))
				link(t, root+"/target", root+handOff)
			},
			check: func(t *testing.T, root string) {
				if _, err := os.Lstat(root + "/target"); err != nil {
					t.Errorf("the file that the link pointed to is gone: %v", err)
				}
			},
		},
		{name: "a link to nothing", want: symlink, setup: func(t *testing.T, root string) { remove(t, root+handOff); link(t, root+"/none", root+handOff) }},
		{name: "a link to a folder", want: symlink, setup: func(t *testing.T, root string) { remove(t, root+handOff); link(t, root+"/etc", root+handOff) }},
		// The script reads no file through the link, and removes none in the folder that
		// the link points to.
		{
			name: "the start data folder is a link", want: folder, dataStays: true, setup: linkFolder,
			check: func(t *testing.T, root string) {
				if got := entries(t, root+"/elsewhere"); !slices.Equal(got, []string{".start-data.x", "start-data"}) {
					t.Errorf("the folder that the link points to contains %q, want .start-data.x and start-data", got)
				}
			},
		},
		{
			name: "a file at the start data folder", want: folder, dataStays: true,
			setup: func(t *testing.T, root string) {
				dir := filepath.Dir(root + handOff)
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				write(t, dir, "not a folder")
			},
		},
		{
			name: "a link to nothing at the start data folder", want: folder,
			setup: func(t *testing.T, root string) {
				dir := filepath.Dir(root + handOff)
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				link(t, root+"/none", dir)
			},
			check: func(t *testing.T, root string) { linkStays(t, root+"/none", filepath.Dir(root+handOff)) },
		},
		{name: "another user's folder", want: folder, setup: func(t *testing.T, root string) { otherOwnerFolder(t, filepath.Dir(root+handOff)) }},
		{
			name: "a named pipe", want: irregular,
			setup: func(t *testing.T, root string) {
				remove(t, root+handOff)
				if err := syscall.Mkfifo(root+handOff, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a folder", want: irregular,
			setup: func(t *testing.T, root string) {
				remove(t, root+handOff)
				if err := os.Mkdir(root+handOff, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{name: "another user's file", setup: otherOwner, want: "Write {root}/run/qory-machine-start/start-data as root. Another user owns the file."},
		{
			name: "start data file not readable", want: "Check that root can read {root}/run/qory-machine-start/start-data. Reading the file failed.",
			setup: func(t *testing.T, root string) {
				skipRoot(t)
				if err := os.Chmod(root+handOff, 0); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "start data folder not writable", dataStays: true,
			want: "Check that root can write {root}/run/qory-machine-start. Removing {root}/run/qory-machine-start/start-data failed.",
			setup: func(t *testing.T, root string) {
				skipRoot(t)
				dir := filepath.Dir(root + handOff)
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			},
		},
		{name: "iptables fails", stage: "start data read", vars: []string{"STUB_FAIL_IPTABLES=1"}, ran: "iptables -w -t raw -I PREROUTING -d 169.254.169.254 -j DROP", want: "Check that iptables can add rules to the raw table on this machine image. Adding the PREROUTING rule that drops packets to 169.254.169.254 failed."},
		{name: "daemon.json is not an object", stage: "firewall set", daemonJSON: `["not an object"]`, ran: "iptables -w -t raw -I OUTPUT -d 169.254.169.254 -j DROP", want: "Make {root}/etc/docker/daemon.json in the machine image one JSON object, or remove it. Writing the engine proxy into it failed."},
		// The journal's lines reach stderr with the password replaced and without the
		// lines that look like a secret, before the failure line.
		{
			name: "docker.service fails", stage: "firewall set", ran: "journalctl -b -u docker.service -n 20 --no-pager -o cat",
			vars: []string{"STUB_FAIL_DOCKER=1", "STUB_JOURNAL=" + strings.Join([]string{
				"Starting docker.service",
				"proxy http://qory:" + password + "@127.0.0.1:3129 refused",
				"a key qrk_TESTrunkey000000000000 in a line",
				"an access key qak_TESTaccesskey0000000000 in a line",
				"a run code QECR_TEST in a line",
				password + " twice " + password,
				"docker.service failed",
			}, "\n")},
			want: "Starting docker.service\n" +
				"proxy http://qory:[hidden]@127.0.0.1:3129 refused\n" +
				"[hidden] twice [hidden]\n" +
				"docker.service failed\n" +
				"Read the lines of docker.service above this line in the machine log for the cause. Starting docker.service failed.",
		},
		{
			name: "docker.service and journalctl fail", stage: "firewall set", ran: "journalctl -b -u docker.service -n 20 --no-pager -o cat",
			vars: []string{"STUB_FAIL_DOCKER=1", "STUB_FAIL_JOURNALCTL=1", "STUB_JOURNAL=never printed"},
			want: "Read the lines of docker.service above this line in the machine log for the cause. Starting docker.service failed.",
		},
		{name: "SIGTERM", stage: "qory run started", vars: []string{"STUB_TERM=1"}, status: 143, ran: "runuser-home {root}/home/qory", want: "No action needed. The script stopped on SIGTERM, and the machine powers off."},
		{name: "ip6tables fails", stage: "start data read", vars: []string{"STUB_FAIL_IP6TABLES=1"}, ran: "ip6tables -w -t raw -I PREROUTING -d fd00:ec2::254 -j DROP", want: "Check that ip6tables can add rules to the raw table on this machine image. Adding the PREROUTING rule that drops packets to fd00:ec2::254 failed."},
		{name: "shutdown fails", stage: "firewall set", vars: []string{"STUB_FAIL_SHUTDOWN=1"}, ran: "shutdown -P +60", want: "Check that shutdown -P can schedule a power-off on this machine image. Scheduling the power-off in 60 minutes failed."},
		{name: "/etc/hosts not writable", stage: "firewall set", readOnly: "/etc/hosts", ran: "ip6tables -w -t raw -I OUTPUT -d fd00:ec2::254 -j DROP", want: "Check that root can write {root}/etc/hosts. Appending the gateway's line failed."},
		{name: "/etc/docker not writable", stage: "firewall set", readOnly: "/etc/docker", ran: "ip6tables -w -t raw -I OUTPUT -d fd00:ec2::254 -j DROP", want: "Check that root can write {root}/etc/docker. Writing {root}/etc/docker/daemon.json failed."},
		{name: "/etc/qory not writable", stage: "firewall set", readOnly: "/etc/qory", ran: "ip6tables -w -t raw -I OUTPUT -d fd00:ec2::254 -j DROP", want: "Check that root can write {root}/etc/qory. Writing {root}/etc/qory/engine-proxy.json failed."},
		{name: "/run/qory-machine not writable", stage: "Docker started", readOnly: "/run/qory-machine", ran: "systemctl start docker.service", want: "Check that root can write {root}/run/qory-machine. Writing {root}/run/qory-machine/run-code failed."},
		{
			name: "qory run fails", stage: "qory run started", vars: []string{"STUB_QORY_EXIT=3", "STUB_QORY_OUTPUT=1"}, status: 3, ran: "runuser-status-fd 3",
			stdout: "agent: a line of the agent's output\n",
			want:   "qory run: a status line\nRead the status lines of qory run above this line in the machine log for the cause. qory run exited with status 3.",
		},
		{name: "/var/lib/qory-machine not writable", stage: "run code written", readOnly: "/var/lib/qory-machine", ran: "chown qory {root}/run/qory-machine/run-code {root}/run/qory-machine", want: "Check that root can write {root}/var/lib/qory-machine. Making {root}/var/lib/qory-machine/work failed."},
		// The script resolves the link and stops before it writes into /.
		{
			name: "working folder links to /", stage: "run code written", ran: "chown qory {root}/run/qory-machine/run-code {root}/run/qory-machine",
			setup: func(t *testing.T, root string) { link(t, "/", root+"/var/lib/qory-machine/work") },
			want:  "Make {root}/var/lib/qory-machine/work and {root}/home/qory two folders in the machine image, neither inside the other. With links resolved, they are / and {real}/home/qory.",
		},
		{
			name: "working folder links into home", stage: "run code written", ran: "chown qory {root}/run/qory-machine/run-code {root}/run/qory-machine",
			setup: func(t *testing.T, root string) {
				if err := os.Mkdir(root+"/home/qory/work", 0o755); err != nil {
					t.Fatal(err)
				}
				link(t, root+"/home/qory/work", root+"/var/lib/qory-machine/work")
			},
			want: "Make {root}/var/lib/qory-machine/work and {root}/home/qory two folders in the machine image, neither inside the other. With links resolved, they are {real}/home/qory/work and {real}/home/qory.",
		},
		{
			name: "home links into the working folder", stage: "run code written", ran: "chown qory {root}/run/qory-machine/run-code {root}/run/qory-machine",
			setup: func(t *testing.T, root string) {
				work := root + "/var/lib/qory-machine/work"
				if err := os.MkdirAll(work, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(root+"/home/qory", work+"/qory"); err != nil {
					t.Fatal(err)
				}
				link(t, work+"/qory", root+"/home/qory")
			},
			want: "Make {root}/var/lib/qory-machine/work and {root}/home/qory two folders in the machine image, neither inside the other. With links resolved, they are {real}/var/lib/qory-machine/work and {real}/var/lib/qory-machine/work/qory.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, path)
			e.dataStays = c.dataStays
			if c.data != "" || c.name == "empty" {
				e.writeData(t, c.data)
			}
			if c.daemonJSON != "" {
				if err := os.MkdirAll(e.root+"/etc/docker", 0o755); err != nil {
					t.Fatal(err)
				}
				write(t, e.root+"/etc/docker/daemon.json", c.daemonJSON)
			}
			if c.readOnly != "" {
				readOnly(t, e.root+c.readOnly)
			}
			if c.setup != nil {
				c.setup(t, e.root)
			}
			r := e.run(t, sh, c.vars...)
			status := c.status
			if status == 0 {
				status = 1
			}
			if r.status != status {
				t.Errorf("status %d, want %d", r.status, status)
			}
			real, err := filepath.EvalSymlinks(e.root)
			if err != nil {
				t.Fatal(err)
			}
			want := strings.NewReplacer("{root}", e.root, "{real}", real).Replace(c.want) + "\n"
			if c.orWant != "" && r.stderr == c.orWant+"\n" {
				want = r.stderr
			}
			if r.stderr != want || r.stdout != c.stdout {
				t.Errorf("stdout %q, stderr %q; want stdout %q and stderr\n%q", r.stdout, r.stderr, c.stdout, want)
			}
			if (c.ran == "") != (c.stage == "") {
				t.Fatalf("the case sets ran %q and stage %q; set both or neither", c.ran, c.stage)
			}
			equalStages(t, r.stages, stages[:slices.Index(stages, c.stage)+1])
			ran := strings.ReplaceAll(c.ran, "{root}", e.root)
			if ran != "" && !slices.Contains(r.log, ran) {
				t.Errorf("the commands do not contain %q:\n%s", ran, strings.Join(r.log, "\n"))
			}
			if ran == "" && slices.ContainsFunc(r.log, func(l string) bool { return strings.HasPrefix(l, "ip") }) {
				t.Errorf("the script ran iptables after an invalid input:\n%s", strings.Join(r.log, "\n"))
			}
			if c.status == 0 && slices.ContainsFunc(r.log, func(l string) bool { return strings.HasPrefix(l, "runuser") }) {
				t.Errorf("the script ran qory after a failure:\n%s", strings.Join(r.log, "\n"))
			}
			if _, err := os.Lstat(e.root + stepsFile); c.status == 0 && !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the steps file exists after a failure before qory run: %v", err)
			}
			if c.check != nil {
				c.check(t, e.root)
			}
		})
	}
}

// stepNames are the names of the steps in the steps file, in order.
var stepNames = []string{"boot", "read start data", "set firewall", "start Docker", "write run code"}

// stepTime is a time in the steps file: UTC to the millisecond.
var stepTime = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}Z$`)

// stepRecord is one line of the steps file.
type stepRecord struct {
	name           string
	started, ended time.Time
}

// steps reads the steps file under root and checks each line: one JSON object with
// exactly name, started and ended, times in UTC to the millisecond, each step's start
// not after its end, and each step after the first starting where the one before it
// ended.
func steps(t *testing.T, root string) []stepRecord {
	t.Helper()
	text := read(t, root+stepsFile)
	if !strings.HasSuffix(text, "\n") {
		t.Errorf("the steps file %q does not end with a newline", text)
	}
	var records []stepRecord
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		var fields map[string]string
		if err := json.Unmarshal([]byte(line), &fields); err != nil || len(fields) != 3 {
			t.Fatalf("line %d of the steps file is %q, want a JSON object with name, started and ended: %v", i+1, line, err)
		}
		var at [2]time.Time
		for j, key := range []string{"started", "ended"} {
			if !stepTime.MatchString(fields[key]) {
				t.Fatalf("line %d of the steps file has %s %q, want UTC to the millisecond", i+1, key, fields[key])
			}
			var err error
			if at[j], err = time.Parse(time.RFC3339Nano, fields[key]); err != nil {
				t.Fatal(err)
			}
		}
		if at[1].Before(at[0]) {
			t.Errorf("line %d of the steps file ends before it starts: %s", i+1, line)
		}
		if i > 0 && !at[0].Equal(records[i-1].ended) {
			t.Errorf("line %d of the steps file starts at %s, want the end of the step before it, %s", i+1, at[0].Format(time.RFC3339Nano), records[i-1].ended.Format(time.RFC3339Nano))
		}
		records = append(records, stepRecord{name: fields["name"], started: at[0], ended: at[1]})
	}
	return records
}

// failingTool makes a folder with a script named name that runs body, and returns the
// PATH that lists it before path.
func failingTool(t *testing.T, sh, path, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!"+sh+"\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir + string(filepath.ListSeparator) + path
}

// TestStartUnitWritesSteps checks the steps file: it exists when qory run starts, has
// one line for each step, in order, contains no value of the start data, and has mode
// 0644. Without a number in /proc/uptime, it has no boot line. A failed or skipped write
// prints one line, leaves no steps file and no temporary file, and qory run still runs.
func TestStartUnitWritesSteps(t *testing.T) {
	sh := bash(t)
	path := commands(t, sh)
	const (
		writeFailed = "Check that root can write {root}/var/lib/qory-machine. Writing {root}/var/lib/qory-machine/steps.jsonl failed, so qory run starts without it."
		aFolder     = "Remove the folder {root}/var/lib/qory-machine/steps.jsonl. A folder is at the path of the steps file, so qory run starts without it."
		badState    = "Make {root}/var/lib/qory-machine a folder, not a link, that root owns and that no group or other user can write. The folder fails this check, so qory run starts without {root}/var/lib/qory-machine/steps.jsonl."
	)
	stale := func(t *testing.T, root string) {
		t.Helper()
		if err := os.MkdirAll(root+"/var/lib/qory-machine", 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, root+stepsFile, `{"name":"a step from the machine image"}`+"\n")
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, root string)
		// tool is a tool that the run finds first on PATH, and toolBody its script.
		tool, toolBody string
		// tick is the milliseconds that each stub adds to the uptime, or 0.
		tick int
		// names are the steps in the file, or nil when the write fails or is skipped.
		names []string
		// durations are the lengths of the steps in milliseconds, or nil.
		durations []int64
		// want is the line on stderr, or "".
		want   string
		status int
		stages []string
		// check checks the root folder after the run.
		check func(t *testing.T, root string)
		// linked is set when the folder is a link, and the file in the folder that it
		// links to stays.
		linked bool
	}{
		{name: "a normal start", names: stepNames},
		// The stubs that run in each step add 1005 ms each: none in read start data,
		// four in set firewall, five in start Docker and one in write run code. The
		// lengths follow the uptime, not the clock, which moves far less.
		{name: "an uptime that moves", tick: 1005, names: stepNames, durations: []int64{12340, 0, 4020, 5025, 1005}},
		{name: "no /proc/uptime", names: stepNames[1:], setup: func(t *testing.T, root string) { remove(t, root+"/proc/uptime") }},
		{name: "empty /proc/uptime", names: stepNames[1:], setup: func(t *testing.T, root string) { write(t, root+"/proc/uptime", "") }},
		{name: "/proc/uptime not a number", names: stepNames[1:], setup: func(t *testing.T, root string) { write(t, root+"/proc/uptime", "12,34 45,67\n") }},
		{name: "/proc/uptime a whole number", names: stepNames, setup: func(t *testing.T, root string) { write(t, root+"/proc/uptime", "12 45\n") }},
		{
			name: "a link at steps.jsonl", names: stepNames,
			setup: func(t *testing.T, root string) {
				write(t, root+"/elsewhere", "the link's target")
				link(t, root+"/elsewhere", root+stepsFile)
			},
			check: func(t *testing.T, root string) {
				if got := mode(t, root+stepsFile); !got.IsRegular() {
					t.Errorf("steps.jsonl has mode %v, want a regular file", got)
				}
				fileStays(t, root+"/elsewhere", "the link's target")
			},
		},
		// rm fails on a folder for every user, root among them, so the script does not
		// move the temporary file into it.
		{
			name: "a folder at steps.jsonl", want: aFolder,
			setup: func(t *testing.T, root string) {
				if err := os.MkdirAll(root+stepsFile, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, root string) {
				if got := entries(t, root+stepsFile); len(got) != 0 {
					t.Errorf("the folder at steps.jsonl contains %q, want no file", got)
				}
			},
		},
		{
			name: "folder not writable", want: writeFailed,
			setup: func(t *testing.T, root string) {
				skipRoot(t)
				dir := root + "/var/lib/qory-machine"
				if err := os.MkdirAll(dir+"/work", 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dir, 0o555); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			},
		},
		{name: "a stale file and mktemp fails", want: writeFailed, setup: stale, tool: "mktemp", toolBody: "exit 1"},
		{name: "a stale file and mv fails", want: writeFailed, setup: stale, tool: "mv", toolBody: "exit 1"},
		{
			name: "a stale file and a group that can write the folder", want: badState,
			setup: func(t *testing.T, root string) {
				stale(t, root)
				if err := os.Chmod(root+"/var/lib/qory-machine", 0o775); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "other users can write the folder", want: badState,
			setup: func(t *testing.T, root string) {
				if err := os.MkdirAll(root+"/var/lib/qory-machine", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(root+"/var/lib/qory-machine", 0o757); err != nil {
					t.Fatal(err)
				}
			},
		},
		// The script removes and writes nothing through the link.
		{
			name: "the folder is a link", want: badState, linked: true,
			setup: func(t *testing.T, root string) {
				if err := os.MkdirAll(root+"/state", 0o755); err != nil {
					t.Fatal(err)
				}
				write(t, root+"/state/steps.jsonl", "a file in the folder that the link points to\n")
				link(t, root+"/state", root+"/var/lib/qory-machine")
			},
			check: func(t *testing.T, root string) {
				fileStays(t, root+"/state/steps.jsonl", "a file in the folder that the link points to\n")
			},
		},
		// SIGTERM during the write: the exit removes the temporary file.
		{
			name: "SIGTERM during the write", want: "No action needed. The script stopped on SIGTERM, and the machine powers off.",
			status: 143, stages: stages[:4], tool: "mv", toolBody: `kill -TERM "$PPID"; exit 1`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, path)
			if c.tool != "" {
				e.path = failingTool(t, sh, path, c.tool, c.toolBody)
			}
			var vars []string
			if c.tick != 0 {
				counter := filepath.Join(t.TempDir(), "uptime-ms")
				write(t, counter, "12340")
				vars = []string{"STUB_UPTIME_MS=" + counter, "STUB_UPTIME_TICK=" + strconv.Itoa(c.tick)}
			}
			if c.setup != nil {
				c.setup(t, e.root)
			}
			before := time.Now()
			r := e.run(t, sh, vars...)
			after := time.Now()
			wantStages := c.stages
			if wantStages == nil {
				wantStages = stages
			}
			equalStages(t, r.stages, wantStages)
			want := ""
			if c.want != "" {
				want = strings.ReplaceAll(c.want, "{root}", e.root) + "\n"
			}
			if r.status != c.status || r.stderr != want {
				t.Errorf("status %d, stderr %q; want %d and %q", r.status, r.stderr, c.status, want)
			}
			if c.status == 0 {
				wantSteps := "runuser-steps present"
				if c.names == nil && !c.linked {
					wantSteps = "runuser-steps absent"
				}
				if !slices.Contains(r.log, wantSteps) {
					t.Errorf("the commands do not contain %q:\n%s", wantSteps, strings.Join(r.log, "\n"))
				}
				run := "runuser -u qory -- qory run --run-code-file " + e.root + "/run/qory-machine/run-code --gateway " + gatewayURL
				if c.names != nil {
					run = "runuser -u qory -- qory run --run-code-file " + e.root + "/run/qory-machine/run-code --machine-steps-file " + e.root + stepsFile + " --gateway " + gatewayURL
				}
				if !slices.Contains(r.log, run) {
					t.Errorf("the commands do not contain %q:\n%s", run, strings.Join(r.log, "\n"))
				}
			}
			if c.check != nil {
				c.check(t, e.root)
			}
			dir := e.root + "/var/lib/qory-machine"
			if got := entries(t, dir); slices.ContainsFunc(got, func(n string) bool { return strings.HasPrefix(n, ".steps") }) {
				t.Errorf("the folder %s contains %q after the run, want no temporary file", dir, got)
			}
			if c.names == nil {
				if info, err := os.Lstat(e.root + stepsFile); !c.linked && err == nil && !info.IsDir() {
					t.Errorf("a steps file exists after a failed or skipped write")
				}
				return
			}
			records := steps(t, e.root)
			var names []string
			for _, rec := range records {
				names = append(names, rec.name)
			}
			if !slices.Equal(names, c.names) {
				t.Errorf("the steps are %q, want %q", names, c.names)
			}
			if got := entries(t, dir); !slices.Equal(got, []string{"steps.jsonl", "work"}) {
				t.Errorf("the folder %s contains %q, want steps.jsonl and work", dir, got)
			}
			if got := mode(t, e.root+stepsFile); got != 0o644 {
				t.Errorf("the steps file has mode %v, want 0644", got)
			}
			text := read(t, e.root+stepsFile)
			for _, s := range []string{runCode, "MARKER", password, gatewayURL, "gateway.example.com", gatewayAddress, "8443"} {
				if strings.Contains(text, s) {
					t.Errorf("the steps file contains %q, a value of the start data:\n%s", s, text)
				}
			}
			if c.durations != nil {
				for i, rec := range records {
					if got := rec.ended.Sub(rec.started).Milliseconds(); got != c.durations[i] {
						t.Errorf("the step %s lasts %d ms, want %d ms", rec.name, got, c.durations[i])
					}
				}
				return
			}
			// The uptime stays the same during the run, so every step ends when the file
			// is written, and boot starts the seconds in /proc/uptime before that.
			from, to := before.Truncate(time.Millisecond), after
			if records[0].name == "boot" {
				up, err := strconv.ParseFloat(strings.Fields(read(t, e.root+"/proc/uptime"))[0], 64)
				if err != nil {
					t.Fatal(err)
				}
				d := time.Duration(up * float64(time.Second))
				if got := records[0].ended.Sub(records[0].started); got != d {
					t.Errorf("the boot step lasts %s, want %s", got, d)
				}
				from, to = from.Add(-d), to.Add(-d)
			}
			if first := records[0].started; first.Before(from) || first.After(to) {
				t.Errorf("the first step starts at %s, want from %s to %s", first.Format(time.RFC3339Nano), from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano))
			}
		})
	}
}

// TestStartUnitStepTimes runs the script's function utc on fixed times: it writes UTC
// to the millisecond, with the milliseconds padded to three digits, whatever the local
// time zone.
func TestStartUnitStepTimes(t *testing.T) {
	sh := bash(t)
	fn := regexp.MustCompile(`(?ms)^utc\(\) \{$.*?^\}$`).FindString(read(t, "qory-machine-start"))
	if fn == "" {
		t.Fatal("qory-machine-start has no function utc")
	}
	for _, us := range []int64{1762389104005000, 1762389104999999, 1762389104000999, 0} {
		cmd := exec.Command(sh, "-c", fn+"\nutc \"$1\"\nprintf %s \"$REPLY\"", sh, strconv.FormatInt(us, 10))
		cmd.Env = []string{"LC_ALL=C", "TZ=XXX-5"}
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if want := time.UnixMicro(us).UTC().Format("2006-01-02T15:04:05.000Z"); string(out) != want {
			t.Errorf("utc %d is %q, want %q", us, out, want)
		}
	}
}

// sections returns the lines of each section of a unit file, by the section's header.
func sections(unit string) map[string][]string {
	all := map[string][]string{}
	section := ""
	for _, line := range strings.Split(unit, "\n") {
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		all[section] = append(all[section], line)
	}
	return all
}

// TestStartUnitFiles checks the unit files, the start of each script, and the lines
// that no file may contain.
func TestStartUnitFiles(t *testing.T) {
	unit := read(t, "qory-machine.service")
	for _, line := range []string{
		"Wants=network-online.target",
		"After=network-online.target",
		"Before=docker.service",
		"Type=notify",
		"NotifyAccess=all",
		"ExecStart=/usr/local/libexec/qory/qory-machine-start",
		"StandardError=journal+console",
		"TimeoutStartSec=120",
		"SuccessAction=poweroff",
		"FailureAction=poweroff",
		"WantedBy=multi-user.target",
	} {
		if !slices.Contains(strings.Split(unit, "\n"), line) {
			t.Errorf("qory-machine.service has no line %s", line)
		}
	}
	// systemd reads SuccessAction and FailureAction only in [Unit], and ignores them in
	// [Service].
	unitSections := sections(unit)
	for _, line := range []string{"SuccessAction=poweroff", "FailureAction=poweroff"} {
		if !slices.Contains(unitSections["[Unit]"], line) {
			t.Errorf("qory-machine.service has no line %s in [Unit]", line)
		}
		key := strings.SplitAfter(line, "=")[0]
		if slices.ContainsFunc(unitSections["[Service]"], func(l string) bool { return strings.HasPrefix(l, key) }) {
			t.Errorf("qory-machine.service sets %s in [Service], where systemd ignores it", key)
		}
	}

	// The start-data unit for EC2 runs before the start unit and powers the machine off
	// when it fails. It has no RequiredBy= or Requires=, so its failure does not skip the
	// start unit.
	ec2Unit := read(t, "qory-machine-data-aws-ec2.service")
	ec2Sections := sections(ec2Unit)
	for section, lines := range map[string][]string{
		"[Unit]": {
			"Wants=network-online.target",
			"After=network-online.target",
			"Before=qory-machine.service",
			"FailureAction=poweroff",
		},
		"[Service]": {
			"Type=oneshot",
			"ExecStart=/usr/local/libexec/qory/qory-machine-data-aws-ec2",
			"StandardError=journal+console",
			"TimeoutStartSec=120",
		},
		"[Install]": {"WantedBy=qory-machine.service"},
	} {
		for _, line := range lines {
			if !slices.Contains(ec2Sections[section], line) {
				t.Errorf("qory-machine-data-aws-ec2.service has no line %s in %s", line, section)
			}
		}
	}
	for _, line := range strings.Split(ec2Unit, "\n") {
		if strings.HasPrefix(line, "RequiredBy=") || strings.HasPrefix(line, "Requires=") || strings.HasPrefix(line, "SuccessAction=") {
			t.Errorf("qory-machine-data-aws-ec2.service has the line %s", line)
		}
	}

	files := map[string]string{"qory-machine.service": unit, "qory-machine-data-aws-ec2.service": ec2Unit}
	for _, name := range []string{"qory-machine-start", "qory-machine-data-aws-ec2"} {
		script := read(t, name)
		files[name] = script
		if !strings.HasPrefix(script, "#!/bin/bash\n") || !strings.Contains(script, "\nset -euo pipefail\n") {
			t.Errorf("%s does not start with #!/bin/bash and set -euo pipefail", name)
		}
		if got := mode(t, name); got&0o111 == 0 {
			t.Errorf("%s has mode %v, want an executable", name, got)
		}
	}
	// An absolute command path would bypass the stubs on PATH.
	absolute := regexp.MustCompile(`(^|[\s;|&(])/(usr/)?s?bin/`)
	forbidden := regexp.MustCompile(`set -x|set -o xtrace`)
	for name, text := range files {
		for i, line := range strings.Split(text, "\n") {
			if m := forbidden.FindString(line); m != "" {
				t.Errorf("%s:%d contains %q", name, i+1, m)
			}
			if i > 0 && !strings.HasPrefix(strings.TrimSpace(line), "#") && absolute.MatchString(line) {
				t.Errorf("%s:%d runs a command by an absolute path: %s", name, i+1, line)
			}
		}
	}
}

// TestStartUnitSecretLines pins every line of each script that expands the token, the
// start data, the run code or the password. The runs in the other tests observe the
// arguments of the stubs only, so this list covers the arguments of the real tools: each
// line passes the secret to a builtin printf, a pipe, a file or a [[ test, or is inside a
// jq program. A new line that expands a secret fails this test until it is reviewed and
// added here.
func TestStartUnitSecretLines(t *testing.T) {
	allowed := map[string][]string{
		"qory-machine-start": {
			`if [[ $data == *'\'* ]]; then`,
			`problem=$(printf '%s' "$data" | jq -rs '`,
			`run_code=$(printf '%s' "$data" | jq -rs '.[0].run_code' 2>/dev/null) || problem=json`,
			`url=$(printf '%s' "$data" | jq -rs '.[0].gateway.url' 2>/dev/null) || problem=json`,
			`address=$(printf '%s' "$data" | jq -rs '.[0].gateway.address' 2>/dev/null) || problem=json`,
			`without_version=${data//\"version\"/}`,
			`if [[ $problem == ok ]] && { ((${#data} - ${#without_version} != 9)) || ! [[ $data =~ $version_text ]]; }; then`,
			`without_lifetime=${data//\"max_lifetime_minutes\"/}`,
			`if ((${#data} - ${#without_lifetime} == 22)) && [[ $data =~ $lifetime_text ]] &&`,
			`if [[ $problem == ok ]] && ! [[ $run_code =~ ^qecr_[0-9A-HJKMNP-TV-Z]{26}(\.[A-Za-z0-9_-]{22}){1,2}$ ]]; then`,
			`{ printf '%s\n' "$password"; printf '%s' "$daemon_base"; } | jq -nR '`,
			`input as $password |`,
			`"http-proxy": ("http://qory:" + $password + "@127.0.0.1:3129"),`,
			`"https-proxy": ("http://qory:" + $password + "@127.0.0.1:3129"),`,
			`printf '{"version": 1, "address": "127.0.0.1:3129", "password": "%s"}\n' "$password" 2>/dev/null >"$engine_proxy_json" ||`,
			`printf '%s\n' "${line//"$password"/[hidden]}" >&2 2>/dev/null || true`,
			// docker_log filters journal lines, and count_groups counts IPv6 groups. Neither
			// reads the run code file.
			`while IFS= read -r line || [[ -n $line ]]; do`,
			`lower=${line,,}`,
			`if [[ $lower == *qak_* || $lower == *qrk_* || $lower == *qecr_* ]]; then`,
			`if [[ -n $rest ]]; then`,
			`group=${rest%%:*}`,
			`[[ $rest == *:* ]] || break`,
			`rest=${rest#*:}`,
			`printf '%s' "$run_code" 2>/dev/null >"$run_code_file" || fail "Check that root can write $run_code_dir. Writing $run_code_file failed."`,
			// read_run_code_lines and its caller keep the content of the run code lines
			// file in variables. These lines compare it, test it with a pattern, take its
			// parts, count it, or append it to the run code file. None of them prints it.
			// The failure lines show a path, a line number and a fixed key name.
			`content=${content%x}`,
			`if [[ -z $content ]]; then`,
			`total=$((${#run_code} + 1 + ${#content}))`,
			`if [[ $content =~ [^[:print:]$'\n'] ]]; then`,
			`lower=${content,,}`,
			`if [[ $lower == *qecr_* || $lower == *qak_* || $lower == *qrk_* ]]; then`,
			`rest=${content%$'\n'}`,
			`line=${rest%%$'\n'*}`,
			`[[ -n $line ]] || fail "Remove the blank line, line $n, from $file. The run code file may have no blank line."`,
			`key=${line%% *}`,
			`[[ $line != *' '* ]] || value=${line#* }`,
			`if [[ $value =~ ^[A-Za-z0-9.-]+(:([0-9]{1,5}))?$ ]] && valid_port "${BASH_REMATCH[2]}" &&`,
			`[[ $value == *[.:]* || $value == localhost ]]; then`,
			`if [[ $value =~ ^(https?)://([A-Za-z0-9.-]+)(:([0-9]{1,5}))?$ ]]; then`,
			`if [[ $value == /* && ( $value == / || ( $value != *//* && $value != */ && $value != */. && $value != */.. && $value != */./* && $value != */../* ) ) ]]; then`,
			`[[ $rest == *$'\n'* ]] || break`,
			`rest=${rest#*$'\n'}`,
			`run_code_lines=$content`,
			// read_uptime reads the first field of /proc/uptime into value. It reads no
			// secret.
			`{ IFS=' ' read -r value rest <"$uptime_file"; } 2>/dev/null || [[ -n $value ]] || return 1`,
			`[[ $value =~ ^([0-9]{1,9})(\.([0-9]{1,6}))?$ ]] || return 1`,
			// The length of the engine proxy password, not the password.
			`[[ ${#password} -eq 43 ]] || fail "Check that root can read $urandom. Making the engine proxy password failed."`,
			`printf '\n%s' "$run_code_lines" 2>/dev/null >>"$run_code_file" ||`,
		},
		"qory-machine-data-aws-ec2": {
			`printf 'X-aws-ec2-metadata-token: %s\n' "$token" |`,
		},
	}
	secret := regexp.MustCompile(`\$\{?#?(token|data|run_code|password|content|run_code_lines|lower|rest|line|value)\b`)
	for name, lines := range allowed {
		for i, line := range strings.Split(read(t, name), "\n") {
			line = strings.TrimSpace(line)
			if secret.MatchString(line) && !slices.Contains(lines, line) {
				t.Errorf("%s:%d expands a secret in a line that is not reviewed: %s", name, i+1, line)
			}
		}
	}
}

// curlOptions are the options of both curl calls of qory-machine-data-aws-ec2.
const curlOptions = "curl -q --noproxy * --silent --fail --retry 5 --retry-all-errors --retry-delay 1 --connect-timeout 2 --max-time 5 "

// newEC2Env makes the root folder of newEnv without the start data folder, and returns
// it with the start data folder's path.
func newEC2Env(t *testing.T, path string) (*env, string) {
	t.Helper()
	e := newEnv(t, path)
	dir := filepath.Dir(e.data)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	return e, dir
}

// runEC2 runs qory-machine-data-aws-ec2 once. It checks what every run must leave: no
// stage line, and no token marker in the stubs' arguments, besides the checks of
// runScript.
func (e *env) runEC2(t *testing.T, sh string, vars ...string) result {
	t.Helper()
	r := e.runScript(t, sh, "qory-machine-data-aws-ec2", vars...)
	if len(r.stages) != 0 {
		t.Errorf("the script printed the stage lines %q", r.stages)
	}
	for _, line := range r.log {
		if !strings.HasPrefix(line, "curl-stdin ") && strings.Contains(line, token) {
			t.Errorf("a stub's arguments contain the token marker %s: %s", token, line)
		}
	}
	return r
}

// entries returns the names in the folder dir, or nil when dir does not exist.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range list {
		names = append(names, entry.Name())
	}
	return names
}

// TestDataUnitWritesStartData follows one boot on EC2: the script gets an IMDSv2 token,
// reads the user data with the token on stdin, and writes it to the start data file,
// mode 0600, in a folder of mode 0700, with no other file in the folder.
func TestDataUnitWritesStartData(t *testing.T) {
	sh := bash(t)
	path := commands(t, sh)
	for _, c := range []struct {
		name string
		// folder is the mode of a start data folder that exists before the run, or 0.
		folder fs.FileMode
	}{
		{name: "no folder"},
		{name: "a folder of mode 0755", folder: 0o755},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e, dir := newEC2Env(t, path)
			if c.folder != 0 {
				if err := os.Mkdir(dir, c.folder); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dir, c.folder); err != nil {
					t.Fatal(err)
				}
			}
			r := e.runEC2(t, sh)
			if r.status != 0 || r.stdout != "" || r.stderr != "" {
				t.Fatalf("status %d, stdout %q, stderr %q; want 0 and no output", r.status, r.stdout, r.stderr)
			}
			want := []string{
				curlOptions + "-X PUT -H X-aws-ec2-metadata-token-ttl-seconds: 60 http://169.254.169.254/latest/api/token",
				curlOptions + "-H @- http://169.254.169.254/latest/user-data",
				"curl-stdin X-aws-ec2-metadata-token: " + token,
			}
			if !slices.Equal(r.log, want) {
				t.Errorf("the commands are\n%s\nwant\n%s", strings.Join(r.log, "\n"), strings.Join(want, "\n"))
			}
			if got, want := read(t, e.data), read(t, e.userData); got != want {
				t.Errorf("the start data file is %q, want the user data %q", got, want)
			}
			if got := mode(t, e.data); got != 0o600 {
				t.Errorf("the start data file has mode %v, want 0600", got)
			}
			if got := mode(t, dir); got != fs.ModeDir|0o700 {
				t.Errorf("the start data folder has mode %v, want 0700", got)
			}
			if got := entries(t, dir); !slices.Equal(got, []string{"start-data"}) {
				t.Errorf("the start data folder contains %q, want only start-data", got)
			}
		})
	}
}

// TestDataUnitFails checks each failed step: the script prints one line to stderr and
// leaves no file in the start data folder that it did not find there.
func TestDataUnitFails(t *testing.T) {
	sh := bash(t)
	path := commands(t, sh)
	const (
		folder = "Remove {root}/run/qory-machine-start. The path is a link, not a folder, or a folder that another user owns."
		exists = "Remove {root}/run/qory-machine-start/start-data. The path exists before this unit writes it."
	)
	// toolDir is the tools folder of commands, without the stubs and so without curl.
	toolDir := filepath.SplitList(path)[1]
	for _, c := range []struct {
		name   string
		vars   []string
		status int
		want   string
		// path is PATH for the run, or "" for the stubs and the tools.
		path string
		// readOnly is a folder under the root that the script cannot write.
		readOnly string
		// setup changes the root folder and the start data folder dir before the run.
		setup func(t *testing.T, root, dir string)
		// left are the names in the start data folder after the run.
		left []string
		// notFolder is set when the start data folder path is no folder after the run.
		notFolder bool
		// check checks the root folder after the run.
		check func(t *testing.T, root string)
	}{
		{name: "token request fails", vars: []string{"STUB_FAIL_TOKEN=1"}, status: 1, want: "Turn on the instance metadata service with IMDSv2 for this machine. The token request to 169.254.169.254 failed after 5 retries.\n"},
		{name: "user data request fails", vars: []string{"STUB_FAIL_USERDATA=1"}, status: 1, want: "Launch this machine with the start data as its user data. The user data request to 169.254.169.254 failed after 5 retries.\n"},
		{name: "/run not writable", readOnly: "/run", status: 1, want: "Check that root can write {root}/run. Making {root}/run/qory-machine-start failed.\n"},
		{name: "SIGTERM", vars: []string{"STUB_TERM=1"}, status: 143, want: "No action needed. The script stopped on SIGTERM, and the machine powers off.\n"},
		{name: "no curl", path: toolDir, status: 1, want: "Install curl in the machine image. This script runs curl, and it is not on PATH.\n"},
		{
			name: "a folder at start-data", status: 1, want: exists + "\n", left: []string{"start-data"},
			setup: func(t *testing.T, _, dir string) {
				if err := os.MkdirAll(dir+"/start-data", 0o700); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, root string) {
				if got := entries(t, root+handOff); len(got) != 0 {
					t.Errorf("the folder at start-data contains %q, want no file", got)
				}
			},
		},
		{
			name: "a link to a folder at start-data", status: 1, want: exists + "\n", left: []string{"start-data"},
			setup: func(t *testing.T, root, dir string) {
				if err := os.Mkdir(root+"/elsewhere", 0o755); err != nil {
					t.Fatal(err)
				}
				link(t, root+"/elsewhere", dir+"/start-data")
			},
			check: func(t *testing.T, root string) {
				if got := entries(t, root+"/elsewhere"); len(got) != 0 {
					t.Errorf("the folder that the link points to contains %q, want no file", got)
				}
			},
		},
		{
			name: "a link to nothing at start-data", status: 1, want: exists + "\n", left: []string{"start-data"},
			setup: func(t *testing.T, root, dir string) { link(t, root+"/none", dir+"/start-data") },
		},
		{
			name: "a file at start-data", status: 1, want: exists + "\n", left: []string{"start-data"},
			setup: func(t *testing.T, _, dir string) {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				write(t, dir+"/start-data", "earlier data")
			},
			check: func(t *testing.T, root string) { fileStays(t, root+handOff, "earlier data") },
		},
		{
			name: "a file at the start data folder", status: 1, want: folder + "\n", notFolder: true,
			setup: func(t *testing.T, _, dir string) { write(t, dir, "not a folder") },
			check: func(t *testing.T, root string) { fileStays(t, filepath.Dir(root+handOff), "not a folder") },
		},
		{
			name: "a link to nothing at the start data folder", status: 1, want: folder + "\n",
			setup: func(t *testing.T, root, dir string) { link(t, root+"/none", dir) },
			check: func(t *testing.T, root string) { linkStays(t, root+"/none", filepath.Dir(root+handOff)) },
		},
		// The script writes nothing in the folder that the link points to, and leaves its
		// mode.
		{
			name: "the start data folder is a link", status: 1, want: folder + "\n",
			setup: func(t *testing.T, root, dir string) {
				if err := os.Mkdir(root+"/elsewhere", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(root+"/elsewhere", 0o755); err != nil {
					t.Fatal(err)
				}
				link(t, root+"/elsewhere", dir)
			},
			check: func(t *testing.T, root string) {
				if got := entries(t, root+"/elsewhere"); len(got) != 0 {
					t.Errorf("the folder that the link points to contains %q, want no file", got)
				}
				if got := mode(t, root+"/elsewhere"); got != fs.ModeDir|0o755 {
					t.Errorf("the folder that the link points to has mode %v, want 0755", got)
				}
			},
		},
		{
			name: "another user's folder", status: 1, want: folder + "\n",
			setup: func(t *testing.T, _, dir string) {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				otherOwnerFolder(t, dir)
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e, dir := newEC2Env(t, path)
			if c.path != "" {
				e.path = c.path
			}
			if c.readOnly != "" {
				readOnly(t, e.root+c.readOnly)
			}
			if c.setup != nil {
				c.setup(t, e.root, dir)
			}
			r := e.runEC2(t, sh, c.vars...)
			want := strings.ReplaceAll(c.want, "{root}", e.root)
			if r.status != c.status || r.stdout != "" || r.stderr != want {
				t.Errorf("status %d, stdout %q, stderr %q; want %d, no stdout, and stderr %q", r.status, r.stdout, r.stderr, c.status, want)
			}
			if !c.notFolder {
				if got := entries(t, dir); !slices.Equal(got, c.left) {
					t.Errorf("the start data folder contains %q after a failure, want %q", got, c.left)
				}
			}
			if c.check != nil {
				c.check(t, e.root)
			}
		})
	}
}
