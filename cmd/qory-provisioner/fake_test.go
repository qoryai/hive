package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fakeSwitch makes the test binary act as a fake machine adapter. Its name does not
// start with QORY_, so the adapter runner passes it to the adapter.
const fakeSwitch = "PROVISIONER_TEST_FAKE_ADAPTER"

func TestMain(m *testing.M) {
	if os.Getenv(fakeSwitch) == "1" {
		os.Exit(fakeAdapter(os.Args[1:], os.Stdin, os.Stdout))
	}
	if err := os.Setenv(fakeSwitch, "1"); err != nil {
		panic(err)
	}
	// Under -race, a process that exits with status 0 waits 1 s by default.
	if err := os.Setenv("GORACE", "atexit_sleep_ms=0"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// fakeSettings is the fake adapter's settings file: the folder in which it keeps its
// machines and its records.
type fakeSettings struct {
	Dir string `json:"dir"`
}

// The files in the fake adapter's folder.
const (
	// fakeCommands has one line per command the fake ran: its name and its arguments.
	fakeCommands = "commands"
	// fakeMachines is the fake's machines, a JSON object of the key to the machine id.
	fakeMachines = "machines.json"
	// fakeMade is the record of the last make, see [made].
	fakeMade = "made.json"
)

// made is what the fake adapter recorded of a make request. It contains the SHA-256 of
// the run code in the start data, never the run code.
type made struct {
	Key                string `json:"key"`
	Pool               string `json:"pool"`
	Size               string `json:"size"`
	RunCodeSHA256      string `json:"run_code_sha256"`
	GatewayURL         string `json:"gateway_url"`
	GatewayAddress     string `json:"gateway_address"`
	MaxLifetimeMinutes int    `json:"max_lifetime_minutes"`
}

// fakeAdapter runs as the machine adapter: args are <command> --settings FILE. It
// answers check, make, list, log and delete as version 1 of the protocol, keeps its
// machines in a file, and writes no line on standard error.
func fakeAdapter(args []string, stdin io.Reader, stdout io.Writer) int {
	if len(args) != 3 || args[1] != "--settings" {
		return 9
	}
	b, err := os.ReadFile(args[2])
	if err != nil {
		return 9
	}
	var s fakeSettings
	if err := json.Unmarshal(b, &s); err != nil {
		return 9
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, fakeCommands), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return 9
	}
	fmt.Fprintln(f, strings.Join(args, " "))
	f.Close()
	req, err := io.ReadAll(stdin)
	if err != nil {
		return 9
	}
	var answer any
	switch args[0] {
	case "check":
		answer = map[string]any{}
	case "make":
		answer, err = fakeMake(s.Dir, req)
	case "list":
		list := []map[string]string{}
		machines := fakeRead(s.Dir)
		keys := make([]string, 0, len(machines))
		for k := range machines {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			list = append(list, map[string]string{"id": machines[k], "key": k, "state": "running", "started": "2026-10-22T10:00:00Z"})
		}
		answer = map[string]any{"machines": list}
	case "log":
		answer = map[string]string{"log": "boot\n"}
	case "delete":
		answer = map[string]string{"state": "deleting"}
	default:
		return 9
	}
	if err != nil {
		return 9
	}
	if err := json.NewEncoder(stdout).Encode(answer); err != nil {
		return 9
	}
	return 0
}

// fakeRead reads the fake adapter's machines in dir.
func fakeRead(dir string) map[string]string {
	m := map[string]string{}
	if b, err := os.ReadFile(filepath.Join(dir, fakeMachines)); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// fakeMake makes one machine per key, records the request in [fakeMade], and returns
// the make answer.
func fakeMake(dir string, req []byte) (any, error) {
	var r struct {
		Key       string `json:"key"`
		Pool      string `json:"pool"`
		Size      string `json:"size"`
		StartData []byte `json:"start_data"`
	}
	if err := json.Unmarshal(req, &r); err != nil {
		return nil, err
	}
	var data struct {
		RunCode string `json:"run_code"`
		Gateway struct {
			URL     string `json:"url"`
			Address string `json:"address"`
		} `json:"gateway"`
		MaxLifetimeMinutes int `json:"max_lifetime_minutes"`
	}
	if err := json.Unmarshal(r.StartData, &data); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(data.RunCode))
	rec, err := json.Marshal(made{
		Key: r.Key, Pool: r.Pool, Size: r.Size, RunCodeSHA256: hex.EncodeToString(sum[:]),
		GatewayURL: data.Gateway.URL, GatewayAddress: data.Gateway.Address, MaxLifetimeMinutes: data.MaxLifetimeMinutes,
	})
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, fakeMade), rec, 0o600); err != nil {
		return nil, err
	}
	machines := fakeRead(dir)
	id, ok := machines[r.Key]
	if !ok {
		id = fmt.Sprintf("i-%017x", len(machines)+1)
		machines[r.Key] = id
		b, err := json.Marshal(machines)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, fakeMachines), b, 0o600); err != nil {
			return nil, err
		}
	}
	return map[string]string{"id": id}, nil
}
