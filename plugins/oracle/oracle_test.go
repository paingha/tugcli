package oracle

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/pkg/conformance"
	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/internal/tugtest"
)

const (
	tenancyID  = "ocid1.tenancy.oc1..aaaaaaaatenancy"
	userID     = "ocid1.user.oc1..aaaaaaaauser"
	prodID     = "ocid1.compartment.oc1..aaaaaaaaprod"
	noAccessID = "ocid1.compartment.oc1..aaaaaaaanoaccess"
	trainID    = "ocid1.cluster.oc1.iad.aaaaaaaatrain"
	creatingID = "ocid1.cluster.oc1.iad.aaaaaaaacreating"
	inferID    = "ocid1.cluster.oc1.iad.aaaaaaaainfer"
	legacyID   = "ocid1.cluster.oc1.eu-frankfurt-1.aaaaaaaalegacy"

	ashburn   = "us-ashburn-1"
	frankfurt = "eu-frankfurt-1"
)

const testCA = "-----BEGIN CERTIFICATE-----\nMIIDiDCCAnCgAwIBAgIRAMQJ\n-----END CERTIFICATE-----\n"

// keys are RSA keys for the tests, generated once because it is slow.
var keys = sync.OnceValue(func() [2]*rsa.PrivateKey {
	var out [2]*rsa.PrivateKey
	for i := range out {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		out[i] = k
	}
	return out
})

// apiKeyPEM encodes a key as `oci setup keys` writes it, with the line the
// console adds after it.
func apiKeyPEM(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return append(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), "OCI_API_KEY\n"...)
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ociHome writes ~/.oci/config with these profiles, and the API key the
// DEFAULT profile names, into a new home directory.
func ociHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	key := keys()[0]
	writeFile(t, filepath.Join(home, ".oci", "oci_api_key.pem"), apiKeyPEM(t, key))
	config := `# written by oci setup config
[DEFAULT]
user=` + userID + `
fingerprint=` + fingerprint(&key.PublicKey) + `
key_file=~/.oci/oci_api_key.pem
tenancy=` + tenancyID + `
region=` + ashburn + `

[CI]
region = ` + frankfurt + `

; a profile from oci session authenticate
[SESSION]
fingerprint=` + fingerprint(&key.PublicKey) + `
key_file=~/.oci/sessions/SESSION/oci_api_key.pem
security_token_file=~/.oci/sessions/SESSION/token
`
	writeFile(t, filepath.Join(home, ".oci", "config"), []byte(config))
	return home
}

func testProvider(home string, env map[string]string) *Provider {
	return &Provider{
		now:    time.Now,
		getenv: func(k string) string { return env[k] },
		home:   func() (string, error) { return home, nil },
	}
}

// fixture is one cluster the fake serves.
type fixture struct {
	region, compartment string
	summary             string
	// servers maps the kubeconfig endpoint types to the server they give.
	servers map[string]string
}

// The clusters follow ClusterSummary in the Container Engine API 20180222:
// train has a public and a private endpoint, infer only a private one,
// legacy only the endpoint of a cluster without native VCN networking, and
// creating none yet.
var fixtures = map[string]fixture{
	trainID: {ashburn, prodID, `{"id":"` + trainID + `","name":"train","compartmentId":"` + prodID + `","vcnId":"ocid1.vcn.oc1.iad.aaaa",
	  "kubernetesVersion":"v1.31.1","lifecycleState":"ACTIVE","lifecycleDetails":"","freeformTags":{"env":"prod","team":"ml","Region":"iad"},
	  "definedTags":{"Operations":{"CostCenter":"42"}},"endpoints":{"kubernetes":null,"publicEndpoint":"203.0.113.10:6443",
	  "privateEndpoint":"10.0.0.10:6443","vcnHostnameEndpoint":"train.k8sapi.vcn.oraclevcn.com:6443"},
	  "endpointConfig":{"subnetId":"ocid1.subnet.oc1.iad.aaaa","isPublicIpEnabled":true},"type":"ENHANCED_CLUSTER"}`,
		map[string]string{"PUBLIC_ENDPOINT": "https://203.0.113.10:6443", "PRIVATE_ENDPOINT": "https://10.0.0.10:6443"}},
	creatingID: {ashburn, prodID, `{"id":"` + creatingID + `","name":"creating","compartmentId":"` + prodID + `",
	  "kubernetesVersion":"v1.32.1","lifecycleState":"CREATING","lifecycleDetails":"Creating","endpoints":null,"type":"BASIC_CLUSTER"}`, nil},
	inferID: {ashburn, prodID, `{"id":"` + inferID + `","name":"infer","compartmentId":"` + prodID + `",
	  "kubernetesVersion":"v1.30.1","lifecycleState":"UPDATING","freeformTags":{},
	  "endpoints":{"privateEndpoint":"10.0.1.20:6443"},"endpointConfig":{"isPublicIpEnabled":false},"type":"ENHANCED_CLUSTER"}`,
		map[string]string{"PRIVATE_ENDPOINT": "https://10.0.1.20:6443"}},
	legacyID: {frankfurt, tenancyID, `{"id":"` + legacyID + `","name":"legacy","compartmentId":"` + tenancyID + `",
	  "kubernetesVersion":"v1.29.1","lifecycleState":"ACTIVE",
	  "endpoints":{"kubernetes":"ctest.eu-frankfurt-1.clusters.oci.oraclecloud.com:6443"},"type":"BASIC_CLUSTER"}`,
		map[string]string{"LEGACY_KUBERNETES": "https://ctest.eu-frankfurt-1.clusters.oci.oraclecloud.com:6443"}},
}

