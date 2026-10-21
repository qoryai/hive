package provisioner

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/qoryai/forager/accesskey"
)

// The results of a request the provisioner branches on, each matched with errors.Is
// against the error a method returns. A signed refusal matches only for a request whose
// schema lists it, with its status and its code: claim.schema.json,
// dispatched.schema.json, handover.schema.json and release.schema.json of
// provisioner/v1, and Forager's run-outcome.schema.json.
var (
	// ErrForbidden is the signed 403 forbidden: a request outside the access key's
	// rights, or a dispatch that is unknown or of another node pool.
	ErrForbidden = errors.New("403 forbidden")
	// ErrNotFound is the signed 404 not_found of a provisioner/v1 route: Qory Apiary
	// does not serve the route.
	ErrNotFound = errors.New("404 not_found")
	// ErrGone is the signed 410 gone: the dispatch ended. To a handover, it is also a
	// run Qory Apiary could not form, which Qory Apiary then ended.
	ErrGone = errors.New("410 gone")
	// ErrHandedOver is a handover's signed 409 handed_over: the run was handed over
	// before, through this dispatch or another one. Qory Apiary never sends its run key
	// again.
	ErrHandedOver = errors.New("409 handed_over")
	// ErrLeaseExpired is the signed 409 lease_expired: the dispatch's lease lapsed
	// before its handover.
	ErrLeaseExpired = errors.New("409 lease_expired")
	// ErrNotDispatched is a handover's signed 409 not_dispatched: the handover came
	// before dispatched.
	ErrNotDispatched = errors.New("409 not_dispatched")
	// ErrNoGateway is the signed 409 no_gateway of a claim or a handover: the node pool
	// has no gateway to register a run through. After a handover's, the dispatch stays
	// claimed until its lease lapses, and the handover may be sent again within the
	// lease.
	ErrNoGateway = errors.New("409 no_gateway")
	// ErrUnavailable is a handover's signed 503 unavailable: Qory Apiary failed to make
	// the run's inputs or its run key, and the handover took no effect. Handover returns
	// it after one try; the contract allows the handover again, with backoff, within the
	// lease.
	ErrUnavailable = errors.New("503 unavailable")
	// ErrRateLimited is the signed 429 rate_limited; [Refusal.RetryAfter] is the wait
	// its Retry-After sets. An unsigned 429 is [ErrUnsigned].
	ErrRateLimited = errors.New("429 rate_limited")
	// ErrOutcomeGiven is an outcome's signed 409 outcome_given: the run has another
	// outcome already.
	ErrOutcomeGiven = errors.New("409 outcome_given")
	// ErrRunNotFound is an outcome's signed 404 not_found: the node pool did not
	// register the run, or the run's record is gone.
	ErrRunNotFound = errors.New("404 not_found")
	// ErrUnsigned is an answer within its bound with no valid signature under the pin,
	// after every try: a 401, which is always unsigned and does not mean the access key
	// is revoked, a 413, 415 or 400 that Qory Apiary returns before it checks the
	// signature, or any other answer that fails the check. Nothing in it is used.
	ErrUnsigned = errors.New("an answer with no valid signature under the pin")
	// ErrTooLarge is an answer larger than its bound, whatever its status, after every
	// try. The client drops it before it reads the signature, so nothing in it is used.
	ErrTooLarge = errors.New("an answer larger than its bound")
	// ErrUnreachable is no answer from Qory Apiary after every try.
	ErrUnreachable = errors.New("no answer from Qory Apiary")
	// ErrAnswerUnusable is a signed answer within its bound that breaks the contract: a
	// success status the request does not have, a body that is not one JSON document or
	// fails its schema, another revision than the one sent, another run than the one
	// claimed, inputs over their bound, or a configuration document with no
	// X-Qory-Configuration or a run endpoint outside its form. Nothing in it is used.
	// The request is not sent again, since Qory Apiary acted on it.
	ErrAnswerUnusable = errors.New("Qory Apiary's answer breaks the contract, so the client used nothing in it")
)

