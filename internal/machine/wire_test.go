package machine

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// testSecret is the static test credentials' secret key. It never appears on the wire.
const testSecret = "marker-secret-0b9e4f"

// reply is one HTTP answer of the EC2 test server.
type reply struct {
	status int
	body   string
}

// ec2Server is an EC2 Query endpoint that records each request's form values and
// returns replies in order, repeating the last one.
type ec2Server struct {
	t       *testing.T
	mu      sync.Mutex
	forms   []url.Values
	headers []http.Header
	replies []reply
}

func (s *ec2Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Errorf("read the request body: %v", err)
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		s.t.Errorf("parse the request body: %v", err)
	}
	if strings.Contains(string(body), testSecret) || strings.Contains(fmt.Sprint(r.Header), testSecret) {
		s.t.Error("the secret key is on the wire")
	}
	s.mu.Lock()
	s.forms = append(s.forms, form)
	s.headers = append(s.headers, r.Header.Clone())
	rep := s.replies[min(len(s.forms), len(s.replies))-1]
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/xml;charset=UTF-8")
	w.WriteHeader(rep.status)
	io.WriteString(w, rep.body)
}

// newWireClient returns a Client with the SDK's EC2 client pointed at a test server
// that returns replies. The retryer is the SDK's standard one with no pause between
// attempts.
func newWireClient(t *testing.T, replies ...reply) (*Client, *ec2Server) {
	s := &ec2Server{t: t, replies: replies}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	cfg := aws.Config{
		Region:       "test-region-1",
		Credentials:  credentials.NewStaticCredentialsProvider("AKIDTESTONLY", testSecret, ""),
		BaseEndpoint: aws.String(srv.URL),
		HTTPClient:   srv.Client(),
		Retryer: func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				o.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })
			})
		},
	}
	return NewFromConfig(cfg), s
}

