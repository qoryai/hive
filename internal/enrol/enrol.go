// Package enrol enrols the provisioner's access key with Qory Apiary as a node pool's
// key, at the first start, and reads the state folder at every start. The key pair is
// made in the process; its secret is written only to the state folder's key file.
package enrol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/qoryai/forager/accesskey"
	"github.com/qoryai/hive/internal/state"
)

// Variable is the environment variable that contains the node pool's enrolment code,
// read only while the state folder has no access key.
const Variable = "QORY_ENROLMENT_CODE"

// admin is who revokes a node pool's access key in Qory Apiary.
const admin = "a workspace admin of Qory Apiary"

// Config is what [Start] reads. A function left nil is the one the provisioner uses
// outside a test.
type Config struct {
	// Dir is the state folder, such as [state.DefaultDir].
	Dir string
	// ApiaryURL is Qory Apiary's origin from the settings.
	ApiaryURL string
	// Version is the provisioner's version, sent in the User-Agent.
	Version string
	// Log receives the lines Start logs.
	Log *slog.Logger
	// Getenv reads an environment variable: [os.Getenv].
	Getenv func(string) string
	// GenerateKey makes the key pair: [accesskey.Generate].
	GenerateKey func() (*accesskey.Key, error)
	// Hostname is the host name that names the access key: [os.Hostname].
	Hostname func() (string, error)
	// Client posts the enrolment: nil for accesskey's, which times out after 10 s.
	Client *http.Client
	// CodeFile is enrolment_code_file from the settings: the path of the file that
	// contains the node pool's enrolment code, or "" to read the code from
	// QORY_ENROLMENT_CODE.
	CodeFile string
	// Sleep returns after d, or with ctx's error once ctx ends. Start calls it between
	// two reads of a missing code file. nil is a timer.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Start checks the state folder, removes the new files a crash left in it, see
// [state.RemoveTemporary], and returns its state. When the folder has no access
// key, Start enrols one first: it reads a node pool's code from the file at
// c.CodeFile, see [readCodeFile], or else from QORY_ENROLMENT_CODE, refuses a code of
// another kind before any request, makes the key pair, posts the enrolment, checks the
// signed 201, and writes the state. While the code file is missing, Start reads it
// again every 10 seconds until ctx ends. With both the code file and the variable set,
// Start returns an error, also when the folder has a key. When the folder has a key and
// the code file or the variable is set, it logs one line that it is not used. No log
// line or error contains the secret or the code.
func Start(ctx context.Context, c Config) (state.State, error) {
	if c.Getenv == nil {
		c.Getenv = os.Getenv
	}
	if c.Sleep == nil {
		c.Sleep = sleep
	}
	if c.GenerateKey == nil {
		c.GenerateKey = accesskey.Generate
	}
	if c.Hostname == nil {
		c.Hostname = os.Hostname
	}
	if err := state.CheckDir(c.Dir); err != nil {
		return state.State{}, err
	}
	if err := state.RemoveTemporary(c.Dir); err != nil {
		return state.State{}, err
	}
	has, err := state.HasKey(c.Dir)
	if err != nil {
		return state.State{}, err
	}
	raw := c.Getenv(Variable)
	if raw != "" && c.CodeFile != "" {
		return state.State{}, fmt.Errorf("Remove %s from the environment or enrolment_code_file from the settings file, then start the provisioner again: the provisioner reads the enrolment code from one of them, and both are set.", Variable)
	}
	if has {
		if raw != "" {
			c.Log.Info("Remove QORY_ENROLMENT_CODE from the environment: the state folder has an access key, so the code is not used", slog.String("state_dir", c.Dir))
		}
		if c.CodeFile != "" {
			c.Log.Info("the state folder has an access key, so the enrolment code file is not used", slog.String("state_dir", c.Dir), slog.String("path", c.CodeFile))
		}
		return state.Load(c.Dir, c.ApiaryURL)
	}
	src := source{file: c.CodeFile}
	if src.file != "" {
		if raw, err = waitCodeFile(ctx, c, src.file); err != nil {
			return state.State{}, err
		}
	} else if raw == "" {
		return state.State{}, fmt.Errorf("Set %s to an enrolment code from the node pool's Access key tab in Qory Apiary, then start the provisioner again: the state folder %s has no access key.", Variable, c.Dir)
	}
	code, err := checkCode(raw, src)
	if err != nil {
		return state.State{}, err
	}
	key, err := c.GenerateKey()
	if err != nil {
		return state.State{}, fmt.Errorf("Start the provisioner again: the key pair could not be made: %w", err)
	}
	if key.PublicKey().Fixture() {
		return state.State{}, errors.New("Start the provisioner again: the new key is one of the Forager contract's published fixture keys, whose secret anyone can read, so it was not used.")
	}
	ans, err := post(ctx, c, src, code, key)
	if err != nil {
		return state.State{}, err
	}
	e := state.Enrolment{ApiaryURL: c.ApiaryURL, AccessKeyID: ans.AccessKeyID, PoolID: ans.NodeID, Pin: ans.Pin}
	if err := state.Save(c.Dir, e, key); err != nil {
		if src.file != "" {
			return state.State{}, fmt.Errorf("Fix the state folder, have %s revoke the key %s, and start the provisioner again, which reads the code that Qory Apiary writes to %s: the key is enrolled, and the state folder has no key file for it: %w", admin, ans.AccessKeyID, src.file, err)
		}
		return state.State{}, fmt.Errorf("Make a new enrolment code, set %s to it, and start the provisioner again after you fix the state folder, and have %s revoke the key %s: the key is enrolled, and the state folder has no key file for it: %w", Variable, admin, ans.AccessKeyID, err)
	}
	c.Log.Info("enrolled the access key", slog.String("access_key_id", ans.AccessKeyID), slog.String("pool_id", ans.NodeID), slog.String("fingerprint", key.Fingerprint()), slog.String("apiary_url", c.ApiaryURL))
	return state.Load(c.Dir, c.ApiaryURL)
}

// codeKindNames words each kind of node a code enrols a key for, by the node_kind
// [accesskey.CodeKind] returns.
var codeKindNames = map[string]string{
	"node":    "a node",
	"pool":    "a node pool",
	"gateway": "a gateway",
	"adapter": "an integration",
}

// source is where Start read the enrolment code: the file at file, or
// QORY_ENROLMENT_CODE when file is "".
type source struct {
	file string
}

// newCode is the first part of an error after which the code is not used again: how to
// pass a new code and start the provisioner again. page adds where a person makes a new
// code for QORY_ENROLMENT_CODE.
func (s source) newCode(page bool) string {
	switch {
	case s.file != "":
		return "Start the provisioner again, which reads the code that Qory Apiary writes to " + s.file
	case page:
		return "Make a new enrolment code on the node pool's Access key tab, set " + Variable + " to it, and start the provisioner again"
	}
	return "Make a new enrolment code, set " + Variable + " to it, and start the provisioner again"
}

// checkCode takes an enrolment code as a person copies it and returns it as
// [accesskey.NormaliseCode] writes it, when it is a node pool's code. Any other value is
// refused with an error that quotes nothing of it, since NormaliseCode's error can
// quote a part of the code. src is where the code was read.
func checkCode(raw string, src source) (string, error) {
	notACode := fmt.Errorf("Set %s to an enrolment code as Qory Apiary shows it: %s, 26 characters, a dot and a fingerprint.", Variable, accesskey.CodePrefixPool)
	if src.file != "" {
		notACode = fmt.Errorf("Set enrolment_code_file in the settings file to the file in which Qory Apiary writes a node pool's enrolment code: %s contains no code of the form %s, 26 characters, a dot and a fingerprint.", src.file, accesskey.CodePrefixPool)
	}
	code, err := accesskey.NormaliseCode(strings.TrimSpace(raw))
	if err != nil {
		return "", notACode
	}
	kind, err := accesskey.CodeKind(code)
	if err != nil {
		return "", notACode
	}
	if kind != "pool" {
		what, ok := codeKindNames[kind]
		if !ok {
			return "", notACode
		}
		if src.file != "" {
			return "", fmt.Errorf("Set enrolment_code_file in the settings file to the file in which Qory Apiary writes a node pool's enrolment code: the code in %s is for %s.", src.file, what)
		}
		return "", fmt.Errorf("Set %s to a code from the node pool's Access key tab in Qory Apiary: this code is for %s.", Variable, what)
	}
	return code, nil
}

// post enrols key with code at c's Qory Apiary and returns the verified 201. A 201 for
// another kind of node than a node pool, or whose pin lists a published fixture key, is
// refused: Qory Apiary enrolled the key, and its secret is not kept.
func post(ctx context.Context, c Config, src source, code string, key *accesskey.Key) (*accesskey.EnrolmentAnswer, error) {
	fingerprint := key.Fingerprint()
	req, err := accesskey.NewEnrolmentRequest(key, code, keyName(c.Hostname), time.Now())
	if err != nil {
		return nil, fmt.Errorf("Check this host's clock, then start the provisioner again: the enrolment request could not be made: %w", err)
	}
	ans, err := req.Post(ctx, c.Client, c.ApiaryURL, "qory-provisioner/"+c.Version)
	var kindErr *accesskey.KindError
	if errors.As(err, &kindErr) {
		return nil, notAPool(src, kindErr.AccessKeyID, kindErr.Kind, kindErr.NodeID, fingerprint)
	}
	if err != nil {
		return nil, failed(src, err, c.ApiaryURL, fingerprint)
	}
	if ans.NodeKind != "pool" {
		return nil, notAPool(src, ans.AccessKeyID, ans.NodeKind, ans.NodeID, fingerprint)
	}
	if ans.Pin.Fixture() {
		return nil, fmt.Errorf("Have %s revoke the key %s: Qory Apiary's response contains the Forager contract's published fixture key, whose secret anyone can read, so no secret was written.", admin, fingerprint)
	}
	return ans, nil
}

// notAPool is the error of a key Qory Apiary enrolled as id in the node of kind and id
// node, which is no node pool, so its secret is not kept.
func notAPool(src source, id, kind, node, fingerprint string) error {
	what := "the " + kind
	if name, ok := codeKindNames[kind]; ok {
		what = "the " + strings.TrimPrefix(strings.TrimPrefix(name, "a "), "an ")
	}
	return fmt.Errorf("%s: Qory Apiary enrolled the key as %s for %s %s, not a node pool, so no secret was written. Have %s revoke the key %s.", src.newCode(true), id, what, node, admin, fingerprint)
}

// failed is the error of an enrolment that got no verified 201, worded on the refusal's
// code alone, never on an HTTP status: an answer that does not verify may come from
// anyone on the path, and so may a 401, which is never signed. A verified refusal
// leaves the code as it was. An enrolment that may have succeeded behind an answer that
// did not verify cannot be finished, so its key is to be revoked.
func failed(src source, err error, server, fingerprint string) error {
	revoke := fmt.Sprintf(" Have %s revoke the key %s if Qory Apiary lists it.", admin, fingerprint)
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("Check that this host reaches %s, then start the provisioner again within the code's 15 minutes: the enrolment did not complete: %w. No secret was written.%s", server, err, revoke)
	}
	var ref *accesskey.Refusal
	if !errors.As(err, &ref) {
		return fmt.Errorf("%s: the enrolment did not complete: %w. No secret was written.%s", src.newCode(false), err, revoke)
	}
	switch ref.Code {
	case accesskey.CodeUnauthorized:
		return fmt.Errorf("%s: the server at %s returned HTTP 401: Qory Apiary did not accept this code, or a server in front of Qory Apiary returned the 401.", src.newCode(true), server)
	case accesskey.CodeKeyInvalid:
		return errors.New("Start the provisioner again within the code's 15 minutes, and it makes a new key: Qory Apiary returned key_invalid for the key, and the code is not used.")
	case accesskey.CodeKeyLimit:
		return fmt.Errorf("Have %s revoke one of the node pool's two access keys, then start the provisioner again within the code's 15 minutes: the node pool has two keys already.", admin)
	case accesskey.CodeRateLimited:
		return errors.New("Start the provisioner again later, within the code's 15 minutes: Qory Apiary returned rate_limited, because this code was tried too often.")
	case accesskey.CodeAnswerUnsigned:
		return fmt.Errorf("%s: the response to the enrolment did not verify (HTTP %d), so no secret was written.%s", src.newCode(false), ref.Status, revoke)
	}
	return fmt.Errorf("%s: the enrolment did not complete: %w. No secret was written.%s", src.newCode(false), err, revoke)
}

// keyName is the access key's name in Qory Apiary: this host's name, or its first label
// when the whole does not fit an access key's name, or else qory-provisioner.
func keyName(hostname func() (string, error)) string {
	if host, err := hostname(); err == nil {
		if name := accesskey.DefaultName(host); name != "" {
			return name
		}
	}
	return "qory-provisioner"
}
