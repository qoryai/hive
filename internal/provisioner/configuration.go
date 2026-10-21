package provisioner

import (
	"context"
	jsonv2 "encoding/json/v2"
	"net/http"
	"regexp"
	"slices"

	"github.com/qoryai/forager/accesskey"
	foragercontracts "github.com/qoryai/forager/contracts"
	"github.com/qoryai/forager/server"
)

// Configuration is the configuration document Qory Apiary returns to the node pool's
// access key, Forager's configuration.schema.json, checked: signed under the pin, with
// X-Qory-Configuration, of the schema, and with a run endpoint of the form
// [Client.PostOutcome] posts to.
type Configuration struct {
	// NodeID is the id of the access key's node pool, node_id, for display.
	NodeID string
	// Workspaces are the workspace ids of the document: one for a node pool's key.
	Workspaces []string
	// RunURL is the run endpoint, run.url: https, or http to a loopback address, with
	// no query, no fragment and no trailing slash, and with nothing in it that looks
	// like an access key secret or a run key.
	RunURL string
}

// runURLShape is the run endpoint, Forager's configuration.schema.json run.url: https,
// or http to a loopback address, with no query, no fragment and no trailing slash.
var runURLShape = regexp.MustCompile(`^(https://[^/?#@\s]+|http://(localhost|127\.0\.0\.1|\[::1\])(:[0-9]+)?)(/[^\s?#]*[^\s?#/])?$`)

// The problems [runURLProblem] finds in a run endpoint.
const (
	runURLSecret = "the run endpoint contains what looks like an access key secret or a run key"
	runURLForm   = "the run endpoint is not https, or http to a loopback address, with no query, no fragment and no trailing slash"
)

// runURLProblem is the problem of runURL as a run endpoint, the one check that
// [Client.PostOutcome] and [Client.Configuration] both make: [runURLSecret],
// [runURLForm], or "" for none. It never quotes runURL.
func runURLProblem(runURL string) string {
	switch {
	case accesskey.ContainsSecret(runURL):
		return runURLSecret
	case !runURLShape.MatchString(runURL):
		return runURLForm
	}
	return ""
}

// Configuration sends the signed GET of <URL>/.well-known/qory-configuration, with
// X-Qory-Timestamp the clock's second and X-Qory-Contract-Version Forager's revision,
// and returns the configuration document, once checked, see [Configuration]. Each call
// fetches the document again. The answer is a signed 200, read up to
// server.MaxDocument, with X-Qory-Configuration, and a body of one JSON document, with
// valid UTF-8 and no member twice, that passes configuration.schema.json. A member the
// schema does not define is ignored. Its run.url must pass the check PostOutcome makes.
// An answer with no X-Qory-Configuration, one that fails the schema, and one whose run
// endpoint fails that check are [ErrAnswerUnusable]. A refusal is a [*Refusal]:
// [ErrRateLimited], or a signed 400 bad_request or unsupported_contract_version, or a
// 404, which match no refusal.
func (c *Client) Configuration(ctx context.Context) (Configuration, error) {
	const what = "the configuration document"
	ans, err := c.do(ctx, request{
		what: what, kind: kindConfiguration, url: c.cfg.URL + server.WellKnown, revision: foragercontracts.Revision,
		forager: true, get: true, limit: server.MaxDocument, timeout: RequestTimeout,
	})
	if err != nil {
		return Configuration{}, err
	}
	defer clear(ans.body)
	if ans.status != http.StatusOK {
		return Configuration{}, c.refused(what, kindConfiguration, ans)
	}
	if ans.configuration == "" {
		return Configuration{}, unusable(what, "it has no X-Qory-Configuration header")
	}
	const schema = "configuration.schema.json"
	if err := readAnswer(foragerSchema, schema, ans.body, nil); err != nil {
		return Configuration{}, unusable(what, err.Error())
	}
	var doc server.Configuration
	if err := jsonv2.Unmarshal(ans.body, &doc); err != nil {
		return Configuration{}, unusable(what, "it fails "+schema)
	}
	if doc.Run == nil {
		return Configuration{}, unusable(what, "it has no run endpoint, run.url")
	}
	if p := runURLProblem(doc.Run.URL); p != "" {
		return Configuration{}, unusable(what, p)
	}
	return Configuration{NodeID: doc.NodeID, Workspaces: slices.Clone(doc.Workspaces), RunURL: doc.Run.URL}, nil
}
