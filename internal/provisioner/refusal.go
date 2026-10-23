package provisioner

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"
)

// The results of a request the provisioner branches on, each matched with errors.Is
// against the error a method returns. A signed refusal matches only for a request whose
// schema lists it, with its status and its code: claim.schema.json,
// dispatched.schema.json, release.schema.json and machine-log.schema.json of
// provisioner/v1.
var (
	// ErrForbidden is the signed 403 forbidden: a request outside the access key's
	// rights, or a dispatch that is unknown or of another node pool.
	ErrForbidden = errors.New("403 forbidden")
	// ErrNotFound is the signed 404 not_found: Qory Apiary does not serve the route.
	ErrNotFound = errors.New("404 not_found")
	// ErrGone is the signed 410 gone of a dispatched or a release: the dispatch ended.
	// After a dispatched's, the contract requires the provisioner to send the machine
	// log and delete the machine.
	ErrGone = errors.New("410 gone")
	// ErrLeaseExpired is the signed 409 lease_expired of a dispatched or a release: the
	// dispatch's lease lapsed before its first dispatched and before a redemption of its
	// run code. After it, the contract requires the provisioner to stop the machine it
	// created for the dispatch.
	ErrLeaseExpired = errors.New("409 lease_expired")
	// ErrMachineExists is a release's signed 409 machine_exists: Qory Apiary accepted
	// dispatched for the dispatch, or the machine redeemed the run code.
	ErrMachineExists = errors.New("409 machine_exists")
	// ErrMachineLogExists is a machine log's signed 409 machine_log_exists: the dispatch
	// has a machine log.
	ErrMachineLogExists = errors.New("409 machine_log_exists")
	// ErrNoGateway is a claim's signed 409 no_gateway: the node pool's choice of gateway
	// resolves to no gateway.
	ErrNoGateway = errors.New("409 no_gateway")
	// ErrRateLimited is the signed 429 rate_limited; [Refusal.RetryAfter] is the wait
	// its Retry-After sets. An unsigned 429 is [ErrUnsigned].
	ErrRateLimited = errors.New("429 rate_limited")
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
	// fails its schema, or another revision than the one sent. Nothing in it is used.
	// The request is not sent again, since Qory Apiary acted on it.
	ErrAnswerUnusable = errors.New("Qory Apiary's answer breaks the contract, so the client used nothing in it")
)

// codeOf is the status and the code of each signed refusal above.
var codeOf = map[error]struct {
	status int
	code   string
}{
	ErrForbidden:        {http.StatusForbidden, "forbidden"},
	ErrNotFound:         {http.StatusNotFound, "not_found"},
	ErrGone:             {http.StatusGone, "gone"},
	ErrLeaseExpired:     {http.StatusConflict, "lease_expired"},
	ErrMachineExists:    {http.StatusConflict, "machine_exists"},
	ErrMachineLogExists: {http.StatusConflict, "machine_log_exists"},
	ErrNoGateway:        {http.StatusConflict, "no_gateway"},
	ErrRateLimited:      {http.StatusTooManyRequests, "rate_limited"},
}

// kind is the request a refusal is to.
type kind int

const (
	kindClaim kind = iota
	kindDispatched
	kindRelease
	kindMachineLog
)

// refusalsOf are the signed refusals of each request: first those its schema lists, in
// the order the schema states, then the 404 of a route Qory Apiary does not serve and
// the 429 any request may get, refusal.schema.json. [Refusal.Is] matches by membership
// alone, not by place. A machine log is never refused 410 gone or 409 lease_expired.
var refusalsOf = map[kind][]error{
	kindClaim:      {ErrForbidden, ErrNoGateway, ErrNotFound, ErrRateLimited},
	kindDispatched: {ErrForbidden, ErrGone, ErrLeaseExpired, ErrNotFound, ErrRateLimited},
	kindRelease:    {ErrForbidden, ErrGone, ErrMachineExists, ErrLeaseExpired, ErrNotFound, ErrRateLimited},
	kindMachineLog: {ErrForbidden, ErrMachineLogExists, ErrNotFound, ErrRateLimited},
}

// Refusal is an answer other than the one a request is for: its status, whether its
// signature verified under the pin or the client dropped it for its size first, and
// the code of a signed one's body. errors.Is matches it against [ErrForbidden] and the
// other signed refusals by its request, its status and its code, against [ErrUnsigned]
// when it is within its bound with no valid signature, and against [ErrTooLarge] when
// it is over its bound. Its text contains the request, the status and the code, never
// a body, a header or a secret.
type Refusal struct {
	// What is the request, as an error states it: "the claim", "the release of dp_…".
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
	// valid signature, or a server error. Such a try may have taken effect: a release's
	// 410 gone may follow a release whose 204 was lost.
	Resent bool

	kind kind
}

func (r *Refusal) Error() string {
	switch {
	case r.TooLarge:
		return fmt.Sprintf("check that the Qory Apiary URL reaches Qory Apiary and no other server: %s: the answer with status %d is larger than %d bytes, so the client dropped it before it read the signature", r.What, r.Status, r.Limit)
	case !r.Signed && r.Status == http.StatusUnauthorized:
		return "check that the access key is approved in Qory Apiary and that this machine's clock is within 300 seconds of the correct time: " +
			r.What + " returned 401 unauthorized, which the client does not take as a revoked key"
	case !r.Signed:
		return fmt.Sprintf("check that the Qory Apiary URL reaches Qory Apiary and that the pin is the one from the enrolment: %s: the answer with status %d has no valid signature under the pin, so the client used nothing in it", r.What, r.Status)
	case r.Code == "":
		return fmt.Sprintf("%s%s: Qory Apiary returned %d", r.fix(), r.What, r.Status)
	case r.RetryAfter > 0:
		return fmt.Sprintf("%s%s: Qory Apiary returned %d %s with a wait of %s", r.fix(), r.What, r.Status, r.Code, r.RetryAfter)
	}
	return fmt.Sprintf("%s%s: Qory Apiary returned %d %s", r.fix(), r.What, r.Status, r.Code)
}

// fix is the start of a signed refusal's text: what the instance admin can do, or that
// the refusal is a defect in the provisioner, or "" for a refusal that the provisioner
// handles with no step for the instance admin, such as 410 gone or 409 machine_exists.
func (r *Refusal) fix() string {
	switch {
	case r.Status == http.StatusBadRequest:
		return "report this defect in the provisioner: "
	case r.Code == "forbidden":
		return "check in Qory Apiary that the access key is this node pool's key and allows the request: "
	case r.Code == "no_gateway":
		return "give the node pool a gateway in Qory Apiary that serves its workspace and has an active access key: "
	case r.Code == "not_found":
		return "check that the Qory Apiary URL is the origin of a Qory Apiary that serves provisioner/v1: "
	}
	return ""
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
// [ErrAnswerUnusable] for a success status the request does not have. A code is read
// only from a body that passes refusal.schema.json. A code that contains what looks
// like a secret is dropped. A 429's Retry-After is read
// from 1 second to 1 hour.
func (c *Client) refused(what string, k kind, ans *answer) error {
	if ans.status >= 200 && ans.status < 300 {
		return unusable(what, fmt.Sprintf("its status is %d, which the contract does not have for this request", ans.status))
	}
	r := &Refusal{What: what, Status: ans.status, Signed: ans.signed, Resent: ans.resent, kind: k}
	var doc struct {
		Error string `json:"error"`
	}
	if readAnswer("refusal.schema.json", ans.body, &doc) == nil && !secret(doc.Error) {
		r.Code = doc.Error
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