const nodePoolsJSON = `[
  {"id":"ocid1.nodepool.oc1.iad.aaaagpu","clusterId":"` + trainID + `","compartmentId":"` + prodID + `","name":"h100",
   "kubernetesVersion":"v1.31.1","nodeShape":"BM.GPU.H100.8","lifecycleState":"ACTIVE"},
  {"id":"ocid1.nodepool.oc1.iad.aaaacpu","clusterId":"` + trainID + `","compartmentId":"` + prodID + `","name":"system",
   "kubernetesVersion":"v1.31.1","nodeShape":"VM.Standard.E5.Flex","lifecycleState":"ACTIVE"},
  {"id":"ocid1.nodepool.oc1.iad.aaaainfer","clusterId":"` + inferID + `","compartmentId":"` + prodID + `","name":"cpu",
   "kubernetesVersion":"v1.30.1","nodeShape":"VM.Standard.E4.Flex","lifecycleState":"ACTIVE"}
]`

// fakeOCI serves the Identity and Container Engine calls the plugin makes,
// at /<service>/<region>/..., and checks every request's signature as the
// OCI request signing reference describes.
type fakeOCI struct {
	t   *testing.T
	srv *httptest.Server
	pub *rsa.PublicKey

	rateLimit bool

	mu        sync.Mutex
	calls     []string
	endpoints map[string]string // the kubeconfig endpoint type asked for, by cluster
}

