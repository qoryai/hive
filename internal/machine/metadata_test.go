package machine

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
)

const testToken = "imds-token-marker"

// imdsServer is an IMDSv2 endpoint: PUT /latest/api/token returns a token, and a GET
// with that token returns the value in paths.
type imdsServer struct {
	t     *testing.T
	paths map[string]string
	mu    sync.Mutex
	gets  []string
}

func (s *imdsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut && r.URL.Path == "/latest/api/token" {
		if r.Header.Get("X-Aws-Ec2-Metadata-Token-Ttl-Seconds") == "" {
			s.t.Error("the token request has no TTL header")
		}
		w.Header().Set("X-Aws-Ec2-Metadata-Token-Ttl-Seconds", "300")
		io.WriteString(w, testToken)
		return
	}
	s.mu.Lock()
	s.gets = append(s.gets, r.URL.Path)
	s.mu.Unlock()
	if r.Header.Get("X-Aws-Ec2-Metadata-Token") != testToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	v, ok := s.paths[r.URL.Path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	io.WriteString(w, v)
}

// newIMDS starts an IMDSv2 test server with paths.
func newIMDS(t *testing.T, paths map[string]string) (*httptest.Server, *imdsServer) {
	s := &imdsServer{t: t, paths: paths}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv, s
}

// TestLocalIPv4 pins the read of local-ipv4 through the SDK's client with an IMDSv2
// token, and the errors for an address that is not IPv4 and for a failed read.
func TestLocalIPv4(t *testing.T) {
	srv, s := newIMDS(t, map[string]string{"/latest/meta-data/local-ipv4": "10.0.1.25"})
	md := imds.New(imds.Options{Endpoint: srv.URL})
	addr, err := LocalIPv4(context.Background(), md)
	if err != nil || addr.String() != "10.0.1.25" {
		t.Errorf("address %v, error %v", addr, err)
	}
	if len(s.gets) != 1 || s.gets[0] != "/latest/meta-data/local-ipv4" {
		t.Errorf("reads %v", s.gets)
	}

	for value, want := range map[string]string{
		"fd00::1":   `Check the host's network interface: local-ipv4 in the instance metadata is "fd00::1", not an IPv4 address`,
		"not-an-ip": `Check the host's network interface: local-ipv4 in the instance metadata is "not-an-ip", not an IPv4 address`,
	} {
		srv, _ := newIMDS(t, map[string]string{"/latest/meta-data/local-ipv4": value})
		_, err := LocalIPv4(context.Background(), imds.New(imds.Options{Endpoint: srv.URL}))
		if err == nil || err.Error() != want {
			t.Errorf("%q: error %v, want %q", value, err, want)
		}
	}

	srv, _ = newIMDS(t, nil)
	md = imds.New(imds.Options{Endpoint: srv.URL, Retryer: aws.NopRetryer{}})
	_, err = LocalIPv4(context.Background(), md)
	const prefix = "Run the provisioner on an EC2 instance with IMDSv2 reachable from its container: reading local-ipv4 from the instance metadata failed: "
	if err == nil || !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("error %v, want the prefix %q", err, prefix)
	}
}

// isolateAWSEnv clears every variable and file the SDK's default chain reads, so a
// test reads only what it sets. The metadata endpoint is set to endpoint.
func isolateAWSEnv(t *testing.T, endpoint string) {
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_PROFILE", "AWS_DEFAULT_PROFILE",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_ROLE_ARN", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_EC2_METADATA_DISABLED", "AWS_DEFAULTS_MODE", "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_EC2"} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", empty)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", empty)
	t.Setenv("AWS_EC2_METADATA_SERVICE_ENDPOINT", endpoint)
	t.Setenv("AWS_EC2_METADATA_V1_DISABLED", "true")
}

// TestLoadConfigRegion pins the order: AWS_REGION when it is set, with no metadata
// read, and the region from the instance metadata when it is not.
func TestLoadConfigRegion(t *testing.T) {
	srv, s := newIMDS(t, map[string]string{"/latest/dynamic/instance-identity/document": `{"region": "test-region-2", "instanceId": "i-0123456789abcdef0"}`})
	isolateAWSEnv(t, srv.URL)

	t.Setenv("AWS_REGION", "test-region-1")
	cfg, err := LoadConfig(context.Background())
	if err != nil || cfg.Region != "test-region-1" {
		t.Errorf("with AWS_REGION: region %q, error %v", cfg.Region, err)
	}
	if len(s.gets) != 0 {
		t.Errorf("with AWS_REGION: metadata reads %v, want none", s.gets)
	}

	t.Setenv("AWS_REGION", "")
	cfg, err = LoadConfig(context.Background())
	if err != nil || cfg.Region != "test-region-2" {
		t.Errorf("without AWS_REGION: region %q, error %v", cfg.Region, err)
	}
	if len(s.gets) != 1 || s.gets[0] != "/latest/dynamic/instance-identity/document" {
		t.Errorf("without AWS_REGION: metadata reads %v", s.gets)
	}
}

// TestLoadConfigNoRegion pins the error when neither the environment nor the
// metadata has a region.
func TestLoadConfigNoRegion(t *testing.T) {
	srv, _ := newIMDS(t, nil)
	isolateAWSEnv(t, srv.URL)
	_, err := LoadConfig(context.Background())
	const prefix = "Set AWS_REGION, or run the provisioner on an EC2 instance with IMDSv2 reachable: loading the AWS configuration failed: "
	if err == nil || !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("error %v, want the prefix %q", err, prefix)
	}
}

// TestLoadConfigEmptyRegion pins the error when the metadata returns an empty region.
func TestLoadConfigEmptyRegion(t *testing.T) {
	srv, _ := newIMDS(t, map[string]string{"/latest/dynamic/instance-identity/document": `{"region": ""}`})
	isolateAWSEnv(t, srv.URL)
	_, err := LoadConfig(context.Background())
	const prefix = "Set AWS_REGION, or run the provisioner on an EC2 instance with IMDSv2 reachable: loading the AWS configuration failed: "
	if err == nil || !strings.HasPrefix(err.Error(), prefix) || !strings.Contains(err.Error(), "did not return a region value") {
		t.Errorf("error %v, want the prefix %q and the SDK's cause", err, prefix)
	}
}
