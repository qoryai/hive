package enrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/qoryai/forager/accesskey"
	"github.com/qoryai/hive/internal/state"
)

// codeCore is the 26 characters of the enrolment codes the tests use: a marker that no
// log line, error or file may contain.
const codeCore = "0123456789ABCDEFGHJKMNPQRS"

// poolID is the node pool the fake enrols a key in.
const poolID = "np_0123456789abcdef"

// fakeApiary is Qory Apiary's enrolment endpoint.
type fakeApiary struct {
	url      string
	signer   *accesskey.Key
	requests atomic.Int32
	got      atomic.Pointer[accesskey.EnrolmentRequest]
}

// reply is how the fake answers: a 201 enrols the access key ak_0123456789abcdef in the
// node nodeID of kind; a 401 is unsigned and empty; unsigned is a 201 without a
// signature; any other status is a refusal with the code refusal. Each but the 401 and
// unsigned is signed under the fake's key.
type reply struct {
	status       int
	kind, nodeID string
	refusal      string
	unsigned     bool
}

// poolEnrolled is a reply that enrols the key in a node pool.
var poolEnrolled = reply{status: http.StatusCreated, kind: "pool", nodeID: poolID}

func newFakeApiary(t *testing.T, rp reply) *fakeApiary {
	t.Helper()
	signer, err := accesskey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeApiary{signer: signer}
	listed := accesskey.Pin{{Alg: "ed25519", PublicKey: signer.PublicKey().String()}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != accesskey.EnrolmentPath {
			http.NotFound(w, r)
			return
		}
		var req accesskey.EnrolmentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !req.VerifyProof() {
			t.Errorf("the enrolment request does not verify: %v", err)
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.got.Store(&req)
		var doc any
		switch rp.status {
		case http.StatusCreated:
			doc = map[string]any{"version": 1, "access_key_id": "ak_0123456789abcdef", "node_id": rp.nodeID, "node_kind": rp.kind, "stored_secrets": false, "apiary_public_key": listed}
		case http.StatusUnauthorized:
			w.WriteHeader(rp.status)
			return
		default:
			doc = map[string]any{"error": rp.refusal, "apiary_public_key": listed}
		}
		body, _ := json.Marshal(doc)
		if !rp.unsigned {
			w.Header().Set(accesskey.HeaderSignature, signer.SignAnswer(accesskey.Answer{Enrolment: true, Status: rp.status, RequestSignature: req.Proof, Body: body}))
		}
		w.WriteHeader(rp.status)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

// codeOf is an enrolment code with prefix that carries the fingerprint of key, as a
// person copies it.
func codeOf(prefix string, key *accesskey.Key) string {
	return prefix + codeCore[:13] + "-" + codeCore[13:] + "." + key.Fingerprint()
}

// poolCode is a node pool's enrolment code for the fake.
func poolCode(f *fakeApiary) string { return codeOf(accesskey.CodePrefixPool, f.signer) }

// run is one start of the provisioner's enrolment against the fake f, with code in
// QORY_ENROLMENT_CODE, in the state folder dir. It returns the state, the error, the
// log, and the key it made, nil when it made none.
type run struct {
	st   state.State
	err  error
	log  string
	made *accesskey.Key
}

func start(t *testing.T, f *fakeApiary, dir, code string) run {
	t.Helper()
	return startWith(t, context.Background(), f, dir, code, nil)
}

// startWith is start with ctx, and with the Config changed by set when set is not nil.
func startWith(t *testing.T, ctx context.Context, f *fakeApiary, dir, code string, set func(*Config)) run {
	t.Helper()
	var logs bytes.Buffer
	var r run
	c := Config{
		Dir:       dir,
		ApiaryURL: f.url,
		Version:   "dev",
		Log:       slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Getenv: func(name string) string {
			if name == Variable {
				return code
			}
			return ""
		},
		GenerateKey: func() (*accesskey.Key, error) {
			k, err := accesskey.Generate()
			r.made = k
			return k, err
		},
		Hostname: func() (string, error) { return "provisioner-host.example.com", nil },
	}
	if set != nil {
		set(&c)
	}
	r.st, r.err = Start(ctx, c)
	r.log = logs.String()
	return r
}

// stateDir is a state folder that does not exist yet.
func stateDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "qory-provisioner")
}

// noLeak fails the test when the log, the error, or a file in the state folder other
// than the key file contains the secret of key, the code, or a part of the code.
func noLeak(t *testing.T, r run, dir, code string) {
	t.Helper()
	secrets := []string{code, codeCore, codeCore[:13], codeCore[13:], accesskey.SecretPrefix}
	if r.made != nil {
		secrets = append(secrets, r.made.Secret())
	}
	outputs := map[string]string{"the log": r.log}
	if r.err != nil {
		outputs["the error"] = r.err.Error()
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == state.KeyFile || e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		outputs[e.Name()] = string(b)
	}
	for where, o := range outputs {
		for _, s := range secrets {
			if s != "" && strings.Contains(o, s) {
				t.Errorf("%s contains a secret or the code: %q", where, o)
			}
		}
	}
}

// noKey fails the test when the state folder dir has a key file.
func noKey(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dir, state.KeyFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a key file was written: %v", err)
	}
}

