// Package provisioner is the provisioner's client of Qory Apiary's provisioner
// contract, provisioner/v1, at revision 3: it claims a run, posts that the run's job
// started, takes the handover and releases the run.
//
// The client signs every request with the node pool's access key as Forager's accesskey
// signs one, with X-Qory-Access-Key-Id, X-Qory-Instance-Id, X-Qory-Signature-Ed25519 and
// X-Qory-Contract-Version 3, and checks every body against its schema before it sends
// it. It reads every answer up to its bound, checks its signature under the pin, then
// checks it against its schema and its revision, before it uses anything in it. A
// request's time is the clock's, in whole seconds. A try sent again is built anew with a later time, since Qory Apiary
// returns 401 to a signature it has seen. The client follows no redirect and keeps no
// cookie.
//
// The run key, the inputs and the work item's title are each a [Hidden], which prints,
// logs and marshals as [hidden]. No error the client returns and no line it logs
// contains the access key's secret, a run key, inputs or a title.
package provisioner

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/qoryai/forager/accesskey"
	"github.com/qoryai/integrations/contracts"
)

// Revision is the revision of the provisioner contract the client implements: the one
// its requests send in X-Qory-Contract-Version, and the one a claim's or
// a handover's answer must have. Qory Apiary serves this revision alone.
const Revision = contracts.ProvisionerRevision

// The steps a claim's answer may have.
const (
	StepExplore  = "explore"
	StepPrebuild = "prebuild"
)

// MaxWaitSeconds is the longest wait_seconds a claim may send: Qory Apiary holds a
// claim up to this many seconds while no run can be claimed. ClaimTimeout, a claim's
// timeout, is longer, so a held claim runs to its end; RequestTimeout is the timeout of
// every other request.
const (
	MaxWaitSeconds = 25
	ClaimTimeout   = 40 * time.Second
	RequestTimeout = 30 * time.Second
)

// The bounds of Qory Apiary's answers. The client reads a claim's 200 up to
// MaxClaimAnswer, far more than its members' limits add up to; a handover's 200 up to
// contracts.MaxHandoverAnswer, the contract's bound; every other answer, a refusal or a
// 204, up to MaxAnswer. The client drops a larger answer, whatever its
// status, before it reads the signature, uses nothing in it, and sends the request
// again, built anew, as for an answer with no valid signature.
const (
	MaxClaimAnswer = 64 << 10
	MaxAnswer      = 64 << 10
)

// The tries of one request: tries in all, the waits between them doubling from
// firstWait, so 1, 2, 4 and 8 seconds.
const (
	tries     = 5
	firstWait = time.Second
)

// MaxReason is the most characters a release's reason has; it has at least 1.
const MaxReason = 200

// UserAgent is the User-Agent of every request.
const UserAgent = "qory-provisioner"

// The routes of the provisioner contract.
const (
	routeClaim      = "/v1/pools/claim"
	routeDispatches = "/v1/dispatches/"
)

// headerContractVersion is set to [Revision], the revision a request implements. The
// signature does not cover it; the answers whose content the revision bounds have it
// in their signed body.
const headerContractVersion = "X-Qory-Contract-Version"

