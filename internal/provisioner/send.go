package provisioner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qoryai/forager/accesskey"
	foragercontracts "github.com/qoryai/forager/contracts"
	"github.com/qoryai/integrations/contracts"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// request is one request of the client: what it is, as an error states it, the
// refusals that apply, its URL, the contract revision it implements, the schema its
// body must pass before it is sent, and of which contract, the bound of its answer's
// 200, its timeout, and its body with the time set. A request with get set is a GET
// with no body and no schema, its time the X-Qory-Timestamp it is signed with; any
// other is a POST.
type request struct {
	what     string
	kind     kind
	url      string
	revision int
	schema   string
	forager  bool
	get      bool
	limit    int64
	timeout  time.Duration
	body     func(time string) any
}

func (r request) method() string {
	if r.get {
		return http.MethodGet
	}
	return http.MethodPost
}

// answer is the answer to one try: its status, its body, whether its signature
// verified under the pin, its Retry-After, the X-Qory-Configuration of an answer that
// verified, and whether it was over its bound, limit bytes, in which case the client
// read neither its body nor its signature.
type answer struct {
	status        int
	body          []byte
	signed        bool
	retryAfter    string
	configuration string
	tooLarge      bool
	limit         int64
	// resent is whether the try followed one whose answer was lost, see
	// [Refusal.Resent].
	resent bool
}

// retryLine is the line logged before a try is sent again.
const retryLine = "a try failed, so the client sends the request again after the wait"

// do sends req until Qory Apiary returns a signed status below 500, or a handover's
// signed 503 unavailable, up to [tries] tries. Each try is built anew, its time from
// [Client.nextTime], so its bytes and its signature are new. A try fails with no
// answer, with an answer over its bound or with no valid signature, whatever its
// status, a 429 among them, or with any other signed server error: such a try changes
// no state, and the client sends the request again after a wait that doubles from
// [firstWait]. After the last try, the failure is the error. Once ctx is done, its
// error is the error, and the client logs and sends nothing more. The body of every
// answer that do does not return is cleared.
func (c *Client) do(ctx context.Context, req request) (*answer, error) {
	var b []byte
	if !req.get {
		var err error
		if b, err = json.Marshal(req.body("")); err != nil {
			return nil, fmt.Errorf("%s: the client could not build the request", req.what)
		}
	}
	id := req.method() + " " + req.url + "\n" + string(b)
	if accesskey.ContainsSecret(id) {
		return nil, fmt.Errorf("%s: the request contains what looks like an access key secret or a run key, so the client did not send it", req.what)
	}
	wait := firstWait
	for try := 1; ; try++ {
		t, err := c.nextTime(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", req.what, err)
		}
		body, err := c.build(req, t)
		if err != nil {
			return nil, err
		}
		ans, err := c.send(ctx, req, body, t)
		if ans != nil {
			ans.resent = try > 1
		}
		var failure error
		switch {
		case err != nil:
			failure = err
		case ans.tooLarge:
			failure = &Refusal{What: req.what, Status: ans.status, TooLarge: true, Limit: ans.limit, Resent: ans.resent, kind: req.kind}
		case !ans.signed:
			failure = &Refusal{What: req.what, Status: ans.status, Resent: ans.resent, kind: req.kind}
		case ans.status >= 500:
			failure = c.refused(req.what, req.kind, ans)
			if errors.Is(failure, ErrUnavailable) {
				return ans, nil
			}
		default:
			return ans, nil
		}
		if ans != nil {
			clear(ans.body)
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%s: %w", req.what, err)
		}
		if try == tries {
			return nil, failure
		}
		if c.Log != nil {
			attrs := []any{slog.String("request", req.what), slog.Int("try", try), slog.Duration("wait", wait)}
			if ans != nil {
				attrs = append(attrs, slog.Int("status", ans.status))
			}
			c.Log.Warn(retryLine, append(attrs, slog.String("cause", shown(failure.Error())))...)
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, fmt.Errorf("%s: %w", req.what, err)
		}
		wait *= 2
	}
}

