// Package provisionertest is a fake Qory Apiary for the provisioner's tests: an
// httptest server of provisioner/v1: claim, dispatched, handover and release.
//
// It checks every request as Qory Apiary does, in the contract's order: a body over a
// mebibyte gets an unsigned 413, a POST of another content type an unsigned 415, a
// signing header sent twice an unsigned 400 bad_request; then the signature is checked
// under the pool's key, and a signature that fails, or that the fake has seen before,
// gets the unsigned 401. The answers of the next checks are signed under
// [Server.Signer]: an instance id outside its pattern gets a signed 400 bad_request;
// another revision than 3 a signed 400 unsupported_contract_version; a body that fails
// its schema a signed 400 invalid_request. The fake marks a signature as seen once the
// body passes its schema, as Qory Apiary does. A time more than 300 seconds from
// [Server.Now] then gets the unsigned 401. After those checks the fake signs every
// answer: a reply that [Server.Script] queued for the route, else the model's.
//
// The model keeps a queue of runs, which [Server.Offer] fills, and the dispatches it
// made of them. A claim takes the first run waiting into a new dispatch, or is held up
// to its wait_seconds and [Server.Hold] for one, then gets a signed 204. Each
// dispatch's refusals follow the contract's order: 403 for an unknown dispatch, 410 for
// one that ended, 409 handed_over for a second handover, 409 lease_expired once its
// lease lapsed before its handover or [Server.Expire] made it stale, and 409
// not_dispatched for a handover before dispatched. A handover with run_code true gets
// the run-code form of the answer, with the run's [Run.RunCode], and any other
// handover the run-key form. A release ends the dispatch. The
// fake records every request in order, [Server.Calls].
package provisionertest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qoryai/forager/accesskey"
	"github.com/qoryai/integrations/contracts"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/qoryai/hive/internal/provisioner"
)

// The ids of the fake's node pool: its access key's id, the instance id
// [Server.Client] signs with, and the node pool.
const (
	KeyID      = "ak_f1xt0re000000000"
	InstanceID = "i_0123456789abcdefghjkmnpqrs"
	PoolID     = "np_f1xt0re000000000"
)

// Lease is a dispatch's lease when its [Run] has none, Qory Apiary's.
const Lease = 600 * time.Second

// Window is how far a request's time may be from [Server.Now], either way.
const Window = 300 * time.Second

// maxBody is the largest request body the fake reads; a larger one is a 413.
const maxBody = 1 << 20

// Route is one endpoint of the fake.
type Route string

// The fake's routes.
const (
	RouteClaim      Route = "claim"
	RouteDispatched Route = "dispatched"
	RouteHandover   Route = "handover"
	RouteRelease    Route = "release"
	// RouteUnknown is any other path, which gets a signed 404 not_found once its
	// signature verifies.
	RouteUnknown Route = "unknown"
)

// Run is a run waiting in the queue, as [Server.Offer] puts it there. Each member left
// empty is filled in: a new run id, the step explore, read access, the target
// code.example.com acme/shop, the contract fixture's domain, work item for the step
// explore, and inputs of the step, a new run key, and [Lease]. With NoTarget the run has
// no target and no access, and its domain and inputs are the fixture's of an explore
// run with no target. The handover's answer has RunSettings as it is.
type Run struct {
	RunID    string
	Step     string
	Access   string
	Target   *contracts.Target
	NoTarget bool
	Domain   string
	WorkItem *contracts.WorkItem
	// RunSettings is the handover answer's run_settings; nil sends none.
	RunSettings *contracts.RunSettings
	Inputs      json.RawMessage
	// RunKey is the run key of the handover's answer, of the contract's form:
	// qrk_, base64url, a dot and base64url.
	RunKey string
	// RunCode is the run code of the run-code form of the handover's answer, of the
	// contract's form: qecr_, 26 upper-case Crockford base32 characters, a dot and 22
	// base64url characters.
	RunCode string
	Lease   time.Duration
}

