package cli

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paingha/tugcli/internal/team"
)

// teamServer publishes a team catalog, signed or not, as a team's web
// server would.
type teamServer struct {
	*httptest.Server
	mu    sync.Mutex
	files map[string][]byte
}

func newTeamServer(t *testing.T) *teamServer {
	t.Helper()
	s := &teamServer{files: map[string][]byte{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		b, ok := s.files[r.URL.Path]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(s.Close)
	return s
}

// publish serves doc at /team.yaml with sig, or with doc's own signature
// by priv when sig is nil.
func (s *teamServer) publish(t *testing.T, doc, priv string, sig []byte) {
	t.Helper()
	if sig == nil {
		var err error
		if sig, err = team.Sign([]byte(doc), priv); err != nil {
			t.Fatalf("signing the team catalog: %v", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files["/team.yaml"], s.files["/team.yaml.sig"] = []byte(doc), sig
}

func (s *teamServer) catalogURL() string { return s.URL + "/team.yaml" }

// writeGPUKubeconfig writes a kubeconfig with one context, gpu-train, on
// the fake API server, for an account a team hands out.
func writeGPUKubeconfig(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: %s, certificate-authority-data: %s}
users:
- name: u
  user: {token: %s}
contexts:
- name: gpu-train
  context: {cluster: c, user: u}
current-context: gpu-train
`, srv.URL, b64(ca), secretToken)
	path := filepath.Join(t.TempDir(), "gpu.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// teamCatalogDoc is the payments team's catalog: names and labels for the
// production cluster, an account with a GPU cluster, a plugin index and
// two policies.
func teamCatalogDoc(version int, maxUnlock, gpuPath, indexURL, indexKey string) string {
	return fmt.Sprintf(`apiVersion: tug.team/v1alpha1
kind: TeamCatalog
version: %d
description: Payments platform
accounts:
- provider: kubeconfig
  account: team-gpu
  config: {path: %q}
clusters:
- match: {provider: kubeconfig, providerID: payments-prod}
  name: pay-prod
  aliases: [pp]
  env: prod
  labels: {team: payments}
- match: {provider: kubeconfig, providerID: gpu-train}
  name: gpu
  env: sandbox
pluginIndexes:
- name: acme
  url: %s
  publicKey: %s
policies:
- name: production-read-only
  selector: {env: prod}
  readOnly: true
  maxUnlock: %s
- name: sandbox
  selector: {env: sandbox}
`, version, gpuPath, indexURL, indexKey, maxUnlock)
}

func TestTeamCatalogFlow(t *testing.T) {
	isolate(t)
	api, writes := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, api))
	newSession(t)

	priv, pub, err := team.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	_, indexKey, _ := team.NewKey()
	srv := newTeamServer(t)
	gpuPath := writeGPUKubeconfig(t, api)
	v1 := teamCatalogDoc(1, "10m", gpuPath, srv.URL+"/index.yaml", indexKey)
	srv.publish(t, v1, priv, nil)

	// Without a terminal, trusting a team needs --yes.
	r := tug(t, "team", "add", "platform", srv.catalogURL(), "--public-key", pub)
	if r.code == 0 || !strings.Contains(r.stderr, "pass --yes") || !strings.Contains(r.stderr, "grants no") ||
		!strings.Contains(r.stderr, "+ cluster pay-prod: renamed from payments-prod") {
		t.Fatalf("team add without --yes: %+v", r)
	}
	if out := mustTug(t, "team", "ls", "-o", "name"); strings.TrimSpace(out) != "" {
		t.Fatalf("a team was added without approval: %q", out)
	}
	// A wrong key adds nothing.
	_, otherPub, _ := team.NewKey()
	if r := tug(t, "team", "add", "platform", srv.catalogURL(), "--public-key", otherPub, "--yes"); r.code == 0 ||
		!strings.Contains(r.stderr, "signature does not match") {
		t.Fatalf("team add with the wrong key: %+v", r)
	}

	r = tug(t, "team", "add", "platform", srv.catalogURL(), "--public-key", pub, "--yes")
	if r.code != 0 || !strings.Contains(r.stdout, "Added team platform, version 1") ||
		!strings.Contains(r.stderr, "Not in your catalog yet, named and labelled once tug sync lists them: gpu") ||
		!strings.Contains(r.stderr, "Next: tug sync") {
		t.Fatalf("team add --yes: %+v", r)
	}
	if r := tug(t, "team", "add", "platform", srv.catalogURL(), "--public-key", pub, "--yes"); r.code == 0 || !strings.Contains(r.stderr, "already set up") {
		t.Fatalf("adding a team twice: %+v", r)
	}

	// The team's name, alias and labels are in the catalog, with provenance.
	var clusters []clusterView
	if err := json.Unmarshal([]byte(mustTug(t, "ls", "-o", "json", "-l", "team=payments")), &clusters); err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 || clusters[0].Name != "pay-prod" || clusters[0].Team != "platform" || len(clusters[0].Aliases) != 1 {
		t.Fatalf("ls after team add: %+v", clusters)
	}

	// The policy makes it read-only and limits unlocks.
	if out := mustTug(t, "connect", "pp"); !strings.Contains(out, "pay-prod") || !strings.Contains(out, "read-only") ||
		!strings.Contains(out, "tug unlock 10m") {
		t.Fatalf("connect by the team's alias: %s", out)
	}
	if r := tug(t, "unlock", "1h"); r.code == 0 || !strings.Contains(r.stderr, "at most 10m, as policy production-read-only of team platform says") {
		t.Fatalf("unlock beyond the policy: %+v", r)
	}
	mustTug(t, "unlock")
	var st statusView
	if err := json.Unmarshal([]byte(mustTug(t, "status", "-o", "json")), &st); err != nil {
		t.Fatal(err)
	}
	if st.UnlockedUntil == nil || time.Until(*st.UnlockedUntil) > 10*time.Minute {
		t.Fatalf("unlock without a duration: %+v, want at most 10m", st)
	}
	if r := tug(t, "connect", "pay-prod", "--read-write"); r.code == 0 || !strings.Contains(r.stderr, "cannot open read-write") {
		t.Fatalf("--read-write under a policy: %+v", r)
	}

	// A label of the user's wins in the catalog, is reported, and does not
	// take the cluster out of the team's policy.
	mustTug(t, "label", "pay-prod", "env=dev")
	if out := mustTug(t, "connect", "pay-prod"); !strings.Contains(out, "read-only") {
		t.Fatalf("connect after relabelling: %s", out)
	}
	if out := mustTug(t, "team", "show", "platform"); !strings.Contains(out, "your label env=dev wins over the team's env=prod") ||
		!strings.Contains(out, "production-read-only: env=prod: read-only, unlocks up to 10m") {
		t.Fatalf("team show: %s", out)
	}

	// Nothing new to review.
	if out := mustTug(t, "team", "sync"); !strings.Contains(out, "team platform: version 1, up to date") {
		t.Fatalf("team sync unchanged: %s", out)
	}

	// A tampered catalog is refused and the last good version stays.
	tampered := strings.Replace(v1, "maxUnlock: 10m", "maxUnlock: 24h", 1)
	srv.publish(t, tampered, priv, mustSign(t, v1, priv))
	if r := tug(t, "team", "sync"); r.code == 0 || !strings.Contains(r.stderr, "signature does not match") ||
		!strings.Contains(r.stderr, "keeping version 1") {
		t.Fatalf("team sync of a tampered catalog: %+v", r)
	}
	if r := tug(t, "unlock", "1h"); r.code == 0 {
		t.Fatal("the tampered catalog's limit applied")
	}

	// A new version waits for review, and applies with --yes.
	v2 := teamCatalogDoc(2, "5m", gpuPath, srv.URL+"/index.yaml", indexKey)
	srv.publish(t, v2, priv, nil)
	r = tug(t, "team", "sync")
	if r.code == 0 || !strings.Contains(r.stderr, "version 1 → version 2") ||
		!strings.Contains(r.stderr, "~ policy production-read-only: env=prod: read-only, unlocks up to 10m → env=prod: read-only, unlocks up to 5m") ||
		!strings.Contains(r.stderr, "waiting for review") {
		t.Fatalf("team sync of a new version without a terminal: %+v", r)
	}
	mustTug(t, "unlock", "8m") // still version 1
	if out := mustTug(t, "team", "sync", "--yes"); !strings.Contains(out, "version 2 applied") {
		t.Fatalf("team sync --yes: %s", out)
	}
	if r := tug(t, "unlock", "8m"); r.code == 0 || !strings.Contains(r.stderr, "at most 5m") {
		t.Fatalf("unlock after version 2: %+v", r)
	}
	srv.publish(t, v1, priv, nil)
	if r := tug(t, "team", "sync"); r.code == 0 || !strings.Contains(r.stderr, "older than the version already accepted") {
		t.Fatalf("team sync of an older version: %+v", r)
	}

	// tug sync lists the team's account with the user's credentials and
	// names its cluster as the team says.
	r = tug(t, "sync")
	if r.code != 0 || !strings.Contains(r.stderr, "team platform: + cluster gpu: renamed from gpu-train") {
		t.Fatalf("sync of the team's account: %+v", r)
	}
	if out := mustTug(t, "ls", "-o", "name", "-l", "env=sandbox"); strings.TrimSpace(out) != "gpu" {
		t.Fatalf("ls -l env=sandbox: %q", out)
	}

	// The team's plugin index is searched, under the team's name.
	if r := tug(t, "plugin", "search"); !strings.Contains(r.stderr, "index platform/acme") {
		t.Fatalf("plugin search with the team's index: %+v", r)
	}

	// Offline, the last good version keeps working.
	srv.Close()
	if r := tug(t, "team", "sync"); r.code == 0 || !strings.Contains(r.stderr, "keeping version 2") {
		t.Fatalf("team sync offline: %+v", r)
	}
	if r := tug(t, "unlock", "8m"); r.code == 0 || !strings.Contains(r.stderr, "at most 5m") {
		t.Fatalf("unlock offline: %+v", r)
	}
	var shown teamDetail
	if err := json.Unmarshal([]byte(mustTug(t, "team", "show", "platform", "-o", "json")), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.Catalog == nil || shown.Catalog.Version != 2 || shown.LastError == "" || len(shown.Clusters) != 2 {
		t.Fatalf("team show offline: %+v", shown)
	}

	// Removing the team takes its policies, index, names and labels back.
	r = tug(t, "team", "remove", "platform")
	if r.code != 0 || !strings.Contains(r.stdout, "Removed team platform") ||
		!strings.Contains(r.stdout, "- account kubeconfig/team-gpu: team platform is removed; it stays as your own account") {
		t.Fatalf("team remove: %+v", r)
	}
	if out := mustTug(t, "connect", "pay-prod", "--read-write"); !strings.Contains(out, "read-write") {
		t.Fatalf("connect --read-write after team remove: %s", out)
	}
	clusters = nil
	if err := json.Unmarshal([]byte(mustTug(t, "ls", "-o", "json")), &clusters); err != nil {
		t.Fatal(err)
	}
	for _, cl := range clusters {
		if cl.Team != "" || len(cl.Aliases) > 0 || cl.Labels["team"] != "" {
			t.Fatalf("cluster %s kept the team's entry after team remove: %+v", cl.Name, cl)
		}
	}
	if r := tug(t, "plugin", "search"); r.code == 0 || !strings.Contains(r.stderr, "no plugin index is set up") {
		t.Fatalf("plugin search after team remove: %+v", r)
	}
	if r := tug(t, "team", "remove", "platform"); r.code == 0 || !strings.Contains(r.stderr, "no team platform") {
		t.Fatalf("removing a team twice: %+v", r)
	}
	if writes.Load() != 0 {
		t.Fatalf("API server saw %d writes", writes.Load())
	}
}

func mustSign(t *testing.T, doc, priv string) []byte {
	t.Helper()
	sig, err := team.Sign([]byte(doc), priv)
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// TestUnlockHintsFollowTheTeamLimit checks that every hint to run tug
// unlock on a cluster whose team limits unlocks suggests one the limit
// allows, as tug connect's does.
func TestUnlockHintsFollowTheTeamLimit(t *testing.T) {
	isolate(t)
	api, writes := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, api))
	newSession(t)
	priv, pub, err := team.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	_, indexKey, _ := team.NewKey()
	srv := newTeamServer(t)
	srv.publish(t, teamCatalogDoc(1, "10m", writeGPUKubeconfig(t, api), srv.URL+"/index.yaml", indexKey), priv, nil)
	mustTug(t, "team", "add", "platform", srv.catalogURL(), "--public-key", pub, "--yes")

	const hint = "tug unlock 10m"
	if out := mustTug(t, "connect", "pay-prod"); !strings.Contains(out, "read-only ("+hint+" to allow writes)") {
		t.Fatalf("connect: %s, want it to suggest %q", out, hint)
	}
	// Writes the guard refuses, from tug and through kubectl.
	for _, args := range [][]string{{"scale", "api", "3"}, {"delete", "pod", "api-0"}} {
		r := tug(t, args...)
		if r.code == 0 || !strings.Contains(r.stderr, "is read-only") || !strings.Contains(r.stderr, "run `"+hint+"` to allow writes") {
			t.Errorf("tug %s = %+v, want a refusal that suggests %q", strings.Join(args, " "), r, hint)
		}
	}
	mustTugIn(t, "cluster: pay-prod\n", "profile", "edit", "prod")
	if out := mustTug(t, "profile", "up", "prod"); !strings.Contains(out, "read-only ("+hint+" to allow writes)") {
		t.Errorf("profile up: %s, want it to suggest %q", out, hint)
	}
	if writes.Load() != 0 {
		t.Fatalf("%d writes reached the API server", writes.Load())
	}
	// The unlock the hints suggest is one the policy allows.
	mustTug(t, "unlock", "10m")
}

func TestTeamFromConfigYAML(t *testing.T) {
	home := isolate(t)
	api, _ := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, api))
	priv, pub, _ := team.NewKey()
	_, indexKey, _ := team.NewKey()
	srv := newTeamServer(t)
	srv.publish(t, teamCatalogDoc(1, "10m", writeGPUKubeconfig(t, api), "https://plugins.example.com/index.yaml", indexKey), priv, nil)

	cfg := filepath.Join(home, "config", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	// Only the team's policy makes env=prod read-only here.
	conf := "guard: {readOnlyLabels: [{tier: critical}]}\nteams:\n- {name: platform, url: " + srv.catalogURL() + ", publicKey: " + pub + "}\n"
	if err := os.WriteFile(cfg, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}

	var views []teamView
	if err := json.Unmarshal([]byte(mustTug(t, "team", "ls", "-o", "json")), &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].SetIn != "config.yaml" || views[0].Version != 0 || !strings.HasPrefix(views[0].Fingerprint, "sha256:") {
		t.Fatalf("team ls: %+v", views)
	}
	if out := mustTug(t, "team", "sync", "platform", "--yes"); !strings.Contains(out, "team platform: version 1 applied") {
		t.Fatalf("team sync --yes: %s", out)
	}
	// An account the team added and the user forgot stays forgotten until
	// a new version is reviewed.
	mustTug(t, "forget", "kubeconfig/team-gpu")
	mustTug(t, "sync")
	if out := mustTug(t, "team", "sync"); !strings.Contains(out, "up to date") {
		t.Fatalf("team sync after forget: %s", out)
	}
	if r := tug(t, "forget", "kubeconfig/team-gpu"); r.code == 0 || !strings.Contains(r.stderr, "no source") {
		t.Fatalf("the forgotten team account came back: %+v", r)
	}
	// The team's policy makes the cluster read-only, and production in the
	// prompt.
	newSession(t)
	if out := mustTug(t, "connect", "pay-prod"); !strings.Contains(out, "read-only") {
		t.Fatalf("connect under the team's policy: %s", out)
	}
	var st statusView
	if err := json.Unmarshal([]byte(mustTug(t, "status", "-o", "json")), &st); err != nil {
		t.Fatal(err)
	}
	if !st.ReadOnly || !st.Production {
		t.Fatalf("status under the team's policy: %+v, want read-only and production", st)
	}
	if r := tug(t, "team", "sync", "nobody"); r.code == 0 || !strings.Contains(r.stderr, "no team nobody") {
		t.Fatalf("team sync of an unknown team: %+v", r)
	}
	if r := tug(t, "team", "remove", "platform"); r.code == 0 || !strings.Contains(r.stderr, "set in config.yaml") {
		t.Fatalf("team remove of a config.yaml team: %+v", r)
	}
	if r := tug(t, "team", "add", "platform", srv.catalogURL(), "--public-key", pub, "--yes"); r.code == 0 || !strings.Contains(r.stderr, "already set up") {
		t.Fatalf("team add of a config.yaml team: %+v", r)
	}

	// Once it is gone from config.yaml, tug team remove takes back what it
	// added.
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if out := mustTug(t, "team", "remove", "platform"); !strings.Contains(out, "- cluster pay-prod") {
		t.Fatalf("team remove after config.yaml: %s", out)
	}
}

func TestTeamPublishing(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	pub := strings.TrimSpace(mustTug(t, "team", "keygen"))
	if r := tug(t, "team", "keygen"); r.code == 0 || !strings.Contains(r.stderr, "--force") {
		t.Fatalf("keygen twice: %+v", r)
	}
	_, indexKey, _ := team.NewKey()
	path := filepath.Join(dir, "team.yaml")
	doc := teamCatalogDoc(1, "30m", "/srv/gpu.yaml", "https://plugins.example.com/index.yaml", indexKey)
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := mustTug(t, "team", "sign", path); !strings.Contains(out, "Wrote "+path+".sig") {
		t.Fatalf("team sign: %s", out)
	}
	if out := mustTug(t, "team", "verify", path, "--public-key", pub); !strings.Contains(out, "version 1, signed by that key: 1 account, 2 clusters, 1 plugin index, 2 policies") {
		t.Fatalf("team verify: %s", out)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(doc, "readOnly: true", "readOnly: false", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := tug(t, "team", "verify", path, "--public-key", pub); r.code == 0 || !strings.Contains(r.stderr, "signature does not match") {
		t.Fatalf("team verify of a changed file: %+v", r)
	}

	// A catalog that carries a credential is not signed.
	leaky := strings.Replace(doc, "config: {path:", "config: {token: abc, path:", 1)
	if err := os.WriteFile(path, []byte(leaky), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := tug(t, "team", "sign", path); r.code == 0 || !strings.Contains(r.stderr, "looks like a credential") {
		t.Fatalf("team sign of a catalog with a token: %+v", r)
	}
}
