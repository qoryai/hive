package machine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/qoryai/hive/machines"
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

// wantKind fails t unless err is a *machines.Error of kind.
func wantKind(t *testing.T, err error, kind machines.Kind) {
	t.Helper()
	var e *machines.Error
	if !errors.As(err, &e) || e.Kind != kind {
		t.Errorf("error %#v, want a *machines.Error of kind %s", err, kind)
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

// The instance role's test credentials. The key id and the secret are markers that
// no error may contain.
const (
	roleKeyID  = "ASIAROLEMARKER0001"
	roleSecret = "role-secret-marker-7d1c"
	roleToken  = "role-token-marker-55aa"
)

// rolePaths are the metadata paths that give the SDK's role provider the role
// test-role and its credentials.
func rolePaths() map[string]string {
	return map[string]string{
		"/latest/meta-data/iam/security-credentials/":          "test-role",
		"/latest/meta-data/iam/security-credentials/test-role": `{"Code": "Success", "AccessKeyId": "` + roleKeyID + `", "SecretAccessKey": "` + roleSecret + `", "Token": "` + roleToken + `", "Expiration": "2099-01-01T00:00:00Z"}`,
	}
}

// documentReads returns how many times the identity document, which has the region,
// was read.
func (s *imdsServer) documentReads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.gets {
		if p == "/latest/dynamic/instance-identity/document" {
			n++
		}
	}
	return n
}

// TestLoadConfigRegion pins the order: AWS_REGION when it is set, with no read of the
// region from the metadata, and the region from the instance metadata when it is
// not. Both take the credentials from the instance role.
func TestLoadConfigRegion(t *testing.T) {
	paths := rolePaths()
	paths["/latest/dynamic/instance-identity/document"] = `{"region": "test-region-2", "instanceId": "i-0123456789abcdef0"}`
	srv, s := newIMDS(t, paths)
	isolateAWSEnv(t, srv.URL)

	t.Setenv("AWS_REGION", "test-region-1")
	cfg, err := LoadConfig(context.Background())
	if err != nil || cfg.Region != "test-region-1" {
		t.Fatalf("with AWS_REGION: region %q, error %v", cfg.Region, err)
	}
	if n := s.documentReads(); n != 0 {
		t.Errorf("with AWS_REGION: %d reads of the region from the metadata, want 0", n)
	}
	creds, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil || creds.Source != "EC2RoleProvider" || creds.AccessKeyID != roleKeyID {
		t.Errorf("credentials from %q, error %v", creds.Source, err)
	}

	t.Setenv("AWS_REGION", "")
	cfg, err = LoadConfig(context.Background())
	if err != nil || cfg.Region != "test-region-2" {
		t.Errorf("without AWS_REGION: region %q, error %v", cfg.Region, err)
	}
	if n := s.documentReads(); n != 1 {
		t.Errorf("without AWS_REGION: %d reads of the region from the metadata, want 1", n)
	}
}

// TestLoadConfigRefusesOtherCredentials pins that credentials from the environment
// or a shared credentials file are refused, with an error that starts with the fix
// and contains none of the keys, even when the instance role is there too.
func TestLoadConfigRefusesOtherCredentials(t *testing.T) {
	const keyID, secret = "AKIAENVMARKER00001", "env-secret-marker-93be"
	const prefix = "Remove AWS_ACCESS_KEY_ID, AWS_PROFILE and any ~/.aws folder from the EC2 machine adapter's environment, so the AWS SDK uses the instance role: the credentials came from "
	for name, set := range map[string]func(t *testing.T){
		"environment": func(t *testing.T) {
			t.Setenv("AWS_ACCESS_KEY_ID", keyID)
			t.Setenv("AWS_SECRET_ACCESS_KEY", secret)
		},
		"credentials file": func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "credentials")
			if err := os.WriteFile(file, []byte("[default]\naws_access_key_id = "+keyID+"\naws_secret_access_key = "+secret+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("AWS_SHARED_CREDENTIALS_FILE", file)
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := newIMDS(t, rolePaths())
			isolateAWSEnv(t, srv.URL)
			t.Setenv("AWS_REGION", "test-region-1")
			set(t)
			_, err := LoadConfig(context.Background())
			if err == nil || !strings.HasPrefix(err.Error(), prefix) || !strings.HasSuffix(err.Error(), ", not EC2RoleProvider") {
				t.Fatalf("error %v, want the prefix %q", err, prefix)
			}
			wantKind(t, err, machines.KindDefect)
			for _, marker := range []string{keyID, secret, roleKeyID, roleSecret, roleToken} {
				if strings.Contains(err.Error(), marker) {
					t.Errorf("the error contains the marker %q", marker)
				}
			}
		})
	}
}

// TestLoadConfigNoRole pins the error when the metadata has no instance role.
func TestLoadConfigNoRole(t *testing.T) {
	srv, _ := newIMDS(t, nil)
	isolateAWSEnv(t, srv.URL)
	t.Setenv("AWS_REGION", "test-region-1")
	_, err := LoadConfig(context.Background())
	const prefix = "Attach an instance role to the host, and run the EC2 machine adapter with IMDSv2 reachable: retrieving the AWS credentials failed: "
	if err == nil || !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("error %v, want the prefix %q", err, prefix)
	}
	wantKind(t, err, machines.KindOther)
}

// TestLoadConfigNoRegion pins the error when neither the environment nor the
// metadata has a region.
func TestLoadConfigNoRegion(t *testing.T) {
	srv, _ := newIMDS(t, nil)
	isolateAWSEnv(t, srv.URL)
	_, err := LoadConfig(context.Background())
	const prefix = "Set AWS_REGION, or run the EC2 machine adapter on an EC2 instance with IMDSv2 reachable: loading the AWS configuration failed: "
	if err == nil || !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("error %v, want the prefix %q", err, prefix)
	}
	wantKind(t, err, machines.KindOther)
}

// TestLoadConfigEmptyRegion pins the error when the metadata returns an empty region.
func TestLoadConfigEmptyRegion(t *testing.T) {
	srv, _ := newIMDS(t, map[string]string{"/latest/dynamic/instance-identity/document": `{"region": ""}`})
	isolateAWSEnv(t, srv.URL)
	_, err := LoadConfig(context.Background())
	const prefix = "Set AWS_REGION, or run the EC2 machine adapter on an EC2 instance with IMDSv2 reachable: loading the AWS configuration failed: "
	if err == nil || !strings.HasPrefix(err.Error(), prefix) || !strings.Contains(err.Error(), "did not return a region value") {
		t.Errorf("error %v, want the prefix %q and the SDK's cause", err, prefix)
	}
}