// TestStartEnrolsAndWritesTheState pins the first start: the code enrols a key made in
// the process, named after the host, and the state folder then holds the key, the
// enrolment with the pin and the pool, and the instance id. A second start reads the
// same state and sends nothing.
func TestStartEnrolsAndWritesTheState(t *testing.T) {
	f := newFakeApiary(t, poolEnrolled)
	dir := stateDir(t)
	code := poolCode(f)
	r := start(t, f, dir, code)
	if r.err != nil {
		t.Fatal(r.err)
	}
	got := f.got.Load()
	if f.requests.Load() != 1 || got == nil || got.Name != "provisioner-host.example.com" || got.PublicKey != r.made.PublicKey().String() {
		t.Fatalf("%d requests: %+v", f.requests.Load(), got)
	}
	if r.st.Key.Secret() != r.made.Secret() || r.st.AccessKeyID != "ak_0123456789abcdef" || r.st.PoolID != poolID || r.st.ApiaryURL != f.url ||
		len(r.st.Pin) != 1 || r.st.Pin[0].PublicKey != f.signer.PublicKey().String() || accesskey.CheckInstanceID(r.st.InstanceID) != nil {
		t.Errorf("%+v", r.st)
	}
	b, err := os.ReadFile(filepath.Join(dir, state.KeyFile))
	if err != nil || string(b) != r.made.Secret()+"\n" {
		t.Errorf("the key file: %v", err)
	}
	if !strings.Contains(r.log, `"msg":"enrolled the access key"`) || !strings.Contains(r.log, `"access_key_id":"ak_0123456789abcdef"`) ||
		!strings.Contains(r.log, `"pool_id":"`+poolID+`"`) || !strings.Contains(r.log, `"fingerprint":"`+r.made.Fingerprint()+`"`) || strings.Count(r.log, "\n") != 1 {
		t.Errorf("the log: %s", r.log)
	}
	noLeak(t, r, dir, code)

	again := start(t, f, dir, "")
	if again.err != nil || again.made != nil || f.requests.Load() != 1 || again.st.Key.Secret() != r.made.Secret() || again.st.InstanceID != r.st.InstanceID || again.log != "" {
		t.Errorf("the second start: %v, %d requests, log %q", again.err, f.requests.Load(), again.log)
	}
}

// TestStartWithAKeyLeavesTheCodeUnused pins that a folder with a key and the variable
// set logs one line that the code is not used, sends nothing, and makes no key.
func TestStartWithAKeyLeavesTheCodeUnused(t *testing.T) {
	f := newFakeApiary(t, poolEnrolled)
	dir := stateDir(t)
	if r := start(t, f, dir, poolCode(f)); r.err != nil {
		t.Fatal(r.err)
	}
	code := codeOf(accesskey.CodePrefixPool, f.signer)
	r := start(t, f, dir, code)
	if r.err != nil || r.made != nil || f.requests.Load() != 1 {
		t.Fatalf("%v, %d requests", r.err, f.requests.Load())
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(r.log), &line); err != nil || strings.Count(r.log, "\n") != 1 ||
		line["msg"] != "Remove QORY_ENROLMENT_CODE from the environment: the state folder has an access key, so the code is not used" || line["state_dir"] != dir || line["level"] != "INFO" {
		t.Errorf("the log: %s", r.log)
	}
	noLeak(t, r, dir, code)
}

