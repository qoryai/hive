package provisioner

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/qoryai/integrations/contracts"
)

// runCodeShape is a claim answer's run_code. The schema checks every other member of
// the answer.
var runCodeShape = regexp.MustCompile(`^qecr_[0-9A-HJKMNP-TV-Z]{26}(\.[A-Za-z0-9_-]{22}){1,2}$`)

// codeStandIn takes the place of the run code in the copy of a claim's answer that the
// schema checks: a value of the run code's form that is no run code.
var codeStandIn = []byte(`"qecr_00000000000000000000000000.AAAAAAAAAAAAAAAAAAAAAA"`)

// Claimed is a claim's answer, claim-answer.schema.json, checked: signed under the pin,
// of the schema, and of the revision sent. It is a request for one machine.
type Claimed struct {
	// DispatchID is the dispatch, dp_…, that the later requests name.
	DispatchID string
	// PoolID is the node pool whose key made the claim, np_….
	PoolID string
	// LeaseUntil is when the dispatch's lease lapses: the provisioner sends the first
	// dispatched before then.
	LeaseUntil time.Time
	// Machine is the type of machine to create, such as small, or "" when the answer has
	// none and the provisioner uses its default.
	Machine string
	// RunCode is the run code, qecr_…, that the machine redeems. The caller passes it to
	// the machine and then clears it.
	RunCode Hidden
	// GatewayURL is the gateway the run code is bound to: https, a host and an optional
	// port. The caller passes it to the machine with the run code.
	GatewayURL string
	// MaxLifetimeMinutes is the minutes, from 15 to 1470, after which the machine powers
	// off, counted from its start.
	MaxLifetimeMinutes int
}

// Claim sends POST /v1/pools/claim, claim.schema.json, with waitSeconds, 0 to
// [MaxWaitSeconds]: Qory Apiary holds the claim up to waitSeconds while no work can be
// claimed. A signed 204 is no work: ok is false. A signed 200 is a request for one
// machine, once checked, see [Claimed]; one of another revision than the one sent is
// [ErrAnswerUnusable]. A refusal is a [*Refusal]: [ErrForbidden], [ErrNoGateway],
// [ErrNotFound] or [ErrRateLimited].
//
// The answer's bytes are cleared once read, and the run code is unquoted from them
// straight into its [Hidden]: the schema checks a copy in which a stand-in takes the
// run code's place, so no string ever contains the run code.
func (c *Client) Claim(ctx context.Context, waitSeconds int) (claimed Claimed, ok bool, err error) {
	const what = "the claim"
	if waitSeconds < 0 || waitSeconds > MaxWaitSeconds {
		return Claimed{}, false, fmt.Errorf("%s: set wait_seconds from 0 to %d; %d is outside", what, MaxWaitSeconds, waitSeconds)
	}
	ans, err := c.do(ctx, request{
		what: what, kind: kindClaim, path: routeClaim, schema: "claim.schema.json",
		limit: MaxClaimAnswer, timeout: ClaimTimeout,
		body: func(t string) any { return contracts.Claim{Version: 1, Time: t, WaitSeconds: waitSeconds} },
	})
	if err != nil {
		return Claimed{}, false, err
	}
	defer clear(ans.body)
	switch ans.status {
	case http.StatusNoContent:
		return Claimed{}, false, nil
	case http.StatusOK:
	default:
		return Claimed{}, false, c.refused(what, kindClaim, ans)
	}
	claimed, err = readClaim(ans.body)
	if err != nil {
		return Claimed{}, false, unusable(what, err.Error())
	}
	return claimed, true, nil
}

// readClaim reads body, a claim's signed 200, into a [Claimed]. It unquotes the run code
// from body straight into the Hidden and checks its form first. The schema then checks
// a copy of body with [codeStandIn] in the run code's place. The caller clears body.
// Its error contains the schema or a revision, never a value of body.
func readClaim(body []byte) (Claimed, error) {
	const name = "claim-answer.schema.json"
	broken := fmt.Errorf("it fails %s", name)
	if !jsontext.Value(body).IsValid() {
		return Claimed{}, errNotOneDocument
	}
	start, end, ok := memberSpan(body, "run_code")
	if !ok {
		return Claimed{}, broken
	}
	code, err := jsontext.AppendUnquote(make([]byte, 0, end-start), body[start:end])
	if err != nil || !runCodeShape.Match(code) {
		clear(code)
		return Claimed{}, broken
	}
	stood := slices.Concat(body[:start], codeStandIn, body[end:])
	var doc contracts.ClaimAnswer
	err = readAnswer(name, stood, &doc)
	clear(stood)
	var lease time.Time
	switch {
	case err != nil:
	case doc.Revision != Revision:
		err = errors.New(revisionText(doc.Revision))
	case secret(doc.GatewayURL):
		// The schema allows qecr_ in a host name; a gateway URL that contains it is
		// refused, so that no URL a caller logs contains what looks like a run code.
		err = errors.New("its gateway_url contains what looks like an access key secret, a run key or a run code")
	default:
		lease, err = time.Parse(time.RFC3339, doc.LeaseUntil)
		if err != nil {
			err = errors.New("its lease_until is not a time")
		}
	}
	if err != nil {
		clear(code)
		return Claimed{}, err
	}
	return Claimed{
		DispatchID: doc.DispatchID, PoolID: doc.PoolID, LeaseUntil: lease, Machine: doc.Machine,
		RunCode: Hide(code), GatewayURL: doc.GatewayURL, MaxLifetimeMinutes: doc.MaxLifetimeMinutes,
	}, nil
}

// memberSpan is where the string value of body's top-level member is, quotes included:
// body[start:end]. body is one JSON object; ok is false when it has no such member, or
// its value is not a string. The decoder reads body in place, from a bytes.Buffer, so
// the value is never copied.
func memberSpan(body []byte, member string) (start, end int, ok bool) {
	dec := jsontext.NewDecoder(bytes.NewBuffer(body))
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '{' {
		return 0, 0, false
	}
	for dec.PeekKind() == '"' {
		name, err := dec.ReadToken()
		if err != nil {
			return 0, 0, false
		}
		if name.String() != member {
			if err := dec.SkipValue(); err != nil {
				return 0, 0, false
			}
			continue
		}
		v, err := dec.ReadValue()
		if err != nil || v.Kind() != '"' {
			return 0, 0, false
		}
		end := int(dec.InputOffset())
		return end - len(v), end, true
	}
	return 0, 0, false
}
