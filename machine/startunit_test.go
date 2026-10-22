// Package machine_test has the tests of the machine's start unit: qory-machine.service
// and the script qory-machine-start. The tests run the script under a temporary root
// folder, with stubs in place of every command that changes the computer.
package machine_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
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
	// forager is the machine's forager.yaml.
	forager = "machine: aws-ec2\nwall:\n  engine_proxy_file: /etc/qory/engine-proxy.json\n"
)

// dangerous are the commands that change the computer, start a service, or reach the
// network. The stub folder has a stub for each, and the test stops before it runs the
// script if any of them resolves outside the stub folder.
var dangerous = []string{
	"chgrp", "chown", "curl", "docker", "halt", "ip", "ip6tables", "iptables", "nft",
	"poweroff", "qory", "reboot", "runuser", "shutdown", "su", "sudo", "systemctl",
	"systemd-notify", "wget",
}

// tools are the real commands that the script and the stubs run. The tools folder has a
// link to each.
var tools = []string{"base64", "chmod", "head", "jq", "ln", "mkdir", "rm", "sed", "tr"}

// stub is the script behind every stub. It appends its name and arguments to
// $STUB_LOG, one line per call. The curl stub returns $STUB_TOKEN for the token PUT and
// the file $STUB_USERDATA for the user data, and logs the header line it reads on
// stdin. The runuser stub logs HOME and links the run code file to $STUB_KEEP, so the
// test can read it after the script removes it; with $STUB_TERM set, it sends SIGTERM
// to the script.
const stub = `
name=${0##*/}
{ printf '%s' "$name"; printf ' %s' "$@"; printf '\n'; } >>"$STUB_LOG"
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
		;;
	esac
	;;
iptables | ip6tables)
	[[ -z ${STUB_FAIL_IPTABLES:-} ]] || exit 4
	;;
systemctl)
	[[ $1 != start || -z ${STUB_FAIL_DOCKER:-} ]] || exit 1
	;;
systemd-notify | chown) ;;
runuser)
	printf 'runuser-home %s\n' "$HOME" >>"$STUB_LOG"
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
func bash(t *testing.T) string {
	t.Helper()
	path := "/opt/homebrew/bin/bash"
	if _, err := os.Stat(path); err != nil {
		if path, err = exec.LookPath("bash"); err != nil {
			t.Fatalf("install bash 5: %v", err)
		}
	}
	out, err := exec.Command(path, "-c", `printf %s "${BASH_VERSINFO[0]}"`).Output()
	if err != nil {
		t.Fatalf("install bash 5: %s: %v", path, err)
	}
	if string(out) < "5" || len(out) != 1 {
		t.Fatalf("install bash 5: %s is version %s", path, out)
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
// only them. It stops the test if a dangerous command resolves outside the stub folder.
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
			t.Fatalf("install %s: %v", name, err)
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

// env is one run of the script: its root folder, the files of the stubs, and PATH.
type env struct {
	root, userData, keep, log, path string
}

// newEnv makes a root folder with forager.yaml, /etc/hosts and /dev/urandom, and a user
// data file with valid start data.
func newEnv(t *testing.T, path string) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{
		root:     filepath.Join(dir, "root"),
		userData: filepath.Join(dir, "user-data"),
		keep:     filepath.Join(dir, "kept-run-code"),
		log:      filepath.Join(dir, "stub.log"),
		path:     path,
	}
	for _, d := range []string{"/home/qory/.config/qory", "/etc", "/run", "/dev"} {
		if err := os.MkdirAll(e.root+d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, e.root+"/home/qory/.config/qory/forager.yaml", forager)
	write(t, e.root+"/etc/hosts", "127.0.0.1 localhost\n")
	write(t, e.root+"/dev/urandom", string(randomBytes())+"more bytes after the 32 that head reads")
	write(t, e.userData, startData(runCode, gatewayURL, gatewayAddress))
	return e
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

func startData(code, url, address string) string {
	b, err := json.Marshal(map[string]any{
		"version":  1,
		"run_code": code,
		"gateway":  map[string]any{"url": url, "address": address},
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// result is what one run of the script left behind.
type result struct {
	status         int
	stdout, stderr string
	log            []string
}

// run runs the script once and checks what every run must leave: no secret marker in
// stdout, stderr or the stubs' arguments, no run code file, and systemctl poweroff as
// the last command.
func (e *env) run(t *testing.T, sh string, vars ...string) result {
	t.Helper()
	cmd := exec.Command(sh, "qory-machine-start")
	cmd.Env = append([]string{
		"PATH=" + e.path,
		"QORY_MACHINE_ROOT=" + e.root,
		"STUB_LOG=" + e.log,
		"STUB_TOKEN=" + token,
		"STUB_USERDATA=" + e.userData,
		"STUB_KEEP=" + e.keep,
	}, vars...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	r := result{}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		r.status = exit.ExitCode()
	}
	r.stdout, r.stderr = stdout.String(), stderr.String()
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
	if _, err := os.Lstat(e.root + "/run/qory-machine/run-code"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the run code file exists after the script exits: %v", err)
	}
	if got := r.log[len(r.log)-1]; got != "systemctl poweroff" {
		t.Errorf("the last command is %q, want systemctl poweroff", got)
	}
	return r
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

// TestStartUnitRunsQory follows one boot: the script reads the start data, drops the
// metadata service's packets, writes the hosts line and the engine proxy files, starts
// docker.service, runs qory with the run code file, and powers the machine off.
func TestStartUnitRunsQory(t *testing.T) {
	sh := bash(t)
	path := commands(t, sh)
	cases := []struct {
		name, forager, url, address, hostsLine, daemonJSON string
		wantDaemon                                         map[string]any
	}{
		{name: "IPv4", forager: forager, url: gatewayURL, address: gatewayAddress, hostsLine: "10.0.1.5 gateway.example.com"},
		{name: "IPv6, no port", forager: forager, url: "https://gateway.example.com", address: "fd00:ec2::5", hostsLine: "fd00:ec2::5 gateway.example.com"},
		{name: "IPv4 in IPv6", forager: forager, url: gatewayURL, address: "::ffff:10.0.1.5", hostsLine: "::ffff:10.0.1.5 gateway.example.com"},
		{name: "quoted machine", forager: "machine: \"aws-ec2\"  # the machine key\n", url: gatewayURL, address: gatewayAddress, hostsLine: "10.0.1.5 gateway.example.com"},
		{
			name: "empty daemon.json", forager: forager, url: gatewayURL, address: gatewayAddress, hostsLine: "10.0.1.5 gateway.example.com",
			daemonJSON: "\n",
		},
		{
			name: "existing daemon.json", forager: forager, url: gatewayURL, address: gatewayAddress, hostsLine: "10.0.1.5 gateway.example.com",
			daemonJSON: `{"log-driver": "journald", "proxies": {"http-proxy": "http://old.example.com"}}`,
			wantDaemon: map[string]any{"log-driver": "journald"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, path)
			write(t, e.root+"/home/qory/.config/qory/forager.yaml", c.forager)
			write(t, e.userData, startData(runCode, c.url, c.address))
			if c.daemonJSON != "" {
				if err := os.MkdirAll(e.root+"/etc/docker", 0o755); err != nil {
					t.Fatal(err)
				}
				write(t, e.root+"/etc/docker/daemon.json", c.daemonJSON)
			}
			r := e.run(t, sh)
			if r.status != 0 || r.stdout != "" || r.stderr != "" {
				t.Fatalf("status %d, stdout %q, stderr %q; want 0 and no output", r.status, r.stdout, r.stderr)
			}
			curl := "curl --silent --fail --retry 5 --retry-all-errors --retry-delay 1 --connect-timeout 2 --max-time 5 "
			want := []string{
				curl + "-X PUT -H X-aws-ec2-metadata-token-ttl-seconds: 60 http://169.254.169.254/latest/api/token",
				curl + "-H @- http://169.254.169.254/latest/user-data",
				"curl-stdin X-aws-ec2-metadata-token: " + token,
				"iptables -w -t raw -I PREROUTING -d 169.254.169.254 -j DROP",
				"ip6tables -w -t raw -I PREROUTING -d fd00:ec2::254 -j DROP",
				"iptables -w -t raw -I OUTPUT -d 169.254.169.254 -j DROP",
				"ip6tables -w -t raw -I OUTPUT -d fd00:ec2::254 -j DROP",
				"chown qory " + e.root + "/etc/qory/engine-proxy.json",
				"chown -R qory " + e.root + "/etc/docker/certs.d",
				"systemd-notify --ready",
				"systemctl start docker.service",
				"chown qory " + e.root + "/run/qory-machine/run-code " + e.root + "/run/qory-machine",
				"runuser -u qory -- qory run --run-code-file " + e.root + "/run/qory-machine/run-code --gateway " + c.url,
				"runuser-home " + e.root + "/home/qory",
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
		})
	}
}

// TestStartUnitFails checks each invalid input and each failed step: the script prints
// one line to stderr, runs no qory after an invalid input, and powers the machine off.
func TestStartUnitFails(t *testing.T) {
	sh := bash(t)
	path := commands(t, sh)
	const (
		json1       = "Launch this machine with start data that is one JSON object. The user data is not one JSON object."
		member      = "Remove the unknown members from the start data. Version 1 has only version, run_code and gateway, and gateway has only url and address."
		version     = "Set version to 1 in the start data. This script reads only start data version 1."
		code        = "Set run_code in the start data to the run code from Qory Apiary. The value does not have the form qecr_<26 characters>.<22 characters>, with an optional second .<22 characters>."
		gateway     = "Set gateway in the start data to an object with url and address. The gateway value is not an object."
		url         = "Set gateway.url in the start data to https://<host> or https://<host>:<port>. The value has another scheme, a user name, a path or a query, or its host or port is not valid."
		address     = "Set gateway.address in the start data to an IPv4 or IPv6 address. The value is not an IP address."
		foragerPath = "{root}/home/qory/.config/qory/forager.yaml"
	)
	good := func(code, url, address string) string { return startData(code, url, address) }
	cases := []struct {
		name, userData, forager string
		daemonJSON              string
		vars                    []string
		status                  int
		ran                     string // a command the script ran before it stopped, or ""
		want                    string
	}{
		{name: "not JSON", userData: `{"version": 1,`, want: json1},
		{name: "empty", userData: ``, want: json1},
		{name: "two objects", userData: good(runCode, gatewayURL, gatewayAddress) + good(runCode, gatewayURL, gatewayAddress), want: json1},
		{name: "an array", userData: `[1]`, want: json1},
		{name: "a lifetime", userData: `{"version": 1, "run_code": "` + runCode + `", "gateway": {"url": "https://gateway.example.com", "address": "10.0.1.5"}, "max_lifetime_minutes": 60}`, want: member},
		{name: "a gateway member", userData: `{"version": 1, "run_code": "` + runCode + `", "gateway": {"url": "https://gateway.example.com", "address": "10.0.1.5", "port": 1}}`, want: member},
		{name: "version 2", userData: `{"version": 2, "run_code": "` + runCode + `", "gateway": {"url": "https://gateway.example.com", "address": "10.0.1.5"}}`, want: version},
		{name: "version as text", userData: `{"version": "1", "run_code": "` + runCode + `", "gateway": {"url": "https://gateway.example.com", "address": "10.0.1.5"}}`, want: version},
		{name: "no version", userData: `{"run_code": "` + runCode + `", "gateway": {"url": "https://gateway.example.com", "address": "10.0.1.5"}}`, want: version},
		{name: "no run code", userData: `{"version": 1, "gateway": {"url": "https://gateway.example.com", "address": "10.0.1.5"}}`, want: code},
		{name: "run code as a number", userData: `{"version": 1, "run_code": 5, "gateway": {"url": "https://gateway.example.com", "address": "10.0.1.5"}}`, want: code},
		{name: "run code in lower case", userData: good(strings.ToLower(runCode), gatewayURL, gatewayAddress), want: code},
		{name: "run code with I", userData: good("qecr_IARKER00000000000000000000.MARKERruncode000000000", gatewayURL, gatewayAddress), want: code},
		{name: "run code with three parts", userData: good(runCode+".MARKERruncode000000000.MARKERruncode000000000", gatewayURL, gatewayAddress), want: code},
		{name: "run code too short", userData: good(runCode[:len(runCode)-1], gatewayURL, gatewayAddress), want: code},
		{name: "run code with a newline", userData: good(runCode+"\n", gatewayURL, gatewayAddress), want: code},
		{name: "gateway as text", userData: `{"version": 1, "run_code": "` + runCode + `", "gateway": "https://gateway.example.com"}`, want: gateway},
		{name: "URL with a path", userData: good(runCode, gatewayURL+"/v1", gatewayAddress), want: url},
		{name: "URL with a slash", userData: good(runCode, gatewayURL+"/", gatewayAddress), want: url},
		{name: "URL with a query", userData: good(runCode, gatewayURL+"?a=1", gatewayAddress), want: url},
		{name: "URL with a fragment", userData: good(runCode, gatewayURL+"#a", gatewayAddress), want: url},
		{name: "URL with a user", userData: good(runCode, "https://user:MARKERruncode@gateway.example.com", gatewayAddress), want: url},
		{name: "URL over http", userData: good(runCode, "http://gateway.example.com", gatewayAddress), want: url},
		{name: "URL with port 0", userData: good(runCode, "https://gateway.example.com:0", gatewayAddress), want: url},
		{name: "URL with port 70000", userData: good(runCode, "https://gateway.example.com:70000", gatewayAddress), want: url},
		{name: "URL with no host", userData: good(runCode, "https://:8443", gatewayAddress), want: url},
		{name: "URL with an IPv6 host", userData: good(runCode, "https://[fd00::1]:8443", gatewayAddress), want: url},
		{name: "URL with a space", userData: good(runCode, "https://gateway example.com", gatewayAddress), want: url},
		{name: "address as a name", userData: good(runCode, gatewayURL, "gateway.example.com"), want: address},
		{name: "address over 255", userData: good(runCode, gatewayURL, "10.0.1.256"), want: address},
		{name: "address with 3 parts", userData: good(runCode, gatewayURL, "10.0.1"), want: address},
		{name: "address with a leading zero", userData: good(runCode, gatewayURL, "010.0.1.5"), want: address},
		{name: "address with a zone", userData: good(runCode, gatewayURL, "fd00::1%eth0"), want: address},
		{name: "address with two ::", userData: good(runCode, gatewayURL, "fd00::1::2"), want: address},
		{name: "address with 9 groups", userData: good(runCode, gatewayURL, "1:2:3:4:5:6:7:8:9"), want: address},
		{name: "empty address", userData: good(runCode, gatewayURL, ""), want: address},
		{name: "another machine", forager: "machine: gcp-compute\n", want: "Set machine: aws-ec2 in " + foragerPath + ". This script reads start data only for aws-ec2, and the file sets machine to gcp-compute."},
		{name: "no machine key", forager: "wall:\n  engine_proxy_file: /etc/qory/engine-proxy.json\n", want: "Add the line machine: aws-ec2 to " + foragerPath + ". The file has no machine key."},
		{name: "two machine keys", forager: "machine: aws-ec2\nmachine: aws-ec2\n", want: "Keep one machine line in " + foragerPath + ". The file has more than one."},
		{name: "no forager.yaml", forager: "-", want: "Write machine: aws-ec2 into " + foragerPath + ". The file does not exist or is not readable."},
		{name: "token request fails", vars: []string{"STUB_FAIL_TOKEN=1"}, want: "Turn on the instance metadata service with IMDSv2 for this machine. The token request to 169.254.169.254 failed after 5 retries."},
		{name: "user data request fails", vars: []string{"STUB_FAIL_USERDATA=1"}, want: "Launch this machine with the start data as its user data. The user data request to 169.254.169.254 failed after 5 retries."},
		{name: "iptables fails", vars: []string{"STUB_FAIL_IPTABLES=1"}, ran: "iptables -w -t raw -I PREROUTING -d 169.254.169.254 -j DROP", want: "Check that iptables can add rules to the raw table on this machine image. Adding the PREROUTING rule that drops packets to 169.254.169.254 failed."},
		{name: "daemon.json is not an object", daemonJSON: `["not an object"]`, ran: "iptables -w -t raw -I OUTPUT -d 169.254.169.254 -j DROP", want: "Make {root}/etc/docker/daemon.json in the machine image one JSON object, or remove it. Writing the engine proxy into it failed."},
		{name: "docker.service fails", vars: []string{"STUB_FAIL_DOCKER=1"}, ran: "systemctl start docker.service", want: "Read the journal of docker.service for the cause. Starting docker.service failed."},
		{name: "SIGTERM", vars: []string{"STUB_TERM=1"}, status: 143, ran: "runuser-home {root}/home/qory", want: "No action needed. The script stopped on SIGTERM, and the machine powers off."},
		{name: "qory run fails", vars: []string{"STUB_QORY_EXIT=3"}, status: 3, ran: "runuser-home {root}/home/qory", want: "Read the lines of qory run in the journal of qory-machine.service for the cause. qory run exited with status 3."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, path)
			if c.userData != "" || c.name == "empty" {
				write(t, e.userData, c.userData)
			}
			switch c.forager {
			case "":
			case "-":
				if err := os.Remove(e.root + "/home/qory/.config/qory/forager.yaml"); err != nil {
					t.Fatal(err)
				}
			default:
				write(t, e.root+"/home/qory/.config/qory/forager.yaml", c.forager)
			}
			if c.daemonJSON != "" {
				if err := os.MkdirAll(e.root+"/etc/docker", 0o755); err != nil {
					t.Fatal(err)
				}
				write(t, e.root+"/etc/docker/daemon.json", c.daemonJSON)
			}
			r := e.run(t, sh, c.vars...)
			status := c.status
			if status == 0 {
				status = 1
			}
			if r.status != status {
				t.Errorf("status %d, want %d", r.status, status)
			}
			want := strings.ReplaceAll(c.want, "{root}", e.root) + "\n"
			if r.stderr != want || r.stdout != "" {
				t.Errorf("stdout %q, stderr %q; want no stdout and stderr\n%q", r.stdout, r.stderr, want)
			}
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
		})
	}
}

// TestStartUnitFiles checks the unit file, and the lines that neither file may contain.
func TestStartUnitFiles(t *testing.T) {
	unit := read(t, "qory-machine.service")
	for _, line := range []string{
		"Wants=network-online.target",
		"After=network-online.target",
		"Before=docker.service",
		"Type=notify",
		"NotifyAccess=all",
		"ExecStart=/usr/local/libexec/qory/qory-machine-start",
		"WantedBy=multi-user.target",
	} {
		if !slices.Contains(strings.Split(unit, "\n"), line) {
			t.Errorf("qory-machine.service has no line %s", line)
		}
	}
	script := read(t, "qory-machine-start")
	if !strings.HasPrefix(script, "#!/bin/bash\n") || !strings.Contains(script, "\nset -euo pipefail\n") {
		t.Error("qory-machine-start does not start with #!/bin/bash and set -euo pipefail")
	}
	if got := mode(t, "qory-machine-start"); got&0o111 == 0 {
		t.Errorf("qory-machine-start has mode %v, want an executable", got)
	}
	// An absolute command path would bypass the stubs on PATH.
	absolute := regexp.MustCompile(`(^|[\s;|&(])/(usr/)?s?bin/`)
	forbidden := regexp.MustCompile(`(?i)set -x|set -o xtrace|shutdown|max_lifetime`)
	for name, text := range map[string]string{"qory-machine.service": unit, "qory-machine-start": script} {
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

// TestStartUnitSecretLines pins every line of the script that expands the token, the
// start data, the run code or the password. The runs in the other tests observe the
// arguments of the stubs only, so this list covers the arguments of the real tools: each
// line passes the secret to a builtin printf, a pipe, a file or a [[ test, or is inside a
// jq program. A new line that expands a secret fails this test until it is reviewed and
// added here.
func TestStartUnitSecretLines(t *testing.T) {
	allowed := []string{
		`data=$(printf 'X-aws-ec2-metadata-token: %s\n' "$token" |`,
		`problem=$(printf '%s' "$data" | jq -rs '`,
		`run_code=$(printf '%s' "$data" | jq -rs '.[0].run_code' 2>/dev/null) || problem=json`,
		`url=$(printf '%s' "$data" | jq -rs '.[0].gateway.url' 2>/dev/null) || problem=json`,
		`address=$(printf '%s' "$data" | jq -rs '.[0].gateway.address' 2>/dev/null) || problem=json`,
		`if [[ $problem == ok ]] && ! [[ $run_code =~ ^qecr_[0-9A-HJKMNP-TV-Z]{26}(\.[A-Za-z0-9_-]{22}){1,2}$ ]]; then`,
		`{ printf '%s\n' "$password"; printf '%s' "$daemon_base"; } | jq -nR '`,
		`input as $password |`,
		`"http-proxy": ("http://qory:" + $password + "@127.0.0.1:3129"),`,
		`"https-proxy": ("http://qory:" + $password + "@127.0.0.1:3129"),`,
		`printf '{"version": 1, "address": "127.0.0.1:3129", "password": "%s"}\n' "$password" >"$engine_proxy_json" 2>/dev/null ||`,
		`printf '%s' "$run_code" >"$run_code_file" 2>/dev/null || fail "Check that root can write $run_code_dir. Writing $run_code_file failed."`,
	}
	secret := regexp.MustCompile(`\$\{?(token|data|run_code|password)\b`)
	for i, line := range strings.Split(read(t, "qory-machine-start"), "\n") {
		line = strings.TrimSpace(line)
		if secret.MatchString(line) && !slices.Contains(allowed, line) {
			t.Errorf("qory-machine-start:%d expands a secret in a line that is not reviewed: %s", i+1, line)
		}
	}
}