func TestStartWithoutACodeOrAKey(t *testing.T) {
	f := newFakeApiary(t, poolEnrolled)
	dir := stateDir(t)
	r := start(t, f, dir, "")
	want := "Set QORY_ENROLMENT_CODE to an enrolment code from the node pool's Access key tab in Qory Apiary, then start the provisioner again: the state folder " + dir + " has no access key."
	if r.err == nil || r.err.Error() != want || f.requests.Load() != 0 || r.made != nil {
		t.Errorf("%v", r.err)
	}
	noKey(t, dir)
}

// TestStartRefusesACodeOfAnotherKindBeforeAnyRequest pins that a code for another kind
// of node, or a value that is no code, is refused before a key is made or anything is
// sent, with an error that quotes nothing of it.
func TestStartRefusesACodeOfAnotherKindBeforeAnyRequest(t *testing.T) {
	f := newFakeApiary(t, poolEnrolled)
	for code, want := range map[string]string{
		codeOf(accesskey.CodePrefixNode, f.signer):    "Set QORY_ENROLMENT_CODE to a code from the node pool's Access key tab in Qory Apiary: this code is for a node.",
		codeOf(accesskey.CodePrefixGateway, f.signer): "Set QORY_ENROLMENT_CODE to a code from the node pool's Access key tab in Qory Apiary: this code is for a gateway.",
		codeOf(accesskey.CodePrefixAdapter, f.signer): "Set QORY_ENROLMENT_CODE to a code from the node pool's Access key tab in Qory Apiary: this code is for an integration.",
		"qecp_" + codeCore:                            "Set QORY_ENROLMENT_CODE to an enrolment code as Qory Apiary shows it: qecp_, 26 characters, a dot and a fingerprint.",
		"qecp_" + codeCore + "U.x":                    "Set QORY_ENROLMENT_CODE to an enrolment code as Qory Apiary shows it: qecp_, 26 characters, a dot and a fingerprint.",
		codeCore:                                      "Set QORY_ENROLMENT_CODE to an enrolment code as Qory Apiary shows it: qecp_, 26 characters, a dot and a fingerprint.",
	} {
		dir := stateDir(t)
		r := start(t, f, dir, code)
		if r.err == nil || r.err.Error() != want || r.made != nil {
			t.Errorf("%v, want %q", r.err, want)
		}
		noKey(t, dir)
		noLeak(t, r, dir, code)
	}
	if n := f.requests.Load(); n != 0 {
		t.Errorf("%d requests were sent", n)
	}
}

// TestStartKeepsNoKeyItCannotTrust pins that a refusal, an answer that does not verify,
// and a 201 for another kind of node write no key file, and that each error starts with
// the fix and quotes neither the secret nor the code.
func TestStartKeepsNoKeyItCannotTrust(t *testing.T) {
	for _, tc := range []struct {
		rp   reply
		want string
	}{
		{reply{status: http.StatusUnauthorized}, "Make a new enrolment code on the node pool's Access key tab, set QORY_ENROLMENT_CODE to it, and start the provisioner again: the server at http://127.0.0.1:"},
		{reply{status: http.StatusConflict, refusal: accesskey.CodeKeyInvalid}, "Start the provisioner again within the code's 15 minutes, and it makes a new key: Qory Apiary returned key_invalid for the key, and the code is not used."},
		{reply{status: http.StatusConflict, refusal: accesskey.CodeKeyLimit}, "Have a workspace admin of Qory Apiary revoke one of the node pool's two access keys, then start the provisioner again within the code's 15 minutes: the node pool has two keys already."},
		{reply{status: http.StatusTooManyRequests, refusal: accesskey.CodeRateLimited}, "Start the provisioner again later, within the code's 15 minutes: Qory Apiary returned rate_limited, because this code was tried too often."},
		{reply{status: http.StatusCreated, kind: "pool", nodeID: poolID, unsigned: true}, "Make a new enrolment code, set QORY_ENROLMENT_CODE to it, and start the provisioner again: the response to the enrolment did not verify (HTTP 201), so no secret was written. Have a workspace admin of Qory Apiary revoke the key "},
		{reply{status: http.StatusConflict, refusal: "other"}, "Make a new enrolment code, set QORY_ENROLMENT_CODE to it, and start the provisioner again: the enrolment did not complete: "},
		{reply{status: http.StatusCreated, kind: "gateway", nodeID: "ng_0123456789abcdef"}, "Make a new enrolment code on the node pool's Access key tab, set QORY_ENROLMENT_CODE to it, and start the provisioner again: Qory Apiary enrolled the key as ak_0123456789abcdef for the gateway ng_0123456789abcdef, not a node pool, so no secret was written. Have a workspace admin of Qory Apiary revoke the key "},
	} {
		f := newFakeApiary(t, tc.rp)
		dir := stateDir(t)
		code := poolCode(f)
		r := start(t, f, dir, code)
		if r.err == nil || !strings.HasPrefix(r.err.Error(), tc.want) || f.requests.Load() != 1 {
			t.Errorf("%d %s: %v, want %q", tc.rp.status, tc.rp.refusal, r.err, tc.want)
		}
		noKey(t, dir)
		noLeak(t, r, dir, code)
	}
}