// Reply is one scripted answer, which [Server.Script] queues for a route. With Status
// 0 the model makes the answer, and Edit changes its 200's JSON body first; with any
// other Status the model stays as it was, and the answer's body is Body, else a refusal
// of Code, else empty.
type Reply struct {
	Status int
	// Code is a refusal's code, the body {"error": Code} when Body is empty.
	Code string
	Body string
	// Edit changes the model's 200 answer, a claim's or a handover's, before it is
	// signed: another revision, step or run id, say.
	Edit func(doc map[string]any)
	// Unsigned leaves the answer unsigned, and WrongKey signs it under a key that is not
	// pinned.
	Unsigned bool
	WrongKey bool
	// RetryAfter is the answer's Retry-After, and Header more headers of the answer,
	// each value added after the answer's headers: a Location, a Set-Cookie, or a second
	// signature, say.
	RetryAfter string
	Header     http.Header
	// Pad is how many spaces follow the body, to make it larger than its bound.
	Pad int
	// Hold is how long the fake waits before it sends the answer, or until the request
	// is gone.
	Hold time.Duration
	// Drop closes the connection with no answer, after the model acted when Status is 0:
	// an answer lost on its way.
	Drop bool
}

// Call is one request the fake received, and the answer it sent.
type Call struct {
	Route Route
	// Target is the request's path and query, and DispatchID the dispatch in the path.
	Target     string
	DispatchID string
	Header     http.Header
	Body       []byte
	// Time is the body's time; zero when it has none.
	Time time.Time
	// Verified is whether the signature verified under the pool's key, and Replayed
	// whether the fake had seen it before.
	Verified bool
	Replayed bool
	// Status is the answer's status, and Signed whether it was signed under the pin;
	// Dropped is a connection closed with no answer.
	Status  int
	Signed  bool
	Dropped bool
}

// The states of a dispatch.
const (
	StateClaimed    = "claimed"
	StateDispatched = "dispatched"
	StateHandedOver = "handed_over"
	StateEnded      = "ended"
)

// Dispatch is one dispatch the fake made of a claimed run, and what became of it.
type Dispatch struct {
	ID         string
	Run        Run
	LeaseUntil time.Time
	State      string
	// Stale is a dispatch whose lease lapsed before its handover, and HandedOver one
	// whose handover got the 200.
	Stale      bool
	HandedOver bool
	// Release is the release that ended it, if one did.
	Release *contracts.Release
}

// Server is the fake Qory Apiary. Set its fields before the first request.
type Server struct {
	// URL is the server's origin.
	URL string
	// Pool is the pool's access key, whose requests verify, under [KeyID].
	Pool *accesskey.Key
	// Signer signs every signed answer; [Server.Pin] pins it.
	Signer *accesskey.Key
	// Now is the clock a request's time is checked against and a lease runs on; nil is
	// time.Now.
	Now func() time.Time
	// Hold is the longest an empty claim is held, within its wait_seconds.
	Hold time.Duration

	srv     *httptest.Server
	other   *accesskey.Key
	closing chan struct{}

	mu         sync.Mutex
	queue      []Run
	wake       chan struct{}
	dispatches map[string]*Dispatch
	order      []string
	scripts    map[Route][]Reply
	calls      []Call
	seen       map[string]bool
}

// New starts a fake with a new pool key and a new signing key, holding empty claims up
// to a second, and closes it when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		Pool: newKey(t), Signer: newKey(t), Hold: time.Second,
		other: newKey(t), closing: make(chan struct{}), wake: make(chan struct{}),
		dispatches: map[string]*Dispatch{}, scripts: map[Route][]Reply{}, seen: map[string]bool{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL
	t.Cleanup(func() {
		close(s.closing)
		s.srv.Close()
	})
	return s
}

func newKey(t testing.TB) *accesskey.Key {
	t.Helper()
	k, err := accesskey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// Pin pins [Server.Signer].
func (s *Server) Pin() accesskey.Pin {
	return accesskey.Pin{{Alg: "ed25519", PublicKey: s.Signer.PublicKey().String()}}
}

// Config is the client config of the fake: its URL, the pool's key under [KeyID], the
// pin of [Server.Signer], and [InstanceID].
func (s *Server) Config() provisioner.Config {
	return provisioner.Config{URL: s.URL, KeyID: KeyID, Key: s.Pool, Pin: s.Pin(), InstanceID: InstanceID}
}

// Client is a client of [Server.Config], on the fake's clock.
func (s *Server) Client() *provisioner.Client {
	c, err := provisioner.New(s.Config())
	if err != nil {
		panic(err)
	}
	c.Now = s.Now
	return c
}

// Offer queues runs, filled in as listed under [Run], and returns their run ids.
func (s *Server) Offer(runs ...Run) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, r := range runs {
		r = filled(r)
		s.queue = append(s.queue, r)
		ids = append(ids, r.RunID)
	}
	close(s.wake)
	s.wake = make(chan struct{})
	return ids
}

