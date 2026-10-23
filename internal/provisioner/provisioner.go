// Package provisioner is the provisioner's client of Qory Apiary's provisioner
// contract, provisioner/v1, at [Revision]: it claims a request for one machine, posts
// that the machine for a dispatch exists, again every minute while it lives, releases a
// dispatch before that, and uploads the machine's log.
//
// The client signs every request with the node pool's access key as Forager's accesskey
// signs one, with X-Qory-Access-Key-Id, X-Qory-Instance-Id, X-Qory-Signature-Ed25519 and
// X-Qory-Contract-Version set to [Revision], and checks every body against its schema
// before it sends it. It reads every answer up to its bound, checks its signature under
// the pin, then checks it against its schema and its revision, before it uses anything
// in it. A request's time is the clock's, in whole seconds. A try sent again is built anew with
// a later time, since Qory Apiary returns 401 to a signature it has seen. The client
// follows no redirect and keeps no cookie.
//
// The run code of a claim's answer is a [Hidden], which prints, logs and marshals as
// [hidden]. No error the client returns, no line it logs and no request it sends
// contains the access key's secret, a run key or a run code.
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

	"github.com/qoryai/forager/accesskey"
	"github.com/qoryai/integrations/contracts"
)

// Revision is the revision of the provisioner contract the client implements: the one
// its requests send in X-Qory-Contract-Version, and the one a claim's answer must have.
// Qory Apiary serves this revision alone.
const Revision = contracts.ProvisionerRevision

// MaxWaitSeconds is the longest wait_seconds a claim may send: Qory Apiary holds a
// claim up to this many seconds while no work can be claimed. ClaimTimeout, a claim's
// timeout, is longer, so a held claim runs to its end; RequestTimeout is the timeout of
// every other request.
const (
	MaxWaitSeconds = 25
	ClaimTimeout   = 40 * time.Second
	RequestTimeout = 30 * time.Second
)

// The bounds of Qory Apiary's answers. The client reads a claim's 200 up to
// MaxClaimAnswer, far more than its members' limits add up to, and every other answer,
// a refusal or a 204, up to MaxAnswer. The client drops a larger answer, whatever its
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

// MaxLog is the most bytes, as UTF-8, of the log a machine log sends,
// machine-log.schema.json. Every request body is at most contracts.MaxRequest bytes, as
// sent.
const MaxLog = 768 << 10

// UserAgent is the User-Agent of every request.
const UserAgent = "qory-provisioner"

// The routes of the provisioner contract.
const (
	routeClaim      = "/v1/pools/claim"
	routeDispatches = "/v1/dispatches/"
)

// headerContractVersion is set to [Revision], the revision a request implements. The
// signature does not cover it; the claim's answer, whose content the revision bounds,
// has it in its signed body.
const headerContractVersion = "X-Qory-Contract-Version"

var (
	// originShape is Qory Apiary's origin: https, or http to a loopback address, with no
	// path, query, fragment or trailing slash.
	originShape = regexp.MustCompile(`^(https://[^/?#@\s]+|http://(localhost|127\.0\.0\.1|\[::1\])(:[0-9]+)?)$`)
	// dispatchIDShape is the provisioner contract's dispatch_id. The client checks an id
	// before it puts the id in a path.
	dispatchIDShape = regexp.MustCompile(`^dp_[0-9a-hjkmnp-tv-z]{16}$`)
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

// revisionText states that an answer has the revision n, not the one sent.
func revisionText(n int) string {
	return fmt.Sprintf("it has the contract's revision %d, not %d", n, Revision)
}