// build is req's body with the time t, checked for what looks like a secret and
// against its schema before it is sent; a GET has none. Its error contains the request
// and the schema, never a value.
func (c *Client) build(req request, t time.Time) ([]byte, error) {
	if req.get {
		return nil, nil
	}
	b, err := json.Marshal(req.body(contracts.FormatTime(t)))
	if err != nil {
		return nil, fmt.Errorf("%s: the client could not build the request", req.what)
	}
	if accesskey.ContainsSecret(string(b)) {
		return nil, fmt.Errorf("%s: the request contains what looks like an access key secret or a run key, so the client did not send it", req.what)
	}
	compile := provisionerSchema
	if req.forager {
		compile = foragerSchema
	}
	s, err := compile(req.schema)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", req.what, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil || s.Validate(doc) != nil {
		return nil, fmt.Errorf("%s: the request fails %s, so the client did not send it", req.what, req.schema)
	}
	return b, nil
}

// send signs one try and sends it, a POST of body or a GET whose X-Qory-Timestamp,
// signed with it, is t, then reads the answer: a 200's body up to req.limit, any other
// up to [MaxAnswer]. A larger answer, whatever its status, is marked too large, and its
// body and its signature are not read. A 401 is never signed; the signature of any
// other answer is checked under the pin, bound to this try's signature, with each
// signed header sent at most once. An error is no answer, [ErrUnreachable]; it contains
// the request and never a header, a body or a secret.
func (c *Client) send(ctx context.Context, req request, body []byte, t time.Time) (*answer, error) {
	var content io.Reader
	if !req.get {
		content = bytes.NewReader(body)
	}
	hr, err := http.NewRequestWithContext(ctx, req.method(), req.url, content)
	if err != nil {
		return nil, fmt.Errorf("%s: the client could not build the request", req.what)
	}
	signed := accesskey.Request{AccessKeyID: c.cfg.KeyID, InstanceID: c.cfg.InstanceID, Method: req.method(), Target: hr.URL.RequestURI()}
	if req.get {
		signed.Timestamp = accesskey.Timestamp(t)
		hr.Header.Set(accesskey.HeaderTimestamp, signed.Timestamp)
	} else {
		signed.Body = body
		hr.Header.Set("Content-Type", "application/json")
	}
	sig, err := c.cfg.Key.SignRequest(signed)
	if err != nil {
		return nil, fmt.Errorf("%s: the client could not sign the request", req.what)
	}
	hr.Header.Set("User-Agent", UserAgent)
	hr.Header.Set(accesskey.HeaderAccessKeyID, c.cfg.KeyID)
	hr.Header.Set(accesskey.HeaderInstanceID, c.cfg.InstanceID)
	hr.Header.Set(headerContractVersion, strconv.Itoa(req.revision))
	hr.Header.Set(accesskey.HeaderSignature, sig)
	hc := http.Client{}
	if c.HTTP != nil {
		hc = *c.HTTP
	}
	hc.Timeout = req.timeout
	hc.Jar = nil
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(hr)
	if err != nil {
		return nil, unreachable(req.what, err)
	}
	defer resp.Body.Close()
	limit := int64(MaxAnswer)
	if resp.StatusCode == http.StatusOK {
		limit = req.limit
	}
	b, tooLarge, err := readLimited(resp.Body, resp.ContentLength, limit)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: the client could not read the answer", req.what, ErrUnreachable)
	}
	if tooLarge {
		return &answer{status: resp.StatusCode, tooLarge: true, limit: limit}, nil
	}
	ans := &answer{status: resp.StatusCode, body: b, retryAfter: resp.Header.Get("Retry-After")}
	if resp.StatusCode == http.StatusUnauthorized {
		return ans, nil
	}
	one := func(name string) (string, bool) {
		v := resp.Header.Values(name)
		return strings.Join(v, ""), len(v) <= 1
	}
	signature, ok1 := one(accesskey.HeaderSignature)
	conf, ok2 := one(accesskey.HeaderConfiguration)
	run, ok3 := one(accesskey.HeaderRunConfiguration)
	ans.signed = ok1 && ok2 && ok3 && c.cfg.Pin.VerifyAnswer(accesskey.Answer{
		Status: resp.StatusCode, RequestSignature: sig, Body: b, Configuration: conf, RunConfiguration: run,
	}, signature)
	if ans.signed {
		ans.configuration = conf
	}
	return ans, nil
}

// unreachable is [ErrUnreachable] for the request what, with the transport's cause,
// unless the cause's text contains what looks like a secret, which drops the cause.
func unreachable(what string, err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		err = u.Err
	}
	if accesskey.ContainsSecret(err.Error()) {
		return fmt.Errorf("%s: %w", what, ErrUnreachable)
	}
	return fmt.Errorf("%s: %w: %w", what, ErrUnreachable, err)
}