func newFake(t *testing.T) *fakeOCI {
	t.Helper()
	f := &fakeOCI{t: t, pub: &keys()[0].PublicKey, endpoints: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /identity/{region}/20160918/compartments", f.compartments)
	mux.HandleFunc("GET /containerengine/{region}/20180222/clusters", f.listClusters)
	mux.HandleFunc("GET /containerengine/{region}/20180222/clusters/{id}", f.getCluster)
	mux.HandleFunc("GET /containerengine/{region}/20180222/nodePools", f.nodePools)
	mux.HandleFunc("POST /containerengine/{region}/20180222/clusters/{id}/kubeconfig/content", f.kubeconfig)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("opc-request-id", "F2A1D0C8B7E6/0A1B2C3D")
		if err := verify(r, body, f.pub); err != nil {
			f.t.Logf("%s %s: %v", r.Method, r.URL, err)
			apiErr(w, http.StatusUnauthorized, "NotAuthenticated", "The required information to complete authentication was not provided or was incorrect.")
			return
		}
		if f.rateLimit {
			w.Header().Set("Retry-After", "20")
			apiErr(w, http.StatusTooManyRequests, "TooManyRequests", "User-rate limit exceeded.")
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOCI) apiURL() string { return f.srv.URL + "/{service}/{region}" }

var signatureParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// verify checks a request's signature: the scheme, key ID and algorithm,
// that the required headers are signed and hold the right values, and the
// RSA-SHA256 signature over them.
func verify(r *http.Request, body []byte, pub *rsa.PublicKey) error {
	auth, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Signature ")
	if !ok {
		return errors.New("no Signature authorization")
	}
	params := map[string]string{}
	for _, m := range signatureParam.FindAllStringSubmatch(auth, -1) {
		params[m[1]] = m[2]
	}
	if params["version"] != "1" || params["algorithm"] != "rsa-sha256" ||
		params["keyId"] != tenancyID+"/"+userID+"/"+fingerprint(pub) {
		return fmt.Errorf("signature parameters %v", params)
	}
	headers := strings.Fields(params["headers"])
	want := []string{"date", "(request-target)", "host"}
	if r.Method == http.MethodPost {
		want = append(want, "content-length", "content-type", "x-content-sha256")
		sum := sha256.Sum256(body)
		if got := r.Header.Get("X-Content-Sha256"); got != base64.StdEncoding.EncodeToString(sum[:]) {
			return fmt.Errorf("x-content-sha256 %q does not match the body", got)
		}
	}
	for _, h := range want {
		if !slices.Contains(headers, h) {
			return fmt.Errorf("%s is not signed", h)
		}
	}
	date, err := http.ParseTime(r.Header.Get("Date"))
	if err != nil || time.Since(date).Abs() > 5*time.Minute {
		return fmt.Errorf("date %q is missing or more than 5 minutes out", r.Header.Get("Date"))
	}
	lines := make([]string, len(headers))
	for i, h := range headers {
		var v string
		switch h {
		case "(request-target)":
			v = strings.ToLower(r.Method) + " " + r.RequestURI
		case "host":
			v = r.Host
		case "content-length":
			v = strconv.Itoa(len(body))
		default:
			v = r.Header.Get(h)
		}
		lines[i] = h + ": " + v
	}
	sig, err := base64.StdEncoding.DecodeString(params["signature"])
	if err != nil {
		return err
	}
	hashed := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, hashed[:], sig)
}

func apiErr(w http.ResponseWriter, status int, code, message string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"code":%q,"message":%q}`, code, message)
}

func (f *fakeOCI) compartments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("compartmentId") != tenancyID || q.Get("compartmentIdInSubtree") != "true" || q.Get("accessLevel") != "ACCESSIBLE" {
		apiErr(w, http.StatusBadRequest, "InvalidParameter", "unexpected query "+r.URL.RawQuery)
		return
	}
	fmt.Fprint(w, `[
	  {"compartmentId":"`+tenancyID+`","id":"`+prodID+`","name":"prod","description":"Production","lifecycleState":"ACTIVE","timeCreated":"2026-01-05T10:00:00.000Z"},
	  {"compartmentId":"`+prodID+`","id":"`+noAccessID+`","name":"restricted","description":"","lifecycleState":"ACTIVE","timeCreated":"2026-02-05T10:00:00.000Z"}
	]`)
}

func (f *fakeOCI) listClusters(w http.ResponseWriter, r *http.Request) {
	q, region := r.URL.Query(), r.PathValue("region")
	comp := q.Get("compartmentId")
	if comp == noAccessID {
		apiErr(w, http.StatusNotFound, "NotAuthorizedOrNotFound", "Authorization failed or requested resource not found.")
		return
	}
	if q.Get("limit") != "100" {
		apiErr(w, http.StatusBadRequest, "InvalidParameter", "want limit=100")
		return
	}
	// Ashburn's prod compartment comes in two pages.
	var ids []string
	switch {
	case region == ashburn && comp == prodID && q.Get("page") == "":
		ids = []string{trainID, creatingID}
		w.Header().Set("opc-next-page", "AAAAAAAApage2")
	case region == ashburn && comp == prodID && q.Get("page") == "AAAAAAAApage2":
		ids = []string{inferID}
	default:
		for id, fx := range fixtures {
			if fx.region == region && fx.compartment == comp {
				ids = append(ids, id)
			}
		}
	}
	items := make([]string, len(ids))
	for i, id := range ids {
		items[i] = fixtures[id].summary
	}
	fmt.Fprint(w, "["+strings.Join(items, ",")+"]")
}

func (f *fakeOCI) getCluster(w http.ResponseWriter, r *http.Request) {
	fx, ok := fixtures[r.PathValue("id")]
	if !ok || fx.region != r.PathValue("region") {
		apiErr(w, http.StatusNotFound, "NotAuthorizedOrNotFound", "Authorization failed or requested resource not found.")
		return
	}
	fmt.Fprint(w, fx.summary)
}

func (f *fakeOCI) nodePools(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("region") == ashburn && r.URL.Query().Get("compartmentId") == prodID {
		fmt.Fprint(w, nodePoolsJSON)
		return
	}
	fmt.Fprint(w, "[]")
}

func (f *fakeOCI) kubeconfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var details struct {
		TokenVersion string `json:"tokenVersion"`
		Endpoint     string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&details); err != nil || details.TokenVersion != "2.0.0" {
		apiErr(w, http.StatusBadRequest, "CannotParseRequest", "want tokenVersion 2.0.0")
		return
	}
	fx, ok := fixtures[id]
	server := fx.servers[details.Endpoint]
	if !ok || fx.region != r.PathValue("region") || server == "" {
		apiErr(w, http.StatusNotFound, "NotAuthorizedOrNotFound", "Authorization failed or requested resource not found.")
		return
	}
	f.mu.Lock()
	f.endpoints[id] = details.Endpoint
	f.mu.Unlock()
	// The API answers with the kubeconfig as YAML, exec entry and all.
	name := id[len(id)-11:]
	w.Header().Set("Content-Type", "application/octet-stream")
	fmt.Fprintf(w, `---
apiVersion: v1
kind: ""
clusters:
  - name: "cluster-%[1]s"
    cluster:
      server: "%[2]s"
      certificate-authority-data: "%[3]s"
users:
  - name: "user-%[1]s"
    user:
      exec:
        apiVersion: "client.authentication.k8s.io/v1beta1"
        command: "oci"
        args:
          - "ce"
          - "cluster"
          - "generate-token"
          - "--cluster-id"
          - "%[4]s"
          - "--region"
          - "%[5]s"
        env: []
contexts:
  - name: "context-%[1]s"
    context:
      cluster: "cluster-%[1]s"
      user: "user-%[1]s"
current-context: "context-%[1]s"
`, name, server, base64.StdEncoding.EncodeToString([]byte(testCA)), id, fx.region)
}

func (f *fakeOCI) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func account(cfg map[string]any) provider.Account {
	return provider.Account{ID: "work", Config: cfg}
}

func delegateArgs(cluster, region string, extra ...string) []string {
	return append([]string{"ce", "cluster", "generate-token", "--cluster-id", cluster, "--region", region}, extra...)
}

func TestList(t *testing.T) {
	f := newFake(t)
	p := testProvider(ociHome(t), nil)
	clusters, err := p.List(context.Background(), provider.ListRequest{Account: account(map[string]any{
		"apiURL": f.apiURL(), ConfigRegions: ashburn + ", " + frankfurt,
	})})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(clusters))
	for i, c := range clusters {
		names[i] = c.Name
	}
	if !slices.Equal(names, []string{"train", "infer", "legacy"}) {
		t.Fatalf("List = %v, want train, infer and legacy (not the one being created)", names)
	}
	train, infer, legacy := clusters[0], clusters[1], clusters[2]

	if train.ID != trainID || train.Region != ashburn || train.Version != "1.31.1" ||
		train.Endpoints.Public != "https://203.0.113.10:6443" || train.Endpoints.Private != "https://10.0.0.10:6443" ||
		string(train.CAData) != testCA {
		t.Errorf("train = %+v", train)
	}
	wantLabels := map[string]string{"env": "prod", "team": "ml", "gpu": "true"}
	if len(train.Labels) != len(wantLabels) {
		t.Errorf("train labels = %v, want %v (no region tag)", train.Labels, wantLabels)
	}
	for k, v := range wantLabels {
		if train.Labels[k] != v {
			t.Errorf("train label %s = %q, want %q", k, train.Labels[k], v)
		}
	}
	if infer.Endpoints.Public != "" || infer.Endpoints.Private != "https://10.0.1.20:6443" || infer.Labels["gpu"] != "" {
		t.Errorf("infer = %+v, want only a private endpoint and no gpu label", infer)
	}
	if legacy.Region != frankfurt || legacy.Endpoints.Public != "https://ctest.eu-frankfurt-1.clusters.oci.oraclecloud.com:6443" {
		t.Errorf("legacy = %+v", legacy)
	}

	for _, c := range clusters {
		e := c.Credential
		if e == nil || e.Type != provider.CredentialDelegate || e.Exec.Command != CLI || e.Exec.APIVersion != execAPIVersion ||
			!slices.Equal(e.Exec.Args, delegateArgs(c.ID, c.Region, "--profile", "DEFAULT")) || e.Exec.InstallHint == "" {
			t.Errorf("%s: inline credential = %+v, want the generate-token delegate", c.Name, e)
		}
	}

	wantEndpoints := map[string]string{trainID: "PUBLIC_ENDPOINT", inferID: "PRIVATE_ENDPOINT", legacyID: "LEGACY_KUBERNETES"}
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, want := range wantEndpoints {
		if got := f.endpoints[id]; got != want {
			t.Errorf("kubeconfig of %s asked for %q, want %q", id, got, want)
		}
	}
	if !slices.Contains(f.calls, "GET /identity/"+ashburn+"/20160918/compartments") {
		t.Errorf("calls %v do not list the compartments", f.calls)
	}
}

func TestListCompartmentsSetting(t *testing.T) {
	f := newFake(t)
	p := testProvider(ociHome(t), nil)
	ctx := context.Background()
	clusters, err := p.List(ctx, provider.ListRequest{Account: account(map[string]any{"apiURL": f.apiURL(), ConfigCompartments: prodID})})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 || clusters[0].ID != trainID || clusters[1].ID != inferID {
		t.Fatalf("List = %+v, want train and infer from the prod compartment in the profile's region", clusters)
	}
	for _, call := range f.requests() {
		if strings.Contains(call, "/identity/") || !strings.Contains(call, "/"+ashburn+"/") {
			t.Errorf("call %s, want Container Engine calls in %s only", call, ashburn)
		}
	}

	// A compartment the account names must be listed, unlike one the plugin
	// found.
	_, err = p.List(ctx, provider.ListRequest{Account: account(map[string]any{"apiURL": f.apiURL(), ConfigCompartments: prodID + "," + noAccessID})})
	if pe := provider.AsError(err); pe.Code != provider.NotFound || !strings.Contains(pe.Message, "NotAuthorizedOrNotFound") || !strings.Contains(pe.Hint, "use clusters") {
		t.Errorf("List with a compartment the user may not see = %+v, want NotFound with a policy hint", pe)
	}
}

func TestCredential(t *testing.T) {
	f := newFake(t)
	home := ociHome(t)
	ctx := context.Background()
	custom := filepath.Join(home, ".oci", "config")
	tests := []struct {
		name    string
		env     map[string]string
		cfg     map[string]any
		cluster string
		want    []string
		calls   int
	}{
		{"profile's region", nil, nil, trainID, delegateArgs(trainID, ashburn, "--profile", "DEFAULT"), 0},
		{"profile setting", nil, map[string]any{ConfigProfile: "CI"}, legacyID, delegateArgs(legacyID, frankfurt, "--profile", "CI"), 0},
		{"profile from the environment", map[string]string{EnvProfile: "CI"}, nil, legacyID, delegateArgs(legacyID, frankfurt, "--profile", "CI"), 0},
		{"config file setting", nil, map[string]any{ConfigFile: custom}, trainID, delegateArgs(trainID, ashburn, "--profile", "DEFAULT", "--config-file", custom), 0},
		{"config file from the environment", map[string]string{EnvConfigFile: custom}, nil, trainID, delegateArgs(trainID, ashburn, "--profile", "DEFAULT", "--config-file", custom), 0},
		{"looked up in each region", nil, map[string]any{ConfigRegions: ashburn + "," + frankfurt}, legacyID, delegateArgs(legacyID, frankfurt, "--profile", "DEFAULT"), 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(f.requests())
			cfg := map[string]any{"apiURL": f.apiURL()}
			for k, v := range tt.cfg {
				cfg[k] = v
			}
			c, err := testProvider(home, tt.env).Credential(ctx, provider.CredentialRequest{Account: account(cfg), ClusterID: tt.cluster})
			if err != nil {
				t.Fatal(err)
			}
			if c.Type != provider.CredentialDelegate || c.Exec.Command != CLI || !slices.Equal(c.Exec.Args, tt.want) || c.ExpiresAt != nil {
				t.Errorf("Credential = %+v, exec %+v, want args %v", c, c.Exec, tt.want)
			}
			if got := len(f.requests()) - before; got != tt.calls {
				t.Errorf("Credential made %d API calls, want %d", got, tt.calls)
			}
		})
	}

	both := account(map[string]any{"apiURL": f.apiURL(), ConfigRegions: ashburn + "," + frankfurt})
	if _, err := testProvider(home, nil).Credential(ctx, provider.CredentialRequest{Account: both, ClusterID: "ocid1.cluster.oc1.iad.gone"}); provider.CodeOf(err) != provider.NotFound {
		t.Errorf("Credential(gone) = %v, want NotFound", err)
	}
	if _, err := testProvider(home, nil).Credential(ctx, provider.CredentialRequest{Account: both, ClusterID: "train"}); provider.CodeOf(err) != provider.NotFound {
		t.Errorf("Credential(train) = %v, want NotFound for an ID that is not an OCID", err)
	}
}

func TestAPIErrors(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	acct := account(map[string]any{"apiURL": f.apiURL(), ConfigCompartments: prodID})

	// A key the API does not know, though the profile's fingerprint is its
	// own.
	other := t.TempDir()
	writeFile(t, filepath.Join(other, "key.pem"), apiKeyPEM(t, keys()[1]))
	writeFile(t, filepath.Join(other, "config"), []byte("[DEFAULT]\nuser="+userID+"\nfingerprint="+fingerprint(&keys()[1].PublicKey)+
		"\nkey_file=key.pem\ntenancy="+tenancyID+"\nregion="+ashburn+"\n"))
	_, err := testProvider(other, map[string]string{EnvConfigFile: filepath.Join(other, "config")}).List(ctx, provider.ListRequest{Account: acct})
	if pe := provider.AsError(err); pe.Code != provider.Unauthenticated || !strings.Contains(pe.Message, "NotAuthenticated") || !strings.Contains(pe.Hint, "fingerprint") {
		t.Errorf("List with an unknown key = %+v, want Unauthenticated with a hint", pe)
	}

	f.rateLimit = true
	_, err = testProvider(ociHome(t), nil).List(ctx, provider.ListRequest{Account: acct})
	if pe := provider.AsError(err); pe.Code != provider.RateLimited || pe.RetryAfterSeconds != 20 || pe.Hint == "" {
		t.Errorf("List over the rate limit = %+v, want RateLimited after 20 seconds", pe)
	}
}

func TestSettings(t *testing.T) {
	home := ociHome(t)
	tests := []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{"plain http to a remote host", map[string]any{"apiURL": "http://{service}.{region}.example.com"}, "https"},
		{"region that is not an identifier", map[string]any{ConfigRegions: "US East"}, "region identifier"},
		{"compartment that is not an OCID", map[string]any{ConfigCompartments: "prod"}, "compartment OCID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := testProvider(home, nil).List(context.Background(), provider.ListRequest{Account: account(tt.cfg)})
			if pe := provider.AsError(err); pe.Code != provider.InvalidConfig || !strings.Contains(pe.Message, tt.want) {
				t.Errorf("List = %+v, want InvalidConfig about %q", pe, tt.want)
			}
		})
	}
}

func TestProfiles(t *testing.T) {
	key := keys()[0]
	fp := fingerprint(&key.PublicKey)
	der := x509.MarshalPKCS1PrivateKey(key)
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
	legacyEncrypted := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Headers: map[string]string{
		"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-128-CBC,00112233445566778899AABBCCDDEEFF",
	}, Bytes: der})
	pkcs8Encrypted := pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: der})
	profile := func(extra string) string {
		return "[DEFAULT]\nuser=" + userID + "\nfingerprint=" + fp + "\nkey_file=key.pem\ntenancy=" + tenancyID + "\n" + extra
	}
	tests := []struct {
		name    string
		config  string // "" for no config file
		key     []byte
		cfg     map[string]any
		code    provider.Code
		message string
		hint    string
	}{
		{"no config file", "", nil, nil, provider.Unauthenticated, "no OCI config file", "oci setup config"},
		{"unknown profile", profile("region=" + ashburn), pkcs1, map[string]any{ConfigProfile: "ADMIN"}, provider.InvalidConfig, "no profile ADMIN", "DEFAULT"},
		{"session token", profile("security_token_file=token\n"), pkcs1, nil, provider.InvalidConfig, "session token", "API key"},
		{"missing entry", "[DEFAULT]\nuser=" + userID + "\nkey_file=key.pem\ntenancy=" + tenancyID + "\n", pkcs1, nil, provider.InvalidConfig, "no fingerprint", "oci setup config"},
		{"pass_phrase entry", profile("pass_phrase=example\nregion=" + ashburn), pkcs1, nil, provider.InvalidConfig, "passphrase", "openssl rsa"},
		{"encrypted PKCS #1 key", profile("region=" + ashburn), legacyEncrypted, nil, provider.InvalidConfig, "passphrase", "openssl rsa"},
		{"encrypted PKCS #8 key", profile("region=" + ashburn), pkcs8Encrypted, nil, provider.InvalidConfig, "passphrase", "openssl rsa"},
		{"not a key", profile("region=" + ashburn), []byte("ssh-rsa AAAA tug@example\n"), nil, provider.InvalidConfig, "no PEM private key", "key_file"},
		{"missing key file", profile("region=" + ashburn), nil, nil, provider.InvalidConfig, "reading the API key", "key_file"},
		{"wrong fingerprint", strings.Replace(profile("region="+ashburn), fp, "00:11:22:33:44:55:66:77:88:99:aa:bb:cc:dd:ee:ff", 1), pkcs1, nil,
			provider.InvalidConfig, "fingerprint " + fp, "fingerprint"},
		{"no region", profile(""), pkcs1, nil, provider.InvalidConfig, "no region", ConfigRegions},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			env := map[string]string{EnvConfigFile: filepath.Join(dir, "config")}
			if tt.config != "" {
				writeFile(t, filepath.Join(dir, "config"), []byte(tt.config))
			}
			if tt.key != nil {
				writeFile(t, filepath.Join(dir, "key.pem"), tt.key)
			}
			_, err := testProvider(dir, env).List(context.Background(), provider.ListRequest{Account: account(tt.cfg)})
			pe := provider.AsError(err)
			if pe.Code != tt.code || !strings.Contains(pe.Message, tt.message) || !strings.Contains(pe.Hint, tt.hint) {
				t.Errorf("List = %+v, want %s with %q and a hint about %q", pe, tt.code, tt.message, tt.hint)
			}
		})
	}
}

func TestPKCS1Key(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	key := keys()[0]
	writeFile(t, filepath.Join(dir, "keys", "admin.pem"), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	writeFile(t, filepath.Join(dir, "config"), []byte("[ADMIN]\nuser="+userID+"\nfingerprint="+strings.ToUpper(fingerprint(&key.PublicKey))+
		"\nkey_file=keys/admin.pem\ntenancy="+tenancyID+"\nregion="+ashburn+"\n"))
	acct := account(map[string]any{"apiURL": f.apiURL(), ConfigFile: filepath.Join(dir, "config"), ConfigProfile: "ADMIN", ConfigCompartments: prodID})
	clusters, err := testProvider(t.TempDir(), nil).List(context.Background(), provider.ListRequest{Account: acct})
	if err != nil || len(clusters) != 2 {
		t.Fatalf("List with a PKCS #1 key relative to the config file = %d clusters, %v, want 2", len(clusters), err)
	}
}

func TestAccounts(t *testing.T) {
	home := ociHome(t)
	got, err := testProvider(home, nil).Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "DEFAULT" || got[1].ID != "CI" ||
		got[1].Config[ConfigProfile] != "CI" || got[1].Description != "OCI CLI profile CI ("+frankfurt+")" || got[1].Config[ConfigFile] != nil {
		t.Errorf("Accounts = %+v, want DEFAULT and CI, without the session token profile", got)
	}

	path := filepath.Join(home, ".oci", "config")
	got, err = testProvider(t.TempDir(), map[string]string{EnvConfigFile: path}).Accounts(context.Background())
	if err != nil || len(got) != 2 || got[0].Config[ConfigFile] != path {
		t.Errorf("Accounts with %s = %+v, %v, want the config file in each account", EnvConfigFile, got, err)
	}

	got, err = testProvider(t.TempDir(), nil).Accounts(context.Background())
	if err != nil || len(got) != 0 || got == nil {
		t.Errorf("Accounts without a config file = %#v, %v, want an empty list", got, err)
	}
}

func TestParseConfig(t *testing.T) {
	cfg, err := parseConfig(strings.NewReader("ignored=before any profile\n[DEFAULT]\n  user = u1 \n# comment\n; comment\nregion=r1\n\n[B]\nregion=r2\n[B]\nkey_file=k\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.order, []string{"DEFAULT", "B"}) {
		t.Errorf("profiles = %v, want DEFAULT and B", cfg.order)
	}
	b, ok := cfg.profile("B")
	if !ok || b["user"] != "u1" || b["region"] != "r2" || b["key_file"] != "k" {
		t.Errorf("profile B = %v, want user from DEFAULT and its own region and key_file", b)
	}
	if _, ok := cfg.profile("C"); ok {
		t.Error("profile C exists")
	}
}

func TestExpandPath(t *testing.T) {
	root := "/"
	if runtime.GOOS == "windows" {
		root = `C:\`
	}
	home, dir := filepath.Join(root, "home", "me"), filepath.Join(root, "etc", "oci")
	tests := []struct {
		in   string
		want string
	}{
		{"~/.oci/key.pem", filepath.Join(home, ".oci", "key.pem")},
		{"keys/admin.pem", filepath.Join(dir, "keys", "admin.pem")},
		{filepath.Join(root, "abs", "key.pem"), filepath.Join(root, "abs", "key.pem")},
		{"~other/key.pem", filepath.Join(dir, "~other", "key.pem")},
	}
	if runtime.GOOS == "windows" {
		tests = append(tests,
			struct{ in, want string }{`%HOMEPATH%\.oci\key.pem`, filepath.Join(home, ".oci", "key.pem")},
			struct{ in, want string }{`%USERPROFILE%\.oci\key.pem`, filepath.Join(home, ".oci", "key.pem")})
	}
	for _, tt := range tests {
		if got := expandPath(tt.in, home, dir); got != tt.want {
			t.Errorf("expandPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSign(t *testing.T) {
	k := &apiKey{tenancy: tenancyID, user: userID, fingerprint: fingerprint(&keys()[0].PublicKey), key: keys()[0]}
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	s := &signer{key: k, now: func() time.Time { return now }}

	get, _ := http.NewRequest(http.MethodGet, "https://containerengine.us-ashburn-1.oci.oraclecloud.com/20180222/clusters?compartmentId=ocid1.compartment.oc1..a&limit=100", nil)
	if err := s.sign(get); err != nil {
		t.Fatal(err)
	}
	if got := get.Header.Get("Date"); got != "Fri, 20 Nov 2026 12:00:00 GMT" {
		t.Errorf("Date = %q", got)
	}
	if auth := get.Header.Get("Authorization"); !strings.HasPrefix(auth, `Signature version="1",keyId="`+k.keyID()+`",algorithm="rsa-sha256",headers="date (request-target) host",signature="`) {
		t.Errorf("GET Authorization = %q", auth)
	}

	body := `{"tokenVersion":"2.0.0","endpoint":"PUBLIC_ENDPOINT"}`
	post, _ := http.NewRequest(http.MethodPost, "https://containerengine.us-ashburn-1.oci.oraclecloud.com/20180222/clusters/x/kubeconfig/content", strings.NewReader(body))
	if err := s.sign(post); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(post.Header.Get("Authorization"), `headers="date (request-target) host content-length content-type x-content-sha256"`) ||
		post.Header.Get("Content-Type") != "application/json" {
		t.Errorf("POST headers = %v", post.Header)
	}
	// The signature checks out against the request as a server receives it.
	for _, req := range []*http.Request{get, post} {
		var data []byte
		if req.Body != nil {
			data, _ = io.ReadAll(req.Body)
		}
		req.RequestURI, req.Host = req.URL.RequestURI(), req.URL.Host
		s.now = time.Now
		if err := s.sign(req); err != nil {
			t.Fatal(err)
		}
		if err := verify(req, data, &keys()[0].PublicKey); err != nil {
			t.Errorf("%s: %v", req.Method, err)
		}
	}
}

