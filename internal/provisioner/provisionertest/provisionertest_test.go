package provisionertest_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/qoryai/forager/accesskey"

	"github.com/qoryai/hive/internal/provisioner/provisionertest"
)

// post sends body to path, signed with key as the pool's instance, with the headers
// edit changes, and returns the status, whether the answer verified under the pin, and
// the body.
func post(t *testing.T, s *provisionertest.Server, key *accesskey.Key, path string, body []byte, edit func(http.Header)) (int, bool, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(accesskey.HeaderAccessKeyID, provisionertest.KeyID)
	req.Header.Set(accesskey.HeaderInstanceID, provisionertest.InstanceID)
	req.Header.Set("X-Qory-Contract-Version", "3")
	if edit != nil {
		edit(req.Header)
	}
	sig, err := key.SignRequest(accesskey.Request{AccessKeyID: provisionertest.KeyID, InstanceID: req.Header.Get(accesskey.HeaderInstanceID), Method: http.MethodPost, Target: path, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Add(accesskey.HeaderSignature, sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	signed := s.Pin().VerifyAnswer(accesskey.Answer{Status: resp.StatusCode, RequestSignature: sig, Body: b.Bytes()}, resp.Header.Get(accesskey.HeaderSignature))
	return resp.StatusCode, signed, b.String()
}

// claimAt is a claim's body at t.
func claimAt(t time.Time) []byte {
	return []byte(`{"version":1,"time":"` + t.UTC().Format("2006-01-02T15:04:05Z") + `","wait_seconds":0}`)
}

// TestTheFakeChecksRequestsInOrder sends the fake requests that fail each check, in
// the contract's order: each gets its answer, signed or not as the contract requires. The
// fake's clock stands still at a whole second, so a time 300 seconds either way is
// inside the window and 301 outside it, however long a request takes.
func TestTheFakeChecksRequestsInOrder(t *testing.T) {
	s := provisionertest.New(t)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	other, _ := accesskey.Generate()
	cases := []struct {
		name   string
		key    *accesskey.Key
		body   []byte
		edit   func(http.Header)
		status int
		signed bool
		code   string
	}{
		{"too large", s.Pool, bytes.Repeat([]byte(" "), 1<<20+1), nil, 413, false, "payload_too_large"},
		{"another content type", s.Pool, claimAt(now), func(h http.Header) { h.Set("Content-Type", "text/plain") }, 415, false, "unsupported_media_type"},
		{"a signature twice", s.Pool, claimAt(now), func(h http.Header) { h.Add(accesskey.HeaderSignature, "x") }, 400, false, "bad_request"},
		{"another key", other, claimAt(now), nil, 401, false, "unauthorized"},
		{"an instance id outside its shape", s.Pool, claimAt(now), func(h http.Header) { h.Set(accesskey.HeaderInstanceID, "-x") }, 400, true, "bad_request"},
		{"revision 1", s.Pool, claimAt(now), func(h http.Header) { h.Set("X-Qory-Contract-Version", "1") }, 400, true, "unsupported_contract_version"},
		{"revision 2", s.Pool, claimAt(now), func(h http.Header) { h.Set("X-Qory-Contract-Version", "2") }, 400, true, "unsupported_contract_version"},
		{"no revision", s.Pool, claimAt(now), func(h http.Header) { h.Del("X-Qory-Contract-Version") }, 400, true, "unsupported_contract_version"},
		{"a member the schema does not define", s.Pool, []byte(`{"version":1,"time":"` + now.UTC().Format(time.RFC3339) + `","wait_seconds":0,"x":1}`), nil, 400, true, "invalid_request"},
		{"a member twice", s.Pool, []byte(`{"version":1,"version":1,"time":"` + now.UTC().Format(time.RFC3339) + `","wait_seconds":0}`), nil, 400, true, "invalid_request"},
		{"a fraction", s.Pool, []byte(`{"version":1,"time":"` + now.UTC().Format(time.RFC3339) + `","wait_seconds":0.0}`), nil, 400, true, "invalid_request"},
		{"a time behind", s.Pool, claimAt(now.Add(-301 * time.Second)), nil, 401, false, "unauthorized"},
		{"a time ahead", s.Pool, claimAt(now.Add(301 * time.Second)), nil, 401, false, "unauthorized"},
		{"a time at the window's start", s.Pool, claimAt(now.Add(-300 * time.Second)), nil, 204, true, ""},
		{"a time at the window's end", s.Pool, claimAt(now.Add(300 * time.Second)), nil, 204, true, ""},
		{"in order", s.Pool, claimAt(now), nil, 204, true, ""},
	}
	var sent []byte
	for i, c := range cases {
		// Each body differs, by the spaces after it, so no signature repeats.
		sent = append(c.body, bytes.Repeat([]byte(" "), i)...)
		status, signed, body := post(t, s, c.key, "/v1/pools/claim", sent, c.edit)
		if status != c.status || signed != c.signed || c.code != "" && !strings.Contains(body, `"`+c.code+`"`) {
			t.Errorf("%s: %d signed %v %s, want %d signed %v %s", c.name, status, signed, body, c.status, c.signed, c.code)
		}
	}
	// The request in order once more, its signature seen: the unsigned 401.
	if status, signed, _ := post(t, s, s.Pool, "/v1/pools/claim", sent, nil); status != 401 || signed {
		t.Errorf("a replay: %d signed %v", status, signed)
	}
	if status, signed, _ := post(t, s, s.Pool, "/v1/elsewhere", claimAt(now), nil); status != 404 || !signed {
		t.Errorf("an unknown route: %d signed %v", status, signed)
	}
}
