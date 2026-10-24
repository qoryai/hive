package awsec2

import (
	"context"
	"io/fs"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/integrations/contracts"
)

// wantFits fails t unless message has at most adapterio.MaxMessage characters.
func wantFits(t *testing.T, message string) {
	t.Helper()
	if n := utf8.RuneCountInString(message); n > adapterio.MaxMessage {
		t.Errorf("%d characters, more than %d: %s", n, adapterio.MaxMessage, message)
	}
}

// TestMessagesFit checks that each fixed message of this package, with typical values,
// fits in adapterio.MaxMessage: a settings path of 60 characters, which comes last,
// and a size name of 40 characters. Package machine tests its messages in
// messages_test.go.
func TestMessagesFit(t *testing.T) {
	const ok = `"subnet_id": "subnet-0123456789abcdef0", "security_group_id": "sg-0123456789abcdef0", "image_id": "ami-0123456789abcdef0"`
	for _, doc := range []string{
		``,
		`[]`,
		`{"subnet_id": "subnet-01234567",}`,
		`{"subnet_id": "subnet-01234567", "subnet_id": "subnet-01234567"}`,
		`{"other": 1}`,
		`{}`,
		`{"subnet_id": "x", "security_group_id": "x", "image_id": "x", "sizes": {}}`,
		`{"subnet_id": "subnet-0123456789abcdef0", "security_group_id": "x", "image_id": "x", "sizes": {}}`,
		`{"subnet_id": "subnet-0123456789abcdef0", "security_group_id": "sg-0123456789abcdef0", "image_id": "x", "sizes": {}}`,
		`{` + ok + `, "sizes": {}}`,
		withTags(`[]`),
		withTags(`{"!": ""}`),
		withTags(`{"aws:a": ""}`),
		withTags(`{"qory:key": ""}`),
	} {
		_, err := ParseSettings([]byte(doc))
		if err == nil {
			t.Fatalf("%q: no error", doc)
		}
		wantFits(t, err.Error())
	}

	_, err := ReadSettings(writeSettings(t, strings.Repeat(" ", maxFile+1)))
	wantFits(t, err.Error())

	path := "/etc/qory/provisioner/adapters/machine/settings/aws-ec2.json"
	if len(path) != 60 {
		t.Fatalf("the path has %d characters, want 60", len(path))
	}
	for _, e := range []struct {
		op  string
		err error
	}{
		{"opening", &fs.PathError{Op: "open", Path: path, Err: syscall.ENOENT}},
		{"opening", &fs.PathError{Op: "open", Path: path, Err: syscall.EACCES}},
		{"reading", &fs.PathError{Op: "read", Path: path, Err: syscall.EISDIR}},
	} {
		m := fileError(e.err, e.op).Error()
		if !strings.HasSuffix(m, ": "+path) {
			t.Errorf("the message does not end with the path: %s", m)
		}
		wantFits(t, m)
	}

	h, _, _ := testHandler(&fakeAPI{})
	size := "s" + strings.Repeat("-", 39)
	_, err = h.Make(context.Background(), writeSettings(t, validSettings), contracts.MachinesMake{Version: 1, Key: "dp_0123456789abcdef", Pool: "np_0123456789abcdef", Size: size}, []byte("x"))
	if err == nil || !strings.Contains(err.Error(), size) {
		t.Fatalf("make with an unknown size: error %v", err)
	}
	wantFits(t, err.Error())
}