func TestManifestMatchesInfo(t *testing.T) {
	b, err := os.ReadFile("plugin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Metadata struct{ Name, Version string }
		Spec     struct {
			Capabilities provider.Capability
			Secrets      []provider.SecretSpec
			Env          []string
		}
	}
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	info := New().Info()
	if m.Metadata.Name != info.Name || m.Metadata.Version != info.Version ||
		m.Spec.Capabilities != provider.Capabilities(New()) || !slices.Equal(m.Spec.Secrets, info.Secrets) ||
		!slices.Equal(m.Spec.Env, []string{EnvConfigFile, EnvProfile}) {
		t.Errorf("plugin.yaml = %+v, want Info() = %+v", m, info)
	}
}

func TestConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the plugin")
	}
	f := newFake(t)
	// The plugin finds the config file through the environment, and the
	// config names the key by its full path, so the test can leave HOME,
	// where go build keeps its caches, alone.
	key := keys()[0]
	writeConfig := func(dir string) string {
		writeFile(t, filepath.Join(dir, "key.pem"), apiKeyPEM(t, key))
		writeFile(t, filepath.Join(dir, "config"), []byte("[DEFAULT]\nuser="+userID+"\nfingerprint="+fingerprint(&key.PublicKey)+
			"\nkey_file="+filepath.Join(dir, "key.pem")+"\ntenancy="+tenancyID+"\nregion="+ashburn+"\n\n[CI]\nregion="+frankfurt+"\n"))
		return filepath.Join(dir, "config")
	}
	t.Setenv(EnvConfigFile, writeConfig(t.TempDir()))
	bin := filepath.Join(t.TempDir(), "tug-provider-oracle")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/tug-provider-oracle").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	r := conformance.Run(context.Background(), conformance.Target{Command: bin}, conformance.Options{
		Name:            Name,
		Account:         account(map[string]any{"apiURL": f.apiURL(), ConfigRegions: ashburn + "," + frankfurt}),
		DeclaredSecrets: New().Info().Secrets,
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("the oracle plugin does not conform")
	}

	// In tug's sandbox the plugin may read ~/.oci, where `oci setup config`
	// puts the config and the key, so that run gets a home with them there.
	// tug is built first, while HOME still leads go to its caches.
	tug := tugtest.Build(t)
	home := t.TempDir()
	writeConfig(filepath.Join(home, ".oci"))
	t.Setenv("HOME", home)
	t.Setenv(EnvConfigFile, "")
	tugtest.ConformInSandbox(t, tugtest.Plugin{Tug: tug, Name: Name, Manifest: "plugin.yaml", Bin: bin,
		Settings: map[string]any{"apiURL": f.apiURL(), ConfigRegions: ashburn + "," + frankfurt}})
}
