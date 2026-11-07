package adapterio_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"

	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/integrations/contracts"
)

// TestMessagesFit checks that each fixed message of Serve, for the command with the
// longest name and with the rule texts of the decoders, has at most MaxMessage
// characters before Serve cuts it.
func TestMessagesFit(t *testing.T) {
	var messages []string
	add := func(e contracts.MachinesFailure) { messages = append(messages, e.Message) }

	_, out := serve(&handler{}, `{"version": 1}`)
	doc, err := adapterio.DecodeError([]byte(out))
	if err != nil {
		t.Fatalf("usage: %q: %v", out, err)
	}
	add(doc.Error)

	var stdout bytes.Buffer
	adapterio.Serve(context.Background(), []string{"delete", "--settings", "a.json"}, iotest.ErrReader(errors.New("read failed")), &stdout, &handler{})
	doc, err = adapterio.DecodeError(stdout.Bytes())
	if err != nil {
		t.Fatalf("unreadable request: %q: %v", stdout.String(), err)
	}
	add(doc.Error)

	deleteArgs := []string{"delete", "--settings", "a.json"}
	makeArgs := []string{"make", "--settings", "a.json"}
	add(errorOf(t, &handler{}, strings.Repeat(" ", contracts.MaxMachinesAnswer+1), deleteArgs...))
	for _, in := range []string{
		`{"version": 2, "id": "i-0123456789abcdef0"}`,
		`{"version": 1, "id": "i-0123456789abcdef0", "other": 1}`,
		`{"version": 1}`,
		`[]`,
	} {
		add(errorOf(t, &handler{}, in, deleteArgs...))
	}
	for _, in := range []string{
		`{"version": 1, "key": "dp_0123456789abcdef", "pool": "np_0123456789abcdef", "machine_spec": {"vcpus": 2, "memory_mib": 4096, "disk_gib": 30, "arch": "amd64", "virtualization": false}, "start_data": "AAE"}`,
		`{"version": 1, "key": "dp_iiiiiiiiiiiiiiii", "pool": "np_0123456789abcdef", "machine_spec": {"vcpus": 2, "memory_mib": 4096, "disk_gib": 30, "arch": "amd64", "virtualization": false}, "start_data": "AAEC"}`,
	} {
		add(errorOf(t, &handler{}, in, makeArgs...))
	}

	in := `{"version": 1, "key": "dp_0123456789abcdef", "pool": "np_0123456789abcdef", "machine_spec": {"vcpus": 2, "memory_mib": 4096, "disk_gib": 30, "arch": "amd64", "virtualization": false}, "start_data": "AAEC"}`
	add(errorOf(t, &handler{err: errors.New("Check the code qecr_abc")}, in, makeArgs...))
	add(errorOf(t, &handler{err: &adapterio.Failure{Kind: contracts.FailureOther}}, in, makeArgs...))

	logIn := `{"version": 1, "id": "i-0123456789abcdef0"}`
	logArgs := []string{"log", "--settings", "a.json"}
	add(errorOf(t, &handler{answer: contracts.MachinesLogAnswer{Log: strings.Repeat("a", adapterio.MaxLog+1)}}, logIn, logArgs...))
	add(errorOf(t, &handler{answer: contracts.MachinesLogAnswer{Log: strings.Repeat("\x01", adapterio.MaxLog)}}, logIn, logArgs...))

	for _, m := range messages {
		if n := utf8.RuneCountInString(m); n >= adapterio.MaxMessage {
			t.Errorf("%d characters, %d or more, so Serve may have cut it: %s", n, adapterio.MaxMessage, m)
		}
	}
}