// codeOf is the status and the code of each signed refusal above.
var codeOf = map[error]struct {
	status int
	code   string
}{
	ErrForbidden:     {http.StatusForbidden, "forbidden"},
	ErrNotFound:      {http.StatusNotFound, "not_found"},
	ErrGone:          {http.StatusGone, "gone"},
	ErrHandedOver:    {http.StatusConflict, "handed_over"},
	ErrLeaseExpired:  {http.StatusConflict, "lease_expired"},
	ErrNotDispatched: {http.StatusConflict, "not_dispatched"},
	ErrNoGateway:     {http.StatusConflict, "no_gateway"},
	ErrUnavailable:   {http.StatusServiceUnavailable, "unavailable"},
	ErrRateLimited:   {http.StatusTooManyRequests, "rate_limited"},
	ErrOutcomeGiven:  {http.StatusConflict, "outcome_given"},
	ErrRunNotFound:   {http.StatusNotFound, "not_found"},
}

// kind is the request a refusal is to.
type kind int

const (
	kindClaim kind = iota
	kindDispatched
	kindHandover
	kindRelease
	kindOutcome
	kindConfiguration
)

// refusalsOf are the signed refusals each request's schema lists, in the order Qory
// Apiary checks them, with the 404 of a provisioner/v1 route it does not serve and the
// 429 any request may get, refusal.schema.json. The GET of the configuration document
// has the 429 alone: its signed 400s and 404 match no refusal.
var refusalsOf = map[kind][]error{
	kindClaim:      {ErrForbidden, ErrNoGateway, ErrNotFound, ErrRateLimited},
	kindDispatched: {ErrForbidden, ErrNotFound, ErrGone, ErrLeaseExpired, ErrRateLimited},
	// A handover's 410 gone has two places in its order: a dispatch that ended, third,
	// and a run Qory Apiary cannot form, after 409 no_gateway.
	kindHandover:      {ErrForbidden, ErrNotFound, ErrGone, ErrHandedOver, ErrLeaseExpired, ErrNotDispatched, ErrNoGateway, ErrGone, ErrUnavailable, ErrRateLimited},
	kindRelease:       {ErrForbidden, ErrNotFound, ErrGone, ErrLeaseExpired, ErrRateLimited},
	kindOutcome:       {ErrRunNotFound, ErrOutcomeGiven, ErrRateLimited},
	kindConfiguration: {ErrRateLimited},
}

// Refusal is an answer other than the one a request is for: its status, whether its
// signature verified under the pin or the client dropped it for its size first, and
// the code of a signed one's body. errors.Is matches it against [ErrForbidden] and the
// other signed refusals by its request, its status and its code, against [ErrUnsigned]
// when it is within its bound with no valid signature, and against [ErrTooLarge] when
// it is over its bound. Its text contains the request, the status and the code, never
// a body, a header or a secret.
type Refusal struct {
	// What is the request, as an error states it: "the claim", "the handover of dp_…".
	What string
	// Status is the answer's HTTP status.
	Status int
	// Signed is whether the answer's signature verified under the pin. The client reads
	// a code from a signed answer alone.
	Signed bool
	// Code is a signed answer's refusal code, refusal.schema.json's error, or "" when
	// its body has none.
	Code string
	// RetryAfter is the wait a signed 429's Retry-After sets, from 1 second to 1 hour;
	// 0 otherwise.
	RetryAfter time.Duration
	// TooLarge is whether the answer was over its bound, Limit bytes, so that the
	// client dropped it before it read the signature.
	TooLarge bool
	Limit    int64
	// Resent is whether the client sent the request more than once in the same call,
	// after a try whose answer was lost: no answer, one over its bound, one with no
	// valid signature, or a server error. Such a try may have taken effect: a handover's
	// 409 handed_over may follow a handover whose 200 was lost.
	Resent bool

	kind kind
}