// errorXML returns an EC2 error body with code.
func errorXML(code string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<Response><Errors><Error><Code>` + code + `</Code><Message>test message</Message></Error></Errors><RequestID>req-1</RequestID></Response>`
}

const runReply = `<?xml version="1.0" encoding="UTF-8"?>
<RunInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <requestId>req-1</requestId>
  <reservationId>r-0123456789abcdef0</reservationId>
  <instancesSet><item>
    <instanceId>i-0123456789abcdef0</instanceId>
    <instanceState><code>0</code><name>pending</name></instanceState>
    <clientToken>dsp-0f3a9c2e-1b7d-4e55-8a10-6c2d9e4b7a01</clientToken>
    <launchTime>2026-10-22T10:00:00.000Z</launchTime>
    <tagSet>
      <item><key>qory:managed</key><value>true</value></item>
      <item><key>qory:pool</key><value>pool-7</value></item>
      <item><key>qory:run</key><value>run-41</value></item>
    </tagSet>
  </item></instancesSet>
</RunInstancesResponse>`

// wantRunForm is the RunInstances request that Create sends for testSpec, without
// the client token, every field as the EC2 Query protocol writes it.
func wantRunForm() url.Values {
	f := url.Values{
		"Action":                            {"RunInstances"},
		"Version":                           {"2016-11-15"},
		"MinCount":                          {"1"},
		"MaxCount":                          {"1"},
		"ImageId":                           {"ami-0123456789abcdef0"},
		"InstanceType":                      {"m7g.large"},
		"SubnetId":                          {"subnet-0123456789abcdef0"},
		"SecurityGroupId.1":                 {"sg-0123456789abcdef0"},
		"InstanceInitiatedShutdownBehavior": {"stop"},
		"MetadataOptions.HttpTokens":        {"required"},
		"MetadataOptions.HttpPutResponseHopLimit": {"1"},
		"MetadataOptions.HttpEndpoint":            {"enabled"},
		"BlockDeviceMapping.1.DeviceName":         {"/dev/sda1"},
		"BlockDeviceMapping.1.Ebs.Encrypted":      {"true"},
		"UserData":                                {base64.StdEncoding.EncodeToString(testSpec().UserData)},
	}
	for i, rt := range []string{"instance", "volume"} {
		p := fmt.Sprintf("TagSpecification.%d.", i+1)
		f.Set(p+"ResourceType", rt)
		f.Set(p+"Tag.1.Key", "qory:managed")
		f.Set(p+"Tag.1.Value", "true")
		f.Set(p+"Tag.2.Key", "qory:pool")
		f.Set(p+"Tag.2.Value", "pool-7")
		f.Set(p+"Tag.3.Key", "qory:run")
		f.Set(p+"Tag.3.Value", "run-41")
	}
	return f
}

// TestWireCreate pins the whole RunInstances request on the wire: every parameter,
// the base64 user data, the tags and the client token, and nothing else.
func TestWireCreate(t *testing.T) {
	c, s := newWireClient(t, reply{200, runReply})
	m, err := c.Create(context.Background(), testSpec())
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "i-0123456789abcdef0" || m.State != StatePending || m.RunID != "run-41" || m.PoolID != "pool-7" || m.DispatchID != testSpec().DispatchID {
		t.Errorf("machine %+v", m)
	}
	want := wantRunForm()
	want.Set("ClientToken", testSpec().DispatchID)
	if !reflect.DeepEqual(s.forms[0], want) {
		t.Errorf("request\n%v\nwant\n%v", s.forms[0], want)
	}
	if got, _ := base64.StdEncoding.DecodeString(s.forms[0].Get("UserData")); string(got) != string(testSpec().UserData) {
		t.Errorf("user data on the wire decodes to %q", got)
	}
	if !strings.HasPrefix(s.headers[0].Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIDTESTONLY/") {
		t.Errorf("Authorization %q", s.headers[0].Get("Authorization"))
	}
}

// TestWireCreateRetrySameToken pins that the SDK's retries of a 503 send the same
// client token, and that Create returns the machine of the third attempt.
func TestWireCreateRetrySameToken(t *testing.T) {
	c, s := newWireClient(t, reply{503, errorXML("Unavailable")}, reply{503, errorXML("RequestLimitExceeded")}, reply{200, runReply})
	if _, err := c.Create(context.Background(), testSpec()); err != nil {
		t.Fatal(err)
	}
	if len(s.forms) != 3 {
		t.Fatalf("%d requests, want 3", len(s.forms))
	}
	for i, f := range s.forms {
		if f.Get("ClientToken") != testSpec().DispatchID {
			t.Errorf("attempt %d: ClientToken %q", i+1, f.Get("ClientToken"))
		}
	}
}

// TestWireCreateErrors pins the kind of each error class through the SDK's real
// retryer and error decoder, and the number of attempts each one gets.
func TestWireCreateErrors(t *testing.T) {
	cases := []struct {
		status   int
		code     string
		kind     Kind
		attempts int
	}{
		{503, "RequestLimitExceeded", KindRetried, 3},
		{500, "InternalError", KindRetried, 3},
		{500, "InsufficientInstanceCapacity", KindRetried, 3},
		{400, "InstanceLimitExceeded", KindLimit, 1},
		{400, "VcpuLimitExceeded", KindLimit, 1},
		{400, "IdempotentParameterMismatch", KindTokenMismatch, 1},
		{400, "IdempotentInstanceTerminated", KindTokenTerminated, 1},
		{400, "InvalidAMIID.NotFound", KindOther, 1},
		{403, "UnauthorizedOperation", KindOther, 1},
	}
	for _, tc := range cases {
		c, s := newWireClient(t, reply{tc.status, errorXML(tc.code)})
		_, err := c.Create(context.Background(), testSpec())
		if Classify(err) != tc.kind || errorCode(err) != tc.code {
			t.Errorf("%s: kind %v, code %q, want %v", tc.code, Classify(err), errorCode(err), tc.kind)
		}
		if len(s.forms) != tc.attempts {
			t.Errorf("%s: %d attempts, want %d", tc.code, len(s.forms), tc.attempts)
		}
	}
}

// TestWireCheckRights pins the dry run on the wire: DryRun=true, a client token the
// SDK sets, which is not the dispatch id, and the rest of Create's request.
func TestWireCheckRights(t *testing.T) {
	c, s := newWireClient(t, reply{412, errorXML("DryRunOperation")})
	if err := c.CheckRights(context.Background(), testSpec()); err != nil {
		t.Fatal(err)
	}
	got := s.forms[0]
	token := got.Get("ClientToken")
	if token == "" || token == testSpec().DispatchID {
		t.Errorf("ClientToken %q, want one the SDK sets", token)
	}
	got.Del("ClientToken")
	want := wantRunForm()
	want.Set("DryRun", "true")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("request\n%v\nwant\n%v", got, want)
	}

	c, _ = newWireClient(t, reply{403, errorXML("UnauthorizedOperation")})
	err := c.CheckRights(context.Background(), testSpec())
	const prefix = "Give the instance role the provisioner's EC2 rights: RunInstances dry run failed with UnauthorizedOperation: "
	if err == nil || !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("error %v, want the prefix %q", err, prefix)
	}
}

// TestWireListAndConsole pins the DescribeInstances filters and pages, and the
// GetConsoleOutput request and its decoding, on the wire.
func TestWireListAndConsole(t *testing.T) {
	page := func(id, next string) string {
		return `<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>req-1</requestId>
<reservationSet><item><reservationId>r-1</reservationId><instancesSet><item>
<instanceId>` + id + `</instanceId><instanceState><code>80</code><name>stopped</name></instanceState>
<clientToken>dsp-1</clientToken><tagSet><item><key>qory:run</key><value>run-41</value></item></tagSet>
</item></instancesSet></item></reservationSet>` + next + `</DescribeInstancesResponse>`
	}
	c, s := newWireClient(t, reply{200, page("i-0aaaaaaaaaaaaaaa0", "<nextToken>page-2</nextToken>")}, reply{200, page("i-0bbbbbbbbbbbbbbb0", "")})
	ms, err := c.List(context.Background(), "pool-7", StateStopped)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].ID != "i-0aaaaaaaaaaaaaaa0" || ms[1].ID != "i-0bbbbbbbbbbbbbbb0" || ms[1].State != StateStopped || ms[1].RunID != "run-41" {
		t.Errorf("machines %+v", ms)
	}
	want := url.Values{
		"Action":           {"DescribeInstances"},
		"Version":          {"2016-11-15"},
		"MaxResults":       {"1000"},
		"Filter.1.Name":    {"tag:qory:managed"},
		"Filter.1.Value.1": {"true"},
		"Filter.2.Name":    {"tag:qory:pool"},
		"Filter.2.Value.1": {"pool-7"},
		"Filter.3.Name":    {"instance-state-name"},
		"Filter.3.Value.1": {"stopped"},
	}
	if !reflect.DeepEqual(s.forms[0], want) {
		t.Errorf("first page\n%v\nwant\n%v", s.forms[0], want)
	}
	want.Set("NextToken", "page-2")
	if !reflect.DeepEqual(s.forms[1], want) {
		t.Errorf("second page\n%v\nwant\n%v", s.forms[1], want)
	}

	const text = "qory-machine-exit status=0 opened=yes\n"
	c, s = newWireClient(t, reply{200, `<GetConsoleOutputResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>req-1</requestId>
<instanceId>i-0123456789abcdef0</instanceId><timestamp>2026-10-22T10:05:00.000Z</timestamp>
<output>` + base64.StdEncoding.EncodeToString([]byte(text)) + `</output></GetConsoleOutputResponse>`})
	got, err := c.Console(context.Background(), "i-0123456789abcdef0")
	if err != nil || got != text {
		t.Errorf("text %q, error %v", got, err)
	}
	wantConsole := url.Values{"Action": {"GetConsoleOutput"}, "Version": {"2016-11-15"}, "InstanceId": {"i-0123456789abcdef0"}, "Latest": {"true"}}
	if !reflect.DeepEqual(s.forms[0], wantConsole) {
		t.Errorf("request\n%v\nwant\n%v", s.forms[0], wantConsole)
	}
}

// TestWireTerminate pins TerminateInstances and the confirming DescribeInstances by
// id on the wire.
func TestWireTerminate(t *testing.T) {
	c, s := newWireClient(t,
		reply{200, `<TerminateInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>req-1</requestId><instancesSet><item><instanceId>i-0123456789abcdef0</instanceId><currentState><code>32</code><name>shutting-down</name></currentState><previousState><code>80</code><name>stopped</name></previousState></item></instancesSet></TerminateInstancesResponse>`},
		reply{200, `<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>req-2</requestId><reservationSet><item><reservationId>r-1</reservationId><instancesSet><item><instanceId>i-0123456789abcdef0</instanceId><instanceState><code>48</code><name>terminated</name></instanceState></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`})
	const id = "i-0123456789abcdef0"
	if err := c.Terminate(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	done, err := c.Terminated(context.Background(), id)
	if err != nil || !done {
		t.Errorf("done %v, error %v", done, err)
	}
	if want := (url.Values{"Action": {"TerminateInstances"}, "Version": {"2016-11-15"}, "InstanceId.1": {id}}); !reflect.DeepEqual(s.forms[0], want) {
		t.Errorf("terminate\n%v\nwant\n%v", s.forms[0], want)
	}
	if want := (url.Values{"Action": {"DescribeInstances"}, "Version": {"2016-11-15"}, "InstanceId.1": {id}}); !reflect.DeepEqual(s.forms[1], want) {
		t.Errorf("describe\n%v\nwant\n%v", s.forms[1], want)
	}
}