// Script queues replies for route, each answering one request that passed the checks
// before the model.
func (s *Server) Script(route Route, replies ...Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scripts[route] = append(s.scripts[route], replies...)
}

// Expire makes the dispatch stale, as a lease lapsed before its handover.
func (s *Server) Expire(dispatchID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.dispatches[dispatchID]; d != nil && d.State != StateHandedOver && d.State != StateEnded {
		d.Stale = true
	}
}

// End ends the dispatch, as its work item closed or its run ended.
func (s *Server) End(dispatchID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.dispatches[dispatchID]; d != nil {
		d.State = StateEnded
	}
}

// Calls are the requests the fake received, in order.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.calls)
	for i := range out {
		out[i].Header = out[i].Header.Clone()
		out[i].Body = slices.Clone(out[i].Body)
	}
	return out
}

// Routes are the routes of the requests the fake received, in order.
func (s *Server) Routes() []Route {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Route
	for _, c := range s.calls {
		out = append(out, c.Route)
	}
	return out
}

// Dispatches are the dispatches the fake made, in the order of their claims.
func (s *Server) Dispatches() []Dispatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Dispatch
	for _, id := range s.order {
		out = append(out, *s.dispatches[id])
	}
	return out
}

// Dispatch is the dispatch id, if the fake made it.
func (s *Server) Dispatch(id string) (Dispatch, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.dispatches[id]
	if d == nil {
		return Dispatch{}, false
	}
	return *d, true
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// The contract fixtures a run is filled in from, by the fixture's name: the step, or
// [withoutTarget].
var (
	fixtureWorkItem   contracts.WorkItem
	fixtureInputs     = map[string]json.RawMessage{}
	fixtureDomain     = map[string]string{}
	dispatchPathShape = regexp.MustCompile(`^/v1/dispatches/([^/]+)/(dispatched|handover|release)$`)
)

// withoutTarget is the fixture of an explore run with no target.
const withoutTarget = "explore-without-target"

func init() {
	for _, name := range []string{provisioner.StepExplore, provisioner.StepPrebuild, withoutTarget} {
		b, err := fs.ReadFile(contracts.ProvisionerFS, "fixtures/handover-answer/"+name+".json")
		if err != nil {
			panic(err)
		}
		var doc contracts.HandoverAnswer
		if err := json.Unmarshal(b, &doc); err != nil {
			panic(err)
		}
		fixtureInputs[name], fixtureDomain[name] = doc.Inputs, doc.Domain
		if doc.WorkItem != nil && name == provisioner.StepExplore {
			fixtureWorkItem = *doc.WorkItem
		}
	}
}

// filled is r with every empty member filled in, as listed under [Run].
func filled(r Run) Run {
	if r.RunID == "" {
		r.RunID = newRunID()
	}
	if r.Step == "" {
		r.Step = provisioner.StepExplore
	}
	fixture := r.Step
	if r.NoTarget {
		fixture, r.Target, r.Access = withoutTarget, nil, ""
	} else {
		if r.Access == "" {
			r.Access = "read"
		}
		if r.Target == nil {
			r.Target = &contracts.Target{System: "code.example.com", Path: "acme/shop"}
		}
	}
	if r.Domain == "" {
		r.Domain = fixtureDomain[fixture]
		if r.Domain == "" {
			r.Domain = "software"
		}
	}
	if r.WorkItem == nil && r.Step == provisioner.StepExplore {
		w := fixtureWorkItem
		r.WorkItem = &w
	}
	if r.Inputs == nil {
		r.Inputs = fixtureInputs[fixture]
		if r.Inputs == nil {
			r.Inputs = json.RawMessage(`{"version":1}`)
		}
	}
	if r.RunKey == "" {
		r.RunKey = "qrk_" + random(48) + "." + random(64)
	}
	if r.RunCode == "" {
		r.RunCode = newRunCode()
	}
	if r.Lease == 0 {
		r.Lease = Lease
	}
	return r
}

// newRunCode is a new run code, of the form of the contract's run_code: qecr_, 26
// random Crockford base32 characters, a dot and 16 random bytes in base64url.
func newRunCode() string {
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	b := make([]byte, 26)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = crockford[b[i]%32]
	}
	return "qecr_" + string(b) + "." + random(16)
}