func (r *Refusal) Error() string {
	switch {
	case r.TooLarge:
		return fmt.Sprintf("%s: the answer with status %d is larger than %d bytes, so the client dropped it before it read the signature", r.What, r.Status, r.Limit)
	case !r.Signed && r.Status == http.StatusUnauthorized:
		return "check that the access key is approved in Qory Apiary and that this machine's clock is within 300 seconds of the correct time: " +
			r.What + " returned 401 unauthorized, which the client does not take as a revoked key"
	case !r.Signed:
		return fmt.Sprintf("%s: the answer with status %d has no valid signature under the pin, so the client used nothing in it", r.What, r.Status)
	case r.Code == "":
		return fmt.Sprintf("%s: Qory Apiary returned %d", r.What, r.Status)
	case r.RetryAfter > 0:
		return fmt.Sprintf("%s: Qory Apiary returned %d %s with a wait of %s", r.What, r.Status, r.Code, r.RetryAfter)
	}
	return fmt.Sprintf("%s: Qory Apiary returned %d %s", r.What, r.Status, r.Code)
}

// Is reports whether the refusal is target: [ErrTooLarge] for an answer over its bound,
// [ErrUnsigned] for one within it with no valid signature, or a signed refusal that the
// request's schema lists, with its status and its code.
func (r *Refusal) Is(target error) bool {
	switch target {
	case ErrTooLarge:
		return r.TooLarge
	case ErrUnsigned:
		return !r.Signed && !r.TooLarge
	}
	c, ok := codeOf[target]
	return ok && r.Signed && r.Status == c.status && r.Code == c.code && slices.Contains(refusalsOf[r.kind], target)
}

// refused is the [*Refusal] of a signed answer other than the one wanted, or
// [ErrAnswerUnusable] for a success status the request does not have. A provisioner/v1
// code is read only from a body that passes refusal.schema.json; an outcome's and the
// configuration document's from a coded refusal of Forager's, {"error": "<code>"}. A
// code that contains what looks like a secret is dropped. A 429's Retry-After is read
// from 1 second to 1 hour.
func (c *Client) refused(what string, k kind, ans *answer) error {
	if ans.status >= 200 && ans.status < 300 {
		return unusable(what, fmt.Sprintf("its status is %d, which the contract does not have for this request", ans.status))
	}
	r := &Refusal{What: what, Status: ans.status, Signed: ans.signed, Resent: ans.resent, kind: k}
	var doc struct {
		Error string `json:"error"`
	}
	if k == kindOutcome || k == kindConfiguration {
		if rr := accesskey.ReadRefusal(ans.status, ans.body); rr != nil {
			r.Code = rr.Code
		}
	} else if readAnswer(provisionerSchema, "refusal.schema.json", ans.body, &doc) == nil {
		r.Code = doc.Error
	}
	if accesskey.ContainsSecret(r.Code) {
		r.Code = ""
	}
	if ans.status == http.StatusTooManyRequests {
		r.RetryAfter = retryAfter(ans.retryAfter, c.now())
	}
	return r
}

// unusable is [ErrAnswerUnusable] for the request what, and why: a fixed text that may
// contain ids, statuses and revisions, and no other value of the answer's body.
func unusable(what, why string) error {
	return fmt.Errorf("%s: %w: %s", what, ErrAnswerUnusable, why)
}

// The bounds of a signed 429's Retry-After.
const (
	minRetryAfter = time.Second
	maxRetryAfter = time.Hour
)

// retryAfter reads Retry-After, seconds or an HTTP date, from 1 second to 1 hour; a
// value outside both forms is 1 second.
func retryAfter(v string, now time.Time) time.Duration {
	d := minRetryAfter
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
		d = time.Duration(min(n, int64(maxRetryAfter/time.Second))) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	}
	return min(max(d, minRetryAfter), maxRetryAfter)
}
