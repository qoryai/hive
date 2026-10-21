package provisioner

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	foragercontracts "github.com/qoryai/forager/contracts"
)

// The states an outcome may have, run-outcome.schema.json.
const (
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)

// reasonShape is an outcome's reason, a code, run-outcome.schema.json's reason.
var reasonShape = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Outcome is how a handed-over run ended: the body of Forager's run-outcome.schema.json
// without its version and time, which [Client.PostOutcome] sets.
type Outcome struct {
	// State is [StateSucceeded], [StateFailed] or [StateCancelled]. Empty sends neither
	// state nor reason, which Forager reads as cancelled with the reason stopped.
	State string `json:"state,omitempty"`
	// Reason is a code: a lower-case letter, then up to 63 lower-case letters, digits
	// and underscores. It needs a State. Empty sends none.
	Reason string `json:"reason,omitempty"`
}

// outcomeBody is an outcome's body: the version, the outcome's members and the time,
// in the order Forager's client writes them.
type outcomeBody struct {
	Version int `json:"version"`
	Outcome
	Time string `json:"time"`
}

// PostOutcome posts o, how the handed-over run runID ended, to
// <runURL>/<runID>/outcome, Forager's run-outcome.schema.json, signed with the node
// pool's access key. runURL is the run endpoint, [Configuration.RunURL]: the handover's
// answer does not have it. X-Qory-Contract-Version is Forager's revision. A State
// other than the three, a Reason without a State or outside its form, a runID outside
// its form, and a runURL that fails the check of [Client.Configuration] are errors
// before anything is sent. The answer is a signed 200 whose body passes
// run-outcome-answer.schema.json. The same outcome again gets the signed 200. A
// refusal is a [*Refusal]: [ErrOutcomeGiven], [ErrRunNotFound] or [ErrRateLimited].
func (c *Client) PostOutcome(ctx context.Context, runURL, runID string, o Outcome) error {
	if !runIDShape.MatchString(runID) {
		return errors.New("the outcome of a run: the run id is not a UUID in the canonical lower-case form")
	}
	what := "the outcome of run " + runID
	problem := runURLProblem(runURL)
	switch {
	case problem == runURLSecret:
		return errors.New(what + ": " + runURLSecret + ", so the client did not send it")
	case problem != "":
		return errors.New(what + ": " + problem)
	case o.State != "" && o.State != StateSucceeded && o.State != StateFailed && o.State != StateCancelled:
		return errors.New(what + ": set the state to succeeded, failed or cancelled")
	case o.State == "" && o.Reason != "":
		return errors.New(what + ": set a state for the reason; with no state, the outcome is cancelled with the reason stopped")
	case o.Reason != "" && !reasonShape.MatchString(o.Reason):
		return errors.New(what + ": set the reason to a code, a lower-case letter, then up to 63 lower-case letters, digits and underscores")
	}
	ans, err := c.do(ctx, request{
		what: what, kind: kindOutcome, url: runURL + "/" + runID + "/outcome", revision: foragercontracts.Revision,
		schema: "run-outcome.schema.json", forager: true, limit: MaxAnswer, timeout: RequestTimeout,
		body: func(t string) any { return outcomeBody{Version: 1, Outcome: o, Time: t} },
	})
	if err != nil {
		return err
	}
	defer clear(ans.body)
	if ans.status != http.StatusOK {
		return c.refused(what, kindOutcome, ans)
	}
	if err := readAnswer(foragerSchema, "run-outcome-answer.schema.json", ans.body, nil); err != nil {
		return unusable(what, err.Error())
	}
	return nil
}