// random is n random bytes in base64url.
func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// newRunID is a new run id, a version 4 UUID in the canonical lower-case form.
func newRunID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// crockford are the characters of an id after its prefix.
const crockford = "0123456789abcdefghjkmnpqrstvwxyz"

// newDispatchID is a new dispatch id, dp_ and 16 lower-case Crockford base32 characters.
func newDispatchID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := []byte("dp_")
	for _, c := range b {
		id = append(id, crockford[int(c)%len(crockford)])
	}
	return string(id)
}

// answer is the fake's answer: a status, a body and more headers.
type answer struct {
	status int
	body   []byte
	header http.Header
}

func refusal(status int, code string) answer {
	return answer{status: status, body: []byte(`{"error":"` + code + `"}`)}
}

// serve checks one request in the contract's order and sends its answer.
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	route, id := routeOf(r)
	c := Call{Route: route, Target: r.URL.RequestURI(), DispatchID: id, Header: r.Header.Clone(), Body: body}
	sig := r.Header.Get(accesskey.HeaderSignature)
	c.Verified = r.Header.Get(accesskey.HeaderAccessKeyID) == KeyID && s.Pool.PublicKey().VerifyRequest(accesskey.Request{
		AccessKeyID: KeyID, InstanceID: r.Header.Get(accesskey.HeaderInstanceID), Method: r.Method,
		Target: r.URL.RequestURI(), Body: body,
	}, sig)
	c.Time = timeOf(body)
	s.mu.Lock()
	c.Replayed = s.seen[sig]
	s.mu.Unlock()

	ans, rep, signed := s.decide(r, &c, route, id, body)
	if rep.Hold > 0 {
		select {
		case <-time.After(rep.Hold):
		case <-r.Context().Done():
		case <-s.closing:
		}
	}
	c.Status, c.Signed = ans.status, signed && !rep.Unsigned
	if rep.Drop {
		c.Status, c.Signed, c.Dropped = 0, false, true
	}
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
	if rep.Drop {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		panic(http.ErrAbortHandler)
	}
	b := ans.body
	if rep.Pad > 0 {
		b = append(slices.Clone(b), bytes.Repeat([]byte(" "), rep.Pad)...)
	}
	for k, v := range ans.header {
		w.Header()[k] = v
	}
	if rep.RetryAfter != "" {
		w.Header().Set("Retry-After", rep.RetryAfter)
	}
	if signed && !rep.Unsigned {
		k := s.Signer
		if rep.WrongKey {
			k = s.other
		}
		w.Header().Set(accesskey.HeaderSignature, k.SignAnswer(accesskey.Answer{Status: ans.status, RequestSignature: sig, Body: b}))
	}
	if len(b) > 0 {
		w.Header().Set("Content-Type", "application/json")
	}
	for k, v := range rep.Header {
		for _, x := range v {
			w.Header().Add(k, x)
		}
	}
	w.WriteHeader(ans.status)
	_, _ = w.Write(b)
}