// readLimited reads r, whose Content-Length is length or -1, up to limit bytes, into
// one buffer it makes at the start, so a growing read leaves no copy of the body
// behind. tooLarge is a body over limit, which it clears and returns none of.
func readLimited(r io.Reader, length, limit int64) (b []byte, tooLarge bool, err error) {
	size := limit + 1
	if length >= 0 && length < size {
		size = length + 1
	}
	buf := make([]byte, size)
	n, err := io.ReadFull(r, buf)
	switch {
	case err == nil && int64(n) > limit:
		clear(buf)
		return nil, true, nil
	case err == nil, errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return buf[:n:n], false, nil
	}
	clear(buf)
	return nil, false, err
}

// errNotOneDocument is a body that is not one JSON document with valid UTF-8 and no
// member twice.
var errNotOneDocument = errors.New("it is not one JSON document with valid UTF-8 and no member twice")

// readAnswer reads body, a signed answer's, into out: one JSON document with valid
// UTF-8 and no member twice in any object, which passes the schema name that compile
// compiles. With out nil it checks body alone. Its error contains the schema, never a
// value of body.
func readAnswer(compile func(string) (*jsonschema.Schema, error), name string, body []byte, out any) error {
	if !jsontext.Value(body).IsValid() {
		return errNotOneDocument
	}
	s, err := compile(name)
	if err != nil {
		return err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil || s.Validate(doc) != nil {
		return fmt.Errorf("it fails %s", name)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("it fails %s", name)
	}
	return nil
}

// schemaCache keeps each compiled schema by its contract and name.
var schemaCache sync.Map

// cached compiles the schema name with compile once, and keeps it under key.
func cached(key, name string, compile func(string) (*jsonschema.Schema, error)) (*jsonschema.Schema, error) {
	if s, ok := schemaCache.Load(key); ok {
		return s.(*jsonschema.Schema), nil
	}
	s, err := compile(name)
	if err != nil {
		return nil, fmt.Errorf("the contract's schema %s does not compile", name)
	}
	schemaCache.Store(key, s)
	return s, nil
}

// provisionerSchema is a schema of provisioner/v1, compiled once.
func provisionerSchema(name string) (*jsonschema.Schema, error) {
	return cached("provisioner/"+name, name, contracts.CompileProvisioner)
}

// foragerSchema is a schema of Forager's contract, compiled once.
func foragerSchema(name string) (*jsonschema.Schema, error) {
	return cached("forager/"+name, name, foragercontracts.Compile)
}

// shown is s, or [hidden] when s contains what looks like a secret, for a value logged.
func shown(s string) string {
	if accesskey.ContainsSecret(s) {
		return hiddenText
	}
	return s
}

// nextTime is the time of a request whose identity, its method, its URL and its body
// built without the time, is id: the clock's second, once that identity was not sent
// within it. Until then, it waits for the clock's next second, so it never runs ahead
// of the clock. Once ctx is done, its error is the error.
func (c *Client) nextTime(ctx context.Context, id string) (time.Time, error) {
	key := sha256.Sum256([]byte(id))
	for {
		t, wait := c.take(key)
		if wait == 0 {
			return t, nil
		}
		if err := c.sleep(ctx, wait); err != nil {
			return time.Time{}, err
		}
		if err := ctx.Err(); err != nil {
			return time.Time{}, err
		}
	}
}

// take reads the clock and, once the clock's second differs from the one last pruned,
// drops every identity sent at another second, after the clock steps back too. When
// the identity key was not sent within the clock's second, take keeps key at that
// second and returns it, with no wait; otherwise it returns the wait until the clock's
// next second. The clock is read under mu, so a read made before another request's is
// never used after it.
func (c *Client) take(key [sha256.Size]byte) (t time.Time, wait time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().UTC()
	t = now.Truncate(time.Second)
	if !t.Equal(c.pruned) {
		for k, last := range c.last {
			if !t.Equal(last) {
				delete(c.last, k)
			}
		}
		c.pruned = t
	}
	if last, held := c.last[key]; held {
		return t, max(last.Add(time.Second).Sub(now), time.Millisecond)
	}
	if c.last == nil {
		c.last = map[[sha256.Size]byte]time.Time{}
	}
	c.last[key] = t
	return t, 0
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