// TestStartSaysAStateItCouldNotWrite pins that a key enrolled whose state cannot be
// written is an error that says to revoke the key, and keeps no key file.
func TestStartSaysAStateItCouldNotWrite(t *testing.T) {
	f := newFakeApiary(t, poolEnrolled)
	dir := stateDir(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A folder at enrolment.json's path makes the rename over it fail.
	if err := os.Mkdir(filepath.Join(dir, state.EnrolmentFile), 0o700); err != nil {
		t.Fatal(err)
	}
	code := poolCode(f)
	r := start(t, f, dir, code)
	want := "Make a new enrolment code, set QORY_ENROLMENT_CODE to it, and start the provisioner again after you fix the state folder, and have a workspace admin of Qory Apiary revoke the key ak_0123456789abcdef: the key is enrolled, and the state folder has no key file for it: Fix the state folder " + dir + ", then start the provisioner again: renaming a new file to enrolment.json failed: rename "
	if r.err == nil || !strings.HasPrefix(r.err.Error(), want) || f.requests.Load() != 1 {
		t.Errorf("%v", r.err)
	}
	noKey(t, dir)
	noLeak(t, r, dir, code)
}

// TestStartRefusesAStateFolderOthersCanUse pins that the folder is checked before the
// code is read or anything is sent.
func TestStartRefusesAStateFolderOthersCanUse(t *testing.T) {
	f := newFakeApiary(t, poolEnrolled)
	dir := stateDir(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := start(t, f, dir, poolCode(f))
	if r.err == nil || r.err.Error() != "Make the state folder "+dir+" 0700: group or others have permissions on it (-rwxr-xr-x)." || f.requests.Load() != 0 || r.made != nil {
		t.Errorf("%v", r.err)
	}
}

// TestStartSaysAQoryApiaryItCannotReach pins the error of an enrolment that got no
// answer: it starts with the fix, writes no key, and quotes neither the secret nor the
// code.
func TestStartSaysAQoryApiaryItCannotReach(t *testing.T) {
	f := newFakeApiary(t, poolEnrolled)
	srv := httptest.NewServer(http.NotFoundHandler())
	gone := &fakeApiary{url: srv.URL, signer: f.signer}
	srv.Close()
	dir := stateDir(t)
	code := poolCode(f)
	r := start(t, gone, dir, code)
	want := "Check that this host reaches " + gone.url + ", then start the provisioner again within the code's 15 minutes: the enrolment did not complete: "
	if r.err == nil || !strings.HasPrefix(r.err.Error(), want) || !strings.HasSuffix(r.err.Error(), ". No secret was written. Have a workspace admin of Qory Apiary revoke the key "+r.made.Fingerprint()+" if Qory Apiary lists it.") {
		t.Errorf("%v", r.err)
	}
	noKey(t, dir)
	noLeak(t, r, dir, code)
}

// TestStartRemovesANewKeyFileACrashLeft pins that a new key file that a crash left
// before its rename is removed at the next start, before the code is read.
func TestStartRemovesANewKeyFileACrashLeft(t *testing.T) {
	f := newFakeApiary(t, poolEnrolled)
	dir := stateDir(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	left, err := accesskey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, ".access-key-secret.0123456789abcdef")
	if err := os.WriteFile(tmp, []byte(left.Secret()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := start(t, f, dir, "")
	if r.err == nil || !strings.HasPrefix(r.err.Error(), "Set QORY_ENROLMENT_CODE ") {
		t.Errorf("%v", r.err)
	}
	if _, err := os.Lstat(tmp); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the new key file was left: %v", err)
	}
	noKey(t, dir)
}