// decide is the answer to one request, the reply it follows, and whether the fake
// signs the answer.
func (s *Server) decide(r *http.Request, c *Call, route Route, id string, body []byte) (answer, Reply, bool) {
	switch {
	case len(body) > maxBody:
		return refusal(http.StatusRequestEntityTooLarge, "payload_too_large"), Reply{}, false
	case r.Method == http.MethodPost && r.Header.Get("Content-Type") != "application/json":
		return refusal(http.StatusUnsupportedMediaType, "unsupported_media_type"), Reply{}, false
	case twice(r.Header):
		return refusal(http.StatusBadRequest, "bad_request"), Reply{}, false
	case !c.Verified || c.Replayed:
		return refusal(http.StatusUnauthorized, "unauthorized"), Reply{}, false
	case accesskey.CheckInstanceID(r.Header.Get(accesskey.HeaderInstanceID)) != nil:
		return refusal(http.StatusBadRequest, "bad_request"), Reply{}, true
	}
	if v := r.Header.Values("X-Qory-Contract-Version"); len(v) != 1 || v[0] != strconv.Itoa(provisioner.Revision) {
		return refusal(http.StatusBadRequest, "unsupported_contract_version"), Reply{}, true
	}
	if route == RouteUnknown || r.Method != http.MethodPost {
		return refusal(http.StatusNotFound, "not_found"), Reply{}, true
	}
	if !validBody(route, body) {
		return refusal(http.StatusBadRequest, "invalid_request"), Reply{}, true
	}
	sig := r.Header.Get(accesskey.HeaderSignature)
	s.mu.Lock()
	c.Replayed = s.seen[sig]
	s.seen[sig] = true
	s.mu.Unlock()
	if c.Replayed {
		return refusal(http.StatusUnauthorized, "unauthorized"), Reply{}, false
	}
	if c.Time.IsZero() || c.Time.Sub(s.now()).Abs() > Window {
		return refusal(http.StatusUnauthorized, "unauthorized"), Reply{}, false
	}
	s.mu.Lock()
	var rep Reply
	if q := s.scripts[route]; len(q) > 0 {
		rep, s.scripts[route] = q[0], q[1:]
	}
	s.mu.Unlock()
	if rep.Status != 0 {
		ans := answer{status: rep.Status, body: []byte(rep.Body)}
		if rep.Body == "" && rep.Code != "" {
			ans = refusal(rep.Status, rep.Code)
		}
		return ans, rep, true
	}
	ans := s.model(r.Context(), route, id, body)
	if rep.Edit != nil && ans.status == http.StatusOK {
		var doc map[string]any
		if json.Unmarshal(ans.body, &doc) == nil {
			rep.Edit(doc)
			ans.body, _ = json.Marshal(doc)
		}
	}
	return ans, rep, true
}

// twice reports whether a header the signature depends on is sent more than once.
func twice(h http.Header) bool {
	for _, name := range []string{accesskey.HeaderAccessKeyID, accesskey.HeaderInstanceID, accesskey.HeaderSignature, accesskey.HeaderTimestamp} {
		if len(h.Values(name)) > 1 {
			return true
		}
	}
	return false
}

// routeOf is the route of r's path, whatever its method, and the dispatch in the path.
func routeOf(r *http.Request) (Route, string) {
	p := r.URL.Path
	switch {
	case p == "/v1/pools/claim":
		return RouteClaim, ""
	}
	if m := dispatchPathShape.FindStringSubmatch(p); m != nil {
		return Route(m[2]), m[1]
	}
	return RouteUnknown, ""
}

