package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qoryai/hive/machines"
)

// fakeSwitch makes the test binary act as a fake machine adapter. Its name does not
// start with QORY_, so the runner passes it to the adapter.
const fakeSwitch = "MACHINE_ADAPTER_TEST_FAKE"

func TestMain(m *testing.M) {
	if os.Getenv(fakeSwitch) == "1" {
		os.Exit(fake(os.Args[1:]))
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

// config is the fake adapter's settings file.
type config struct {
	// Mode is serve, fail, raw, flood, biglog, hang, sleep, daemon, orphan or signal.
	// Mode daemon leaves a child that holds standard output; mode orphan leaves a
	// child that does not. Both write Stdout and exit with status 0.
	Mode string `json:"mode"`
	// Report is the file to which the fake writes what it received.
	Report string `json:"report,omitempty"`
	// StartDataSHA256 is the hex SHA-256 of the start data that the fake expects in a
	// make request, so the settings file never contains the start data.
	StartDataSHA256 string `json:"start_data_sha256,omitempty"`
	// Kind and Message are the failure of mode fail. The Message echo writes the start
	// data, and echo64 writes it as base64.
	Kind    machines.Kind `json:"kind,omitempty"`
	Message string        `json:"message,omitempty"`
	// Stdout and Status are the output and the exit status of mode raw.
	Stdout string `json:"stdout,omitempty"`
	Status int    `json:"status,omitempty"`
	// PIDFile is where mode sleep writes its process id.
	PIDFile string `json:"pid_file,omitempty"`
}

// report is what the fake received.
type report struct {
	Args           []string `json:"args"`
	QoryEnv        []string `json:"qory_env"`
	KeptEnv        bool     `json:"kept_env"`
	StartDataMatch bool     `json:"start_data_match"`
}

// fake runs as the adapter: args are <command> --settings FILE.
func fake(args []string) int {
	if len(args) != 3 {
		return 9
	}
	b, err := os.ReadFile(args[2])
	if err != nil {
		return 9
	}
	var cfg config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return 9
	}
	switch cfg.Mode {
	case "sleep":
		_ = os.WriteFile(cfg.PIDFile, []byte(strconv.Itoa(os.Getpid())), 0o600)
		time.Sleep(10 * time.Minute)
		return 0
	case "hang", "daemon", "orphan":
		child := config{Mode: "sleep", PIDFile: cfg.PIDFile}
		cb, _ := json.Marshal(child)
		path := args[2] + ".child"
		if err := os.WriteFile(path, cb, 0o600); err != nil {
			return 9
		}
		cmd := exec.Command(os.Args[0], "sleep", "--settings", path)
		if cfg.Mode == "daemon" {
			cmd.Stdout = os.Stdout
		}
		if err := cmd.Start(); err != nil {
			return 9
		}
		for range 200 {
			if _, err := os.Stat(cfg.PIDFile); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if cfg.Mode == "daemon" || cfg.Mode == "orphan" {
			_, _ = io.ReadAll(os.Stdin)
			_, _ = os.Stdout.WriteString(cfg.Stdout)
			return 0
		}
		time.Sleep(10 * time.Minute)
		return 0
	case "signal":
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		time.Sleep(time.Minute)
		return 0
	case "flood":
		chunk := bytes.Repeat([]byte("a"), 64*1024)
		for {
			if _, err := os.Stdout.Write(chunk); err != nil {
				return 0
			}
		}
	case "biglog":
		_, _ = io.ReadAll(os.Stdin)
		doc, _ := machines.Encode(machines.LogAnswer{Log: strings.Repeat("\x00", machines.MaxLog)})
		_, _ = os.Stdout.Write(doc)
		return 0
	case "raw":
		_, _ = io.ReadAll(os.Stdin)
		_, _ = os.Stdout.WriteString(cfg.Stdout)
		return cfg.Status
	}

	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 9
	}
	rep := report{Args: args}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "QORY_") {
			rep.QoryEnv = append(rep.QoryEnv, strings.SplitN(kv, "=", 2)[0])
		}
		if kv == "ADAPTER_TEST_KEEP=1" {
			rep.KeptEnv = true
		}
	}
	if req, err := machines.DecodeMakeRequest(stdin); err == nil {
		sum := sha256.Sum256(req.StartData)
		rep.StartDataMatch = hex.EncodeToString(sum[:]) == cfg.StartDataSHA256
	}
	if cfg.Report != "" {
		rb, _ := json.Marshal(rep)
		if err := os.WriteFile(cfg.Report, rb, 0o600); err != nil {
			return 9
		}
	}
	fmt.Fprintf(os.Stderr, "fake adapter: %s ran\n", args[0])
	return machines.Serve(context.Background(), args, bytes.NewReader(stdin), os.Stdout, &handler{cfg: cfg})
}

// handler is the fake adapter's [machines.Handler].
type handler struct{ cfg config }

func (h *handler) err(req []byte) error {
	if h.cfg.Mode != "fail" {
		return nil
	}
	msg := h.cfg.Message
	switch msg {
	case "echo":
		msg = "Check the start data " + string(req)
	case "echo64":
		msg = "Check the start data " + base64.StdEncoding.EncodeToString(req)
	}
	return &machines.Error{Kind: h.cfg.Kind, Message: msg}
}

func (h *handler) Check(context.Context, string, machines.CheckRequest) (machines.CheckAnswer, error) {
	return machines.CheckAnswer{}, h.err(nil)
}

// machines reads the fake's machines, by key, from the file beside its report.
func (h *handler) machines() map[string]string {
	m := map[string]string{}
	if b, err := os.ReadFile(h.cfg.Report + ".machines"); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// Make makes at most one machine per key, and knows the one size standard.
func (h *handler) Make(_ context.Context, _ string, req machines.MakeRequest) (machines.MakeAnswer, error) {
	if err := h.err(req.StartData); err != nil {
		return machines.MakeAnswer{}, err
	}
	if req.Size != "standard" {
		return machines.MakeAnswer{}, &machines.Error{Kind: machines.KindDefect, Message: "Add the size to the adapter's settings: the adapter has no size of this name"}
	}
	m := h.machines()
	if id, ok := m[req.Key]; ok {
		return machines.MakeAnswer{ID: id}, nil
	}
	id := fmt.Sprintf("i-%017x", len(m)+1)
	m[req.Key] = id
	b, _ := json.Marshal(m)
	if err := os.WriteFile(h.cfg.Report+".machines", b, 0o600); err != nil {
		return machines.MakeAnswer{}, err
	}
	return machines.MakeAnswer{ID: id}, nil
}

func (h *handler) List(_ context.Context, _ string, req machines.ListRequest) (machines.ListAnswer, error) {
	m := machines.Machine{ID: "i-0123456789abcdef0", Key: testKey, State: machines.StateRunning, Started: time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)}
	return machines.ListAnswer{Machines: []machines.Machine{m}}, h.err(nil)
}

func (h *handler) Log(context.Context, string, machines.LogRequest) (machines.LogAnswer, error) {
	return machines.LogAnswer{Log: "boot\n"}, h.err(nil)
}

// Delete returns deleting for a machine that the fake made, and gone for any other id.
func (h *handler) Delete(_ context.Context, _ string, req machines.DeleteRequest) (machines.DeleteAnswer, error) {
	if err := h.err(nil); err != nil {
		return machines.DeleteAnswer{}, err
	}
	for _, id := range h.machines() {
		if id == req.ID {
			return machines.DeleteAnswer{State: machines.StateDeleting}, nil
		}
	}
	return machines.DeleteAnswer{State: machines.StateGone}, nil
}
