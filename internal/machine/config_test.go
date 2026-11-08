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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/integrations/contracts"
)

const testToken = "imds-token-marker"

// imdsServer is an IMDSv2 endpoint: PUT /latest/api/token returns a token, and a GET
// with that token returns the value in paths.
type imdsServer struct {
	t     *testing.T
	paths map[string]string
	mu    sync.Mutex
	gets  []string
	all   int
}

func (s *imdsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.all++
	s.mu.Unlock()
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

// wantKind fails t unless err is a *adapterio.Failure of kind.
func wantKind(t *testing.T, err error, kind string) {
	t.Helper()
	var e *adapterio.Failure
	if !errors.As(err, &e) || e.Kind != kind {
		t.Errorf("error %#v, want a *adapterio.Failure of kind %s", err, kind)
	}
}

// requests returns how many requests the server received.
func (s *imdsServer) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.all
}

// isolateAWSEnv clears every variable that the SDK reads for the region, the
// credentials and the endpoints, sets HOME to an empty folder, and points LoadConfig
// at the metadata endpoint.
func isolateAWSEnv(t *testing.T, endpoint string) {
	for _, v := range refusedVariables {
		t.Setenv(v.name, "")
	}
	for _, k := range []string{"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_DEFAULTS_MODE", "AWS_EC2_METADATA_SERVICE_ENDPOINT_MODE"} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AWS_EC2_METADATA_V1_DISABLED", "true")
	before := imdsEndpoint
	imdsEndpoint = endpoint
	t.Cleanup(func() { imdsEndpoint = before })
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

// TestLoadConfigRefusesVariables pins that each variable that selects another
// credential source, a config or credentials file, another metadata address, another
// EC2 endpoint or another certificate authority is refused with an error of the kind
// defect that starts with the fix, before any request to the metadata service. The
// error does not contain the variable's value.
func TestLoadConfigRefusesVariables(t *testing.T) {
	const value = "value-marker-41c7"
	want := map[string]string{
		"AWS_ACCESS_KEY_ID":                      "Unset AWS_ACCESS_KEY_ID: the EC2 machine adapter uses only the instance role's credentials",
		"AWS_ACCESS_KEY":                         "Unset AWS_ACCESS_KEY: the EC2 machine adapter uses only the instance role's credentials",
		"AWS_SECRET_ACCESS_KEY":                  "Unset AWS_SECRET_ACCESS_KEY: the EC2 machine adapter uses only the instance role's credentials",
		"AWS_SECRET_KEY":                         "Unset AWS_SECRET_KEY: the EC2 machine adapter uses only the instance role's credentials",
		"AWS_SESSION_TOKEN":                      "Unset AWS_SESSION_TOKEN: the EC2 machine adapter uses only the instance role's credentials",
		"AWS_WEB_IDENTITY_TOKEN_FILE":            "Unset AWS_WEB_IDENTITY_TOKEN_FILE: the EC2 machine adapter uses only the instance role's credentials",
		"AWS_ROLE_ARN":                           "Unset AWS_ROLE_ARN: the EC2 machine adapter uses only the instance role's credentials",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI":     "Unset AWS_CONTAINER_CREDENTIALS_FULL_URI: the EC2 machine adapter uses only the instance role's credentials",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "Unset AWS_CONTAINER_CREDENTIALS_RELATIVE_URI: the EC2 machine adapter uses only the instance role's credentials",
		"AWS_PROFILE":                            "Unset AWS_PROFILE: the EC2 machine adapter reads no AWS config or credentials file",
		"AWS_DEFAULT_PROFILE":                    "Unset AWS_DEFAULT_PROFILE: the EC2 machine adapter reads no AWS config or credentials file",
		"AWS_CONFIG_FILE":                        "Unset AWS_CONFIG_FILE: the EC2 machine adapter reads no AWS config or credentials file",
		"AWS_SHARED_CREDENTIALS_FILE":            "Unset AWS_SHARED_CREDENTIALS_FILE: the EC2 machine adapter reads no AWS config or credentials file",
		"AWS_EC2_METADATA_SERVICE_ENDPOINT":      "Unset AWS_EC2_METADATA_SERVICE_ENDPOINT: the EC2 machine adapter reads the instance role's credentials from the instance metadata service at its standard address",
		"AWS_EC2_METADATA_DISABLED":              "Unset AWS_EC2_METADATA_DISABLED: the EC2 machine adapter reads the instance role's credentials from the instance metadata service at its standard address",
		"AWS_ENDPOINT_URL":                       "Unset AWS_ENDPOINT_URL: the EC2 machine adapter calls EC2 only at its standard endpoint, with the instance role's credentials",
		"AWS_ENDPOINT_URL_EC2":                   "Unset AWS_ENDPOINT_URL_EC2: the EC2 machine adapter calls EC2 only at its standard endpoint, with the instance role's credentials",
		"AWS_ENDPOINT_URL_PRICING":               "Unset AWS_ENDPOINT_URL_PRICING: the EC2 machine adapter calls the AWS Price List Query API only at its standard endpoint in us-east-1",
		"AWS_CA_BUNDLE":                          "Unset AWS_CA_BUNDLE: the EC2 machine adapter checks the EC2 and price list endpoints' certificates only against the host's certificate authorities",
	}
	if len(want) != len(refusedVariables) {
		t.Fatalf("%d refused variables, want %d", len(refusedVariables), len(want))
	}
	for name, message := range want {
		t.Run(name, func(t *testing.T) {
			srv, s := newIMDS(t, rolePaths())
			isolateAWSEnv(t, srv.URL)
			t.Setenv("AWS_REGION", "test-region-1")
			t.Setenv(name, value)
			_, err := LoadConfig(context.Background())
			if err == nil || err.Error() != message {
				t.Fatalf("error %v, want %q", err, message)
			}
			wantKind(t, err, contracts.FailureDefect)
			if strings.Contains(err.Error(), value) {
				t.Error("the error contains the variable's value")
			}
			if n := s.requests(); n != 0 {
				t.Errorf("%d requests to the metadata service, want 0", n)
			}
		})
	}
}

// TestLoadConfigIgnoresSharedFiles pins that the config and credentials files in
// HOME, and AWS_DEFAULT_REGION, change neither the region, the credentials, the
// metadata address nor the EC2 endpoint.
func TestLoadConfigIgnoresSharedFiles(t *testing.T) {
	const keyID, secret = "AKIAFILEMARKER0001", "file-secret-marker-0c2d"
	paths := rolePaths()
	paths["/latest/dynamic/instance-identity/document"] = `{"region": "test-region-2", "instanceId": "i-0123456789abcdef0"}`
	srv, _ := newIMDS(t, paths)
	isolateAWSEnv(t, srv.URL)
	t.Setenv("AWS_DEFAULT_REGION", "test-region-8")
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a request reached the endpoint of the shared config file")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(other.Close)
	dir := filepath.Join(os.Getenv("HOME"), ".aws")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"config":      "[default]\nregion = test-region-9\nendpoint_url = " + other.URL + "\nec2_metadata_service_endpoint = " + other.URL + "\ncredential_process = /nonexistent/credential-process\n",
		"credentials": "[default]\naws_access_key_id = " + keyID + "\naws_secret_access_key = " + secret + "\n",
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := LoadConfig(context.Background())
	if err != nil || cfg.Region != "test-region-2" {
		t.Fatalf("region %q, error %v", cfg.Region, err)
	}
	creds, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil || creds.Source != "EC2RoleProvider" || creds.AccessKeyID != roleKeyID {
		t.Errorf("credentials from %q with the key id %q, error %v", creds.Source, creds.AccessKeyID, err)
	}
	if cfg.BaseEndpoint != nil {
		t.Errorf("the configuration has the endpoint %q", *cfg.BaseEndpoint)
	}
	if e := ec2.NewFromConfig(cfg).Options().BaseEndpoint; e != nil {
		t.Errorf("the EC2 client has the endpoint %q", *e)
	}
}

// TestLoadConfigDefaultsModeAuto pins that AWS_DEFAULTS_MODE=auto does not apply: the
// configuration has the defaults mode legacy and no runtime environment, which auto
// would set from a region read from the instance metadata, and the metadata service
// gets only the requests for the credentials.
func TestLoadConfigDefaultsModeAuto(t *testing.T) {
	paths := rolePaths()
	paths["/latest/dynamic/instance-identity/document"] = `{"region": "test-region-2", "instanceId": "i-0123456789abcdef0"}`
	srv, s := newIMDS(t, paths)
	isolateAWSEnv(t, srv.URL)
	t.Setenv("AWS_REGION", "test-region-1")
	t.Setenv("AWS_DEFAULTS_MODE", "auto")
	cfg, err := LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultsMode != aws.DefaultsModeLegacy || cfg.RuntimeEnvironment != (aws.RuntimeEnvironment{}) {
		t.Errorf("defaults mode %q, runtime environment %+v; want legacy and none", cfg.DefaultsMode, cfg.RuntimeEnvironment)
	}
	if n := s.documentReads(); n != 0 {
		t.Errorf("%d reads of the region from the metadata, want 0", n)
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
	wantKind(t, err, contracts.FailureOther)
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
	wantKind(t, err, contracts.FailureOther)
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