// timeOf is a body's time; zero when it has none.
func timeOf(body []byte) time.Time {
	var doc struct {
		Time string `json:"time"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, doc.Time)
	return t
}

// schemaOf is the schema each route's body must pass.
var schemaOf = map[Route]func() (*jsonschema.Schema, error){
	RouteClaim:      sync.OnceValues(func() (*jsonschema.Schema, error) { return contracts.CompileProvisioner("claim.schema.json") }),
	RouteDispatched: sync.OnceValues(func() (*jsonschema.Schema, error) { return contracts.CompileProvisioner("dispatched.schema.json") }),
	RouteHandover:   sync.OnceValues(func() (*jsonschema.Schema, error) { return contracts.CompileProvisioner("handover.schema.json") }),
	RouteRelease:    sync.OnceValues(func() (*jsonschema.Schema, error) { return contracts.CompileProvisioner("release.schema.json") }),
}

// validBody reports whether body is one JSON document with no member twice, with its
// integers written as the contract writes them, that passes the route's schema.
func validBody(route Route, body []byte) bool {
	compile := schemaOf[route]
	if compile == nil || !jsontext.Value(body).IsValid() {
		return false
	}
	s, err := compile()
	if err != nil {
		panic(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil || s.Validate(doc) != nil {
		return false
	}
	return !hasFraction(doc)
}

// hasFraction reports whether a decoded document has a number written with a
// fraction or an exponent, which the contract does not allow for an integer.
func hasFraction(doc any) bool {
	switch v := doc.(type) {
	case json.Number:
		return strings.ContainsAny(string(v), ".eE")
	case map[string]any:
		for _, x := range v {
			if hasFraction(x) {
				return true
			}
		}
	case []any:
		for _, x := range v {
			if hasFraction(x) {
				return true
			}
		}
	}
	return false
}

// model is the model's answer to a request that passed every check.
func (s *Server) model(ctx context.Context, route Route, id string, body []byte) answer {
	if route == RouteClaim {
		return s.claim(ctx, body)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.dispatches[id]
	if d == nil {
		return refusal(http.StatusForbidden, "forbidden")
	}
	if d.State != StateHandedOver && d.State != StateEnded && !s.now().Before(d.LeaseUntil) {
		d.Stale = true
	}
	switch {
	case d.State == StateEnded:
		return refusal(http.StatusGone, "gone")
	case route == RouteHandover && d.State == StateHandedOver:
		return refusal(http.StatusConflict, "handed_over")
	case d.Stale:
		return refusal(http.StatusConflict, "lease_expired")
	}
	switch route {
	case RouteDispatched:
		if d.State == StateClaimed {
			d.State = StateDispatched
		}
		return answer{status: http.StatusNoContent}
	case RouteHandover:
		if d.State != StateDispatched {
			return refusal(http.StatusConflict, "not_dispatched")
		}
		d.State, d.HandedOver = StateHandedOver, true
		ans := answer{status: http.StatusOK, header: http.Header{"Cache-Control": {"no-store"}}}
		var h contracts.Handover
		_ = json.Unmarshal(body, &h)
		if h.RunCode {
			ans.body, _ = json.Marshal(contracts.HandoverAnswer{
				Version: 1, Revision: provisioner.Revision, RunID: d.Run.RunID, RunCode: d.Run.RunCode, Step: d.Run.Step,
			})
			return ans
		}
		ans.body, _ = json.Marshal(contracts.HandoverAnswer{
			Version: 1, Revision: provisioner.Revision, RunID: d.Run.RunID, RunKey: d.Run.RunKey, Step: d.Run.Step,
			Domain: d.Run.Domain, Access: d.Run.Access, Target: d.Run.Target, WorkItem: d.Run.WorkItem,
			RunSettings: d.Run.RunSettings, Inputs: d.Run.Inputs,
		})
		return ans
	}
	var rel contracts.Release
	_ = json.Unmarshal(body, &rel)
	d.State, d.Release = StateEnded, &rel
	return answer{status: http.StatusNoContent}
}

// claim takes the first run waiting into a new dispatch, or holds the claim up to its
// wait_seconds and [Server.Hold] for one, then sends a signed 204.
func (s *Server) claim(ctx context.Context, body []byte) answer {
	var cl contracts.Claim
	_ = json.Unmarshal(body, &cl)
	hold := time.NewTimer(min(time.Duration(cl.WaitSeconds)*time.Second, s.Hold))
	defer hold.Stop()
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			defer s.mu.Unlock()
			run := s.queue[0]
			s.queue = s.queue[1:]
			d := &Dispatch{ID: newDispatchID(), Run: run, LeaseUntil: s.now().Add(run.Lease).UTC().Truncate(time.Second), State: StateClaimed}
			s.dispatches[d.ID] = d
			s.order = append(s.order, d.ID)
			doc := contracts.ClaimAnswer{
				Version: 1, Revision: provisioner.Revision, DispatchID: d.ID, RunID: run.RunID, PoolID: PoolID,
				LeaseUntil: contracts.FormatTime(d.LeaseUntil), Step: run.Step, Access: run.Access, Target: run.Target,
			}
			if run.Step != provisioner.StepPrebuild {
				doc.WorkItem = run.WorkItem
			}
			b, _ := json.Marshal(doc)
			return answer{status: http.StatusOK, body: b}
		}
		wake := s.wake
		s.mu.Unlock()
		select {
		case <-wake:
		case <-hold.C:
			return answer{status: http.StatusNoContent}
		case <-ctx.Done():
			return answer{status: http.StatusNoContent}
		case <-s.closing:
			return answer{status: http.StatusNoContent}
		}
	}
}

// String is the dispatch's id, step and state, never its run key.
func (d Dispatch) String() string {
	return fmt.Sprintf("%s %s %s", d.ID, d.Run.Step, d.State)
}