var (
	// originShape is Qory Apiary's origin: https, or http to a loopback address, with no
	// path, query, fragment or trailing slash.
	originShape = regexp.MustCompile(`^(https://[^/?#@\s]+|http://(localhost|127\.0\.0\.1|\[::1\])(:[0-9]+)?)$`)
	// dispatchIDShape and runIDShape are the provisioner contract's dispatch_id and
	// run_id. The client checks an id before it puts the id in a path or a body.
	dispatchIDShape = regexp.MustCompile(`^dp_[0-9a-hjkmnp-tv-z]{16}$`)
	runIDShape      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// Config is what a [Client] signs requests and checks answers with. The caller reads
// it from the provisioner's state; the client reads no file.
type Config struct {
	// URL is Qory Apiary's origin: https://, or http:// to a loopback address, with no
	// path and no trailing slash.
	URL string
	// KeyID is the access key's id, ak_ and 16 Crockford base32 characters.
	KeyID string
	// Key is the access key, enrolled for the node pool.
	Key *accesskey.Key
	// Pin are Qory Apiary's public keys from the enrolment: every answer's signature is
	// checked under them.
	Pin accesskey.Pin
	// InstanceID is the provisioner's instance id, from accesskey.NewInstanceID, signed
	// into every request.
	InstanceID string
}

// check returns the first problem of the config, or nil. Its error contains no secret.
func (cfg Config) check() error {
	switch {
	case secret(cfg.URL):
		return errors.New("set the URL of Qory Apiary to its origin: the URL contains what looks like an access key secret, a run key or a run code")
	case !originShape.MatchString(cfg.URL):
		return errors.New("set the URL of Qory Apiary to its origin, https://host or http:// to a loopback address, with no path and no trailing slash")
	case cfg.Key == nil:
		return errors.New("set the access key: the config has none")
	}
	if err := accesskey.CheckID(cfg.KeyID); err != nil {
		return fmt.Errorf("set the access key id from the enrolment: %w", err)
	}
	if err := cfg.Pin.Check(); err != nil {
		return fmt.Errorf("set the pin from the enrolment: %w", err)
	}
	if err := accesskey.CheckInstanceID(cfg.InstanceID); err != nil {
		return fmt.Errorf("set the instance id from accesskey.NewInstanceID: %w", err)
	}
	return nil
}

// Client is the provisioner's client of Qory Apiary. It is safe for concurrent use,
// and is used by its pointer. Within one Client, while the clock does not step back,
// the same bytes are never sent twice: a request with the same method, URL and body as
// one sent within the clock's current second, a try sent again or an identical call,
// waits for the clock's next second first; a distinct request does not wait.
type Client struct {
	cfg Config

	// HTTP is the HTTP client; nil is one with no other setting. The client sets its
	// timeout per request, and uses neither its redirect policy nor its cookie jar.
	HTTP *http.Client
	// Now is the clock a request's time is read from; nil is time.Now. Now must advance
	// across Sleep, and must not block or call the Client.
	Now func() time.Time
	// Sleep waits d or until ctx is done; nil waits on a timer.
	Sleep func(ctx context.Context, d time.Duration) error
	// Log takes a line before each try sent again; nil logs nothing.
	Log *slog.Logger

	// mu guards last, the SHA-256 of each request identity sent within the clock's
	// second pruned and the second it was sent at, and pruned. See [Client.nextTime].
	mu     sync.Mutex
	last   map[[sha256.Size]byte]time.Time
	pruned time.Time
}

// New is a Client for cfg, once cfg passes its checks: an origin for URL, an access key
// and its id, a pin Forager can verify under, and an instance id.
func New(cfg Config) (*Client, error) {
	if err := cfg.check(); err != nil {
		return nil, err
	}
	return &Client{cfg: cfg}, nil
}

// WorkItem is the work item a run is for, definitions.schema.json's work_item: its
// public id, its page in Qory Apiary, and its title, which is [Hidden]. The URL and the
// title are untrusted text.
type WorkItem struct {
	ID    string
	URL   string
	Title Hidden
}

// workItemOf is w with its title hidden, or nil for nil.
func workItemOf(w *contracts.WorkItem) *WorkItem {
	if w == nil {
		return nil
	}
	return &WorkItem{ID: w.ID, URL: w.URL, Title: Hide([]byte(w.Title))}
}

// Claimed is a claim's answer, claim-answer.schema.json, checked: signed under the pin,
// of the schema, and of the revision sent. WorkItem is set for the step explore and nil
// for the step prebuild. Target is nil, and Access empty, when the run has no target;
// Access is "read" or "write" exactly when Target is set. The step prebuild always has
// a target.
type Claimed struct {
	DispatchID string
	RunID      string
	PoolID     string
	// LeaseUntil is when the dispatch's lease lapses: Qory Apiary must return the
	// handover's 200 before then.
	LeaseUntil time.Time
	Step       string
	Access     string
	Target     *contracts.Target
	WorkItem   *WorkItem
}

// Claim sends POST /v1/pools/claim, claim.schema.json, with waitSeconds, 0 to
// [MaxWaitSeconds]: Qory Apiary holds the claim up to waitSeconds while no run can be
// claimed. A signed 204 is no run: ok is false. A signed 200 is the run claimed, once
// checked, see [Claimed]; one of another revision than the one sent is
// [ErrAnswerUnusable]. A refusal is a [*Refusal]: [ErrForbidden], [ErrNoGateway],
// [ErrNotFound] or [ErrRateLimited].
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

// readClaim reads body, a claim's signed 200, into a [Claimed]. The schema checks the
// bytes before they are decoded, since a decoded "access":"" or "target":null reads as
// no target. Its error contains the schema or a revision, never a value of body.
func readClaim(body []byte) (Claimed, error) {
	const name = "claim-answer.schema.json"
	var doc contracts.ClaimAnswer
	if err := readAnswer(name, body, &doc); err != nil {
		return Claimed{}, err
	}
	if doc.Revision != Revision {
		return Claimed{}, errors.New(revisionText(doc.Revision))
	}
	if err := checkTarget(name, doc.Step, doc.Access, doc.Target); err != nil {
		return Claimed{}, err
	}
	lease, err := time.Parse(time.RFC3339, doc.LeaseUntil)
	if err != nil {
		return Claimed{}, errors.New("its lease_until is not a time")
	}
	return Claimed{
		DispatchID: doc.DispatchID, RunID: doc.RunID, PoolID: doc.PoolID, LeaseUntil: lease,
		Step: doc.Step, Access: doc.Access, Target: doc.Target, WorkItem: workItemOf(doc.WorkItem),
	}, nil
}

// checkTarget checks again, on an answer that passed the schema name, what the
// provisioner relies on: a run with no target has no access, a run with a target has
// read or write access, and the step prebuild has a target.
func checkTarget(name, step, access string, target *contracts.Target) error {
	switch {
	case target == nil && access != "",
		target != nil && access != "read" && access != "write",
		target == nil && step == StepPrebuild:
		return fmt.Errorf("it fails %s", name)
	}
	return nil
}

// Dispatched sends POST /v1/dispatches/<dispatchID>/dispatched, dispatched.schema.json:
// the provisioner started the run's job. It must come before the handover. The answer
// is a signed 204. A refusal is a [*Refusal]: [ErrForbidden], [ErrNotFound], [ErrGone],
// [ErrLeaseExpired], after which the contract requires the provisioner to stop the job,
// or [ErrRateLimited].
func (c *Client) Dispatched(ctx context.Context, dispatchID string) error {
	what, err := ofDispatch("dispatched for ", dispatchID)
	if err != nil {
		return err
	}
	return c.toDispatch(ctx, what, kindDispatched, dispatchID, "dispatched", "dispatched.schema.json", func(t string) any {
		return contracts.Dispatched{Version: 1, Time: t}
	})
}

// Release sends POST /v1/dispatches/<dispatchID>/release, release.schema.json: the
// provisioner releases the run, for reason, display text of 1 to [MaxReason]
// characters. Before the handover, the release ends the dispatch: with retry true the
// same run waits for another claim, except a run of the step prebuild whose collection
// and node pool have another prebuild run waiting, which ends; with retry false the run
// ends. After the handover, the release ends the run: with retry true Qory Apiary
// queues a new run, with a new run id and run key, except for the step prebuild, where
// retry true is read as false. The answer is a signed 204. A refusal is a [*Refusal]:
// [ErrForbidden], a dispatch that is unknown or of another node pool; [ErrNotFound], a
// route Qory Apiary does not serve; [ErrGone], a dispatch that ended, a release after
// the run's end among them; [ErrLeaseExpired], a dispatch whose lease lapsed before its
// handover while its run has not ended; or [ErrRateLimited].
func (c *Client) Release(ctx context.Context, dispatchID, reason string, retry bool) error {
	what, err := ofDispatch("the release of ", dispatchID)
	if err != nil {
		return err
	}
	if n := utf8.RuneCountInString(reason); n < 1 || n > MaxReason {
		return fmt.Errorf("%s: give a reason of 1 to %d characters; this one has %d", what, MaxReason, n)
	}
	return c.toDispatch(ctx, what, kindRelease, dispatchID, "release", "release.schema.json", func(t string) any {
		return contracts.Release{Version: 1, Time: t, Reason: reason, Retry: retry}
	})
}

// ofDispatch is the request what, prefix and dispatchID, once dispatchID is of the
// contract's form. Its error does not contain dispatchID, which may be any text.
func ofDispatch(prefix, dispatchID string) (string, error) {
	if !dispatchIDShape.MatchString(dispatchID) {
		return "", fmt.Errorf("report this defect in the provisioner: %sa dispatch: the dispatch id is not dp_ and 16 lower-case Crockford base32 characters", prefix)
	}
	return prefix + dispatchID, nil
}

// toDispatch sends a request about one dispatch, whose id ofDispatch checked, with a
// signed 204 as its answer.
func (c *Client) toDispatch(ctx context.Context, what string, k kind, dispatchID, action, schema string, body func(string) any) error {
	ans, err := c.do(ctx, request{
		what: what, kind: k, path: routeDispatches + dispatchID + "/" + action, schema: schema, limit: MaxAnswer, timeout: RequestTimeout, body: body,
	})
	if err != nil {
		return err
	}
	defer clear(ans.body)
	if ans.status != http.StatusNoContent {
		return c.refused(what, k, ans)
	}
	return nil
}

// revisionText states that an answer has the revision n, not the one sent.
func revisionText(n int) string {
	return fmt.Sprintf("it has the contract's revision %d, not %d", n, Revision)
}
