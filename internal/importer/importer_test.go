package importer

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/registry"
	"github.com/paingha/tugcli/pkg/provider"
)

const inlineToken = "inline-token-from-list"

// fakeProvider is a registry.Provider that lists canned clusters and records
// every request it receives.
type fakeProvider struct {
	info     provider.Info
	clusters []provider.Cluster
	listErr  error

	lists []provider.ListRequest
	creds []provider.CredentialRequest
}

var _ registry.Provider = (*fakeProvider)(nil)

func (f *fakeProvider) Info() provider.Info { return f.info }

func (f *fakeProvider) Accounts(context.Context) ([]provider.Account, error) {
	return nil, provider.Errorf(provider.Unimplemented, "no accounts")
}

func (f *fakeProvider) List(_ context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	f.lists = append(f.lists, req)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.clusters, nil
}

func (f *fakeProvider) Credential(_ context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	f.creds = append(f.creds, req)
	return provider.Token("fresh-"+req.ClusterID, provider.NoExpiry), nil
}

// newFake returns a provider named name that declares secrets.
func newFake(name string, secrets ...provider.SecretSpec) *fakeProvider {
	return &fakeProvider{info: provider.Info{Name: name, Secrets: secrets}}
}

func tokenPtr(token string) *provider.Credential {
	c := provider.Token(token, provider.NoExpiry)
	return &c
}

var t0 = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// newService wires a Service to an in-memory store, a broker and a registry
// holding providers. The clock starts at t0 and is advanced through *now.
func newService(t *testing.T, providers ...registry.Provider) (*Service, *credentials.MemoryStore, *time.Time) {
	t.Helper()
	reg := registry.New(nil)
	for _, p := range providers {
		if err := reg.Add(p, registry.KindBuiltin, ""); err != nil {
			t.Fatal(err)
		}
	}
	now := t0
	store := credentials.NewMemoryStore()
	svc := &Service{
		Registry: reg,
		Store:    store,
		Broker:   &credentials.Broker{Store: store, Now: func() time.Time { return now }},
		Now:      func() time.Time { return now },
	}
	return svc, store, &now
}

// clearSecretEnv makes sure TUG_SECRET_* variables from the developer's
// environment cannot satisfy a test's secrets.
func clearSecretEnv(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		t.Setenv(SecretEnvPrefix+strings.ToUpper(n), "")
	}
}

func TestImportStoresInlineCredentials(t *testing.T) {
	clearSecretEnv(t, "api_token")
	p := newFake("fake", provider.SecretSpec{Name: "api_token"})
	p.clusters = []provider.Cluster{
		{ID: "c-1", Name: "train-prod", Endpoints: provider.Endpoints{Public: "https://train"}, Credential: tokenPtr(inlineToken)},
		{ID: "c-2", Name: "batch-dev", Endpoints: provider.Endpoints{Public: "https://batch"}},
	}
	svc, store, _ := newService(t, p)
	if err := svc.SetSecret("fake", "work", "api_token", "s3cret"); err != nil {
		t.Fatal(err)
	}
	cat := &catalog.Catalog{}
	src := catalog.Source{Provider: "fake", Account: "work", Config: map[string]any{"org": "acme"}}

	res := svc.Import(context.Background(), cat, src)
	if res.Err != nil {
		t.Fatalf("Import error = %v", res.Err)
	}
	if res.Clusters != 2 || res.Credentials != 1 {
		t.Fatalf("Import counted %d clusters and %d credentials, want 2 and 1", res.Clusters, res.Credentials)
	}
	if want := []string{"train-prod", "batch-dev"}; !reflect.DeepEqual(res.Merge.Added, want) {
		t.Fatalf("Merge.Added = %v, want %v", res.Merge.Added, want)
	}
	if !res.Source.LastSync.Equal(t0) || res.Source.LastError != "" {
		t.Fatalf("Result.Source = %+v, want LastSync %v and no error", res.Source, t0)
	}

	// The provider saw the account, its config and only its declared secret.
	if len(p.lists) != 1 {
		t.Fatalf("provider listed %d times, want 1", len(p.lists))
	}
	wantReq := provider.ListRequest{
		Account: provider.Account{ID: "work", Config: map[string]any{"org": "acme"}},
		Secrets: map[string]string{"api_token": "s3cret"},
	}
	if !reflect.DeepEqual(p.lists[0], wantReq) {
		t.Fatalf("ListRequest = %+v, want %+v", p.lists[0], wantReq)
	}

	// The inline credential went to the store through the broker...
	id := catalog.ClusterID("fake", "work", "c-1")
	cred, err := svc.Broker.Credential(context.Background(), id, false)
	if err != nil || cred.Token != inlineToken {
		t.Fatalf("stored credential for %s = %+v, %v; want token %q", id, cred, err, inlineToken)
	}
	wantKeys := []string{credentials.CredentialKey(id), credentials.SecretKey("fake", "work", "api_token")}
	slices.Sort(wantKeys)
	if got := store.Keys(); !reflect.DeepEqual(got, wantKeys) {
		t.Fatalf("store keys = %v, want %v", got, wantKeys)
	}

	// ...and never into the catalog.
	b, err := yaml.Marshal(cat)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), inlineToken) {
		t.Fatalf("catalog contains the inline token:\n%s", b)
	}

	if len(cat.Sources) != 1 || cat.Sources[0].Key() != "fake/work" {
		t.Fatalf("catalog sources = %+v, want fake/work only", cat.Sources)
	}
	stored, ok := cat.Source("fake", "work")
	if !ok || !stored.LastSync.Equal(t0) {
		t.Fatalf("catalog source = %+v, %v; want LastSync %v", stored, ok, t0)
	}
}

func TestImportMarksStaleOnSecondImport(t *testing.T) {
	p := newFake("fake")
	p.clusters = []provider.Cluster{{ID: "c-1", Name: "a"}, {ID: "c-2", Name: "b"}}
	svc, _, now := newService(t, p)
	cat := &catalog.Catalog{}
	src := catalog.Source{Provider: "fake", Account: "work"}

	if res := svc.Import(context.Background(), cat, src); res.Err != nil {
		t.Fatal(res.Err)
	}

	*now = t0.Add(time.Hour)
	p.clusters = []provider.Cluster{{ID: "c-1", Name: "a"}}
	res := svc.Import(context.Background(), cat, src)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(res.Merge.Added) != 0 || !reflect.DeepEqual(res.Merge.Updated, []string{"a"}) || !reflect.DeepEqual(res.Merge.Stale, []string{"b"}) {
		t.Fatalf("second Merge = %+v, want a updated and b stale", res.Merge)
	}
	b, err := cat.Get("b")
	if err != nil {
		t.Fatalf("stale cluster removed from catalog: %v", err)
	}
	if !b.Stale {
		t.Fatal("cluster b not marked stale")
	}
	if !res.Source.LastSync.Equal(*now) {
		t.Fatalf("LastSync = %v, want %v", res.Source.LastSync, *now)
	}
}

func TestImportListErrorKeepsClusters(t *testing.T) {
	p := newFake("fake")
	p.clusters = []provider.Cluster{{ID: "c-1", Name: "a"}}
	svc, _, now := newService(t, p)
	cat := &catalog.Catalog{}
	src := catalog.Source{Provider: "fake", Account: "work"}
	if res := svc.Import(context.Background(), cat, src); res.Err != nil {
		t.Fatal(res.Err)
	}

	*now = t0.Add(time.Hour)
	listErr := provider.Errorf(provider.Unavailable, "api is down")
	p.listErr = listErr
	res := svc.Import(context.Background(), cat, src)
	if !errors.Is(res.Err, listErr) {
		t.Fatalf("Import error = %v, want %v", res.Err, listErr)
	}
	stored, _ := cat.Source("fake", "work")
	if stored.LastError != listErr.Error() {
		t.Fatalf("LastError = %q, want %q", stored.LastError, listErr.Error())
	}
	if !stored.LastSync.Equal(t0) {
		t.Fatalf("LastSync = %v after a failed import, want the last success %v", stored.LastSync, t0)
	}
	cl, err := cat.Get("a")
	if err != nil {
		t.Fatalf("cluster lost after a failed list: %v", err)
	}
	if cl.Stale {
		t.Fatal("cluster marked stale after a failed list")
	}

	// A later success clears the error.
	p.listErr = nil
	if res := svc.Import(context.Background(), cat, src); res.Err != nil {
		t.Fatal(res.Err)
	}
	if stored, _ := cat.Source("fake", "work"); stored.LastError != "" {
		t.Fatalf("LastError = %q after a successful import, want empty", stored.LastError)
	}
}

func TestImportFailsBeforeListing(t *testing.T) {
	clearSecretEnv(t, "api_token")
	p := newFake("fake", provider.SecretSpec{Name: "api_token"})
	svc, _, _ := newService(t, p)

	tests := []struct {
		name     string
		provider string
		wantCode provider.Code
		wantMsg  string
	}{
		{"unknown provider", "gcp", provider.Internal, `unknown provider "gcp"`},
		{"missing secret", "fake", provider.Unauthenticated, "no api_token stored"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cat := &catalog.Catalog{}
			res := svc.Import(context.Background(), cat, catalog.Source{Provider: tt.provider, Account: "work"})
			if res.Err == nil || provider.CodeOf(res.Err) != tt.wantCode || !strings.Contains(res.Err.Error(), tt.wantMsg) {
				t.Fatalf("Import error = %v, want code %s containing %q", res.Err, tt.wantCode, tt.wantMsg)
			}
			stored, ok := cat.Source(tt.provider, "work")
			if !ok {
				t.Fatal("source not added to the catalog")
			}
			if stored.LastError != res.Err.Error() {
				t.Fatalf("LastError = %q, want %q", stored.LastError, res.Err.Error())
			}
		})
	}
	if len(p.lists) != 0 {
		t.Fatalf("provider listed %d times, want 0", len(p.lists))
	}
}

func TestImportReportsBadInlineCredential(t *testing.T) {
	p := newFake("fake")
	p.clusters = []provider.Cluster{
		{ID: "c-1", Name: "good", Credential: tokenPtr("ok")},
		{ID: "c-2", Name: "bad", Credential: &provider.Credential{Type: provider.CredentialToken}}, // no token
	}
	svc, _, _ := newService(t, p)
	cat := &catalog.Catalog{}

	res := svc.Import(context.Background(), cat, catalog.Source{Provider: "fake", Account: "work"})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "storing credentials in memory") || !strings.Contains(res.Err.Error(), "bad") {
		t.Fatalf("Import error = %v, want one about storing the credential of cluster bad", res.Err)
	}
	if res.Clusters != 2 || res.Credentials != 1 {
		t.Fatalf("Import counted %d clusters and %d credentials, want 2 and 1", res.Clusters, res.Credentials)
	}
	if _, err := cat.Get("bad"); err != nil {
		t.Fatalf("cluster with a bad credential not merged: %v", err)
	}
}

func TestSync(t *testing.T) {
	a, b := newFake("alpha"), newFake("beta")
	a.clusters = []provider.Cluster{{ID: "1", Name: "a1"}}
	b.listErr = provider.Errorf(provider.Unavailable, "down")
	svc, _, _ := newService(t, a, b)
	cat := &catalog.Catalog{}
	cat.AddSource(catalog.Source{Provider: "alpha", Account: "one"})
	cat.AddSource(catalog.Source{Provider: "beta", Account: "two"})
	cat.AddSource(catalog.Source{Provider: "alpha", Account: "three"})

	results := svc.Sync(context.Background(), cat)
	var keys []string
	for _, r := range results {
		keys = append(keys, r.Source.Key())
	}
	if want := []string{"alpha/one", "beta/two", "alpha/three"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("Sync results for %v, want %v in catalog order", keys, want)
	}
	if results[0].Err != nil || results[2].Err != nil {
		t.Fatalf("alpha results have errors: %v, %v", results[0].Err, results[2].Err)
	}
	if provider.CodeOf(results[1].Err) != provider.Unavailable {
		t.Fatalf("beta result error = %v, want Unavailable", results[1].Err)
	}
	var accounts []string
	for _, req := range a.lists {
		accounts = append(accounts, req.Account.ID)
	}
	if want := []string{"one", "three"}; !reflect.DeepEqual(accounts, want) {
		t.Fatalf("alpha listed accounts %v, want %v", accounts, want)
	}
	if len(cat.Clusters) != 2 {
		t.Fatalf("catalog has %d clusters after sync, want 2", len(cat.Clusters))
	}
}

func TestForget(t *testing.T) {
	clearSecretEnv(t, "api_token", "region")
	p := newFake("fake", provider.SecretSpec{Name: "api_token"}, provider.SecretSpec{Name: "region", Optional: true})
	p.clusters = []provider.Cluster{
		{ID: "c-1", Name: "a", Credential: tokenPtr("t1")},
		{ID: "c-2", Name: "b", Credential: tokenPtr("t2")},
	}
	svc, store, _ := newService(t, p)
	cat := &catalog.Catalog{}
	for _, acct := range []string{"work", "home"} {
		if err := svc.SetSecret("fake", acct, "api_token", "secret-"+acct); err != nil {
			t.Fatal(err)
		}
		if err := svc.SetSecret("fake", acct, "region", "eu"); err != nil {
			t.Fatal(err)
		}
		if res := svc.Import(context.Background(), cat, catalog.Source{Provider: "fake", Account: acct}); res.Err != nil {
			t.Fatal(res.Err)
		}
	}
	// A cached delegate output is forgotten too.
	workID := catalog.ClusterID("fake", "work", "c-1")
	if err := store.Set(credentials.DelegateKey(workID), "{}"); err != nil {
		t.Fatal(err)
	}
	// Secrets the provider never declared are not its to delete.
	undeclared := credentials.SecretKey("fake", "work", "undeclared")
	if err := store.Set(undeclared, "x"); err != nil {
		t.Fatal(err)
	}

	removed, err := svc.Forget(cat, "fake", "work")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, cl := range removed {
		names = append(names, cl.ID)
	}
	slices.Sort(names)
	if want := []string{"fake/work/c-1", "fake/work/c-2"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("Forget removed %v, want %v", names, want)
	}
	if _, ok := cat.Source("fake", "work"); ok {
		t.Fatal("source fake/work still in the catalog")
	}
	for _, cl := range cat.Clusters {
		if cl.Account == "work" {
			t.Fatalf("cluster %s from the forgotten source is still in the catalog", cl.Name)
		}
	}

	want := []string{
		credentials.CredentialKey("fake/home/c-1"),
		credentials.CredentialKey("fake/home/c-2"),
		credentials.SecretKey("fake", "home", "api_token"),
		credentials.SecretKey("fake", "home", "region"),
		undeclared,
	}
	slices.Sort(want)
	if got := store.Keys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("store keys after Forget = %v, want %v", got, want)
	}
}

func TestForgetUnknownProvider(t *testing.T) {
	svc, store, _ := newService(t)
	cat := &catalog.Catalog{}
	src := catalog.Source{Provider: "gone", Account: "work"}
	cat.AddSource(src)
	cat.Merge(src, []provider.Cluster{{ID: "c-1", Name: "a"}}, t0)
	id := catalog.ClusterID("gone", "work", "c-1")
	if err := svc.Broker.Put(id, provider.Token("t", provider.NoExpiry)); err != nil {
		t.Fatal(err)
	}

	removed, err := svc.Forget(cat, "gone", "work")
	if err != nil {
		t.Fatalf("Forget for an uninstalled provider error = %v, want nil", err)
	}
	if len(removed) != 1 || len(cat.Clusters) != 0 || len(cat.Sources) != 0 {
		t.Fatalf("Forget removed %d clusters, catalog left with %d clusters and %d sources; want 1, 0, 0", len(removed), len(cat.Clusters), len(cat.Sources))
	}
	if keys := store.Keys(); len(keys) != 0 {
		t.Fatalf("store keys after Forget = %v, want none", keys)
	}
}

func TestSecrets(t *testing.T) {
	required := provider.SecretSpec{Name: "api_token"}
	optional := provider.SecretSpec{Name: "region", Optional: true}

	tests := []struct {
		name     string
		specs    []provider.SecretSpec
		stored   map[string]string // secret name -> value, for fake/work
		env      map[string]string // secret name -> TUG_SECRET_ value
		want     map[string]string
		wantCode provider.Code
	}{
		{
			name: "no declared secrets",
			want: nil,
		},
		{
			name:   "stored",
			specs:  []provider.SecretSpec{required},
			stored: map[string]string{"api_token": "stored"},
			want:   map[string]string{"api_token": "stored"},
		},
		{
			name:   "environment wins over store",
			specs:  []provider.SecretSpec{required},
			stored: map[string]string{"api_token": "stored"},
			env:    map[string]string{"api_token": "from-env"},
			want:   map[string]string{"api_token": "from-env"},
		},
		{
			name:  "environment only",
			specs: []provider.SecretSpec{required},
			env:   map[string]string{"api_token": "from-env"},
			want:  map[string]string{"api_token": "from-env"},
		},
		{
			name:   "only declared secrets are loaded",
			specs:  []provider.SecretSpec{required},
			stored: map[string]string{"api_token": "stored", "other": "not-yours"},
			env:    map[string]string{"region": "not-declared"},
			want:   map[string]string{"api_token": "stored"},
		},
		{
			name:   "optional missing is skipped",
			specs:  []provider.SecretSpec{required, optional},
			stored: map[string]string{"api_token": "stored"},
			want:   map[string]string{"api_token": "stored"},
		},
		{
			name:   "optional present is loaded",
			specs:  []provider.SecretSpec{required, optional},
			stored: map[string]string{"api_token": "stored", "region": "eu"},
			want:   map[string]string{"api_token": "stored", "region": "eu"},
		},
		{
			name:     "required missing",
			specs:    []provider.SecretSpec{required, optional},
			stored:   map[string]string{"region": "eu"},
			wantCode: provider.Unauthenticated,
		},
		{
			name:     "empty stored value counts as missing",
			specs:    []provider.SecretSpec{required},
			stored:   map[string]string{"api_token": ""},
			wantCode: provider.Unauthenticated,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearSecretEnv(t, "api_token", "region", "other")
			for k, v := range tt.env {
				t.Setenv(SecretEnvPrefix+strings.ToUpper(k), v)
			}
			svc, _, _ := newService(t)
			for k, v := range tt.stored {
				if err := svc.SetSecret("fake", "work", k, v); err != nil {
					t.Fatal(err)
				}
			}
			// Another account's secret never satisfies this one.
			if err := svc.SetSecret("fake", "home", "api_token", "home-secret"); err != nil {
				t.Fatal(err)
			}

			got, err := svc.Secrets(provider.Info{Name: "fake", Secrets: tt.specs}, "work")
			if tt.wantCode != "" {
				if provider.CodeOf(err) != tt.wantCode {
					t.Fatalf("Secrets() = %v, %v; want a %s error", got, err, tt.wantCode)
				}
				var pe *provider.Error
				if !errors.As(err, &pe) || !strings.Contains(pe.Hint, "tug import fake --account work") {
					t.Fatalf("Secrets() error hint = %+v, want one telling the user to run tug import", pe)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Secrets() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMissingSecrets(t *testing.T) {
	clearSecretEnv(t, "api_token", "org", "region")
	info := provider.Info{Name: "fake", Secrets: []provider.SecretSpec{
		{Name: "api_token"},
		{Name: "org"},
		{Name: "region", Optional: true},
	}}
	svc, _, _ := newService(t)
	if err := svc.SetSecret("fake", "work", "org", "acme"); err != nil {
		t.Fatal(err)
	}

	got := svc.MissingSecrets(info, "work")
	if want := []provider.SecretSpec{{Name: "api_token"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("MissingSecrets() = %+v, want %+v", got, want)
	}

	t.Setenv(SecretEnvPrefix+"API_TOKEN", "from-env")
	if got := svc.MissingSecrets(info, "work"); len(got) != 0 {
		t.Fatalf("MissingSecrets() with TUG_SECRET_API_TOKEN set = %+v, want none", got)
	}

	if got := svc.MissingSecrets(provider.Info{Name: "fake"}, "work"); len(got) != 0 {
		t.Fatalf("MissingSecrets() with nothing declared = %+v, want none", got)
	}
}

// importOne imports src into a new catalog and fails the test on error.
func importOne(t *testing.T, svc *Service, src catalog.Source) *catalog.Catalog {
	t.Helper()
	cat := &catalog.Catalog{}
	if res := svc.Import(context.Background(), cat, src); res.Err != nil {
		t.Fatal(res.Err)
	}
	return cat
}

func TestFetcher(t *testing.T) {
	clearSecretEnv(t, "api_token")
	p := newFake("fake", provider.SecretSpec{Name: "api_token"})
	p.clusters = []provider.Cluster{{ID: "upstream-42", Name: "train"}}
	svc, _, _ := newService(t, p)
	if err := svc.SetSecret("fake", "work", "api_token", "s3cret"); err != nil {
		t.Fatal(err)
	}
	src := catalog.Source{Provider: "fake", Account: "work", Config: map[string]any{"org": "acme"}}
	cat := importOne(t, svc, src)

	fetch := svc.Fetcher(cat)
	id := catalog.ClusterID("fake", "work", "upstream-42")
	cred, err := fetch(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Token != "fresh-upstream-42" {
		t.Fatalf("credential token = %q, want %q", cred.Token, "fresh-upstream-42")
	}
	want := provider.CredentialRequest{
		Account:   provider.Account{ID: "work", Config: map[string]any{"org": "acme"}},
		ClusterID: "upstream-42", // the provider's ID, not tug's catalog ID
		Secrets:   map[string]string{"api_token": "s3cret"},
	}
	if len(p.creds) != 1 || !reflect.DeepEqual(p.creds[0], want) {
		t.Fatalf("CredentialRequests = %+v, want [%+v]", p.creds, want)
	}

	// The broker uses the fetcher when nothing is stored, and caches it.
	svc.Broker.Fetch = fetch
	for range 2 {
		if c, err := svc.Broker.Credential(context.Background(), id, false); err != nil || c.Token != "fresh-upstream-42" {
			t.Fatalf("Broker.Credential = %+v, %v", c, err)
		}
	}
	if len(p.creds) != 2 {
		t.Fatalf("provider asked %d times, want 2 (once directly, once through the broker)", len(p.creds))
	}
}

func TestFetcherErrors(t *testing.T) {
	clearSecretEnv(t, "api_token")
	src := catalog.Source{Provider: "fake", Account: "work"}
	id := catalog.ClusterID("fake", "work", "c-1")

	tests := []struct {
		name  string
		setup func(t *testing.T) (*Service, *catalog.Catalog, string)
		check func(err error) bool
		want  string
	}{
		{
			name: "unknown cluster",
			setup: func(t *testing.T) (*Service, *catalog.Catalog, string) {
				svc, _, _ := newService(t, newFake("fake"))
				return svc, &catalog.Catalog{}, "nope"
			},
			check: func(err error) bool { return errors.Is(err, catalog.ErrNotFound) },
			want:  "catalog.ErrNotFound",
		},
		{
			name: "source removed",
			setup: func(t *testing.T) (*Service, *catalog.Catalog, string) {
				p := newFake("fake")
				p.clusters = []provider.Cluster{{ID: "c-1", Name: "a"}}
				svc, _, _ := newService(t, p)
				cat := importOne(t, svc, src)
				cat.Sources = nil
				return svc, cat, id
			},
			check: func(err error) bool { return err != nil && strings.Contains(err.Error(), "no source") },
			want:  "an error about the missing source",
		},
		{
			name: "provider not installed",
			setup: func(t *testing.T) (*Service, *catalog.Catalog, string) {
				p := newFake("fake")
				p.clusters = []provider.Cluster{{ID: "c-1", Name: "a"}}
				svc, _, _ := newService(t, p)
				cat := importOne(t, svc, src)
				other, _, _ := newService(t) // empty registry
				other.Store, other.Broker = svc.Store, svc.Broker
				return other, cat, id
			},
			check: func(err error) bool { return err != nil && strings.Contains(err.Error(), `unknown provider "fake"`) },
			want:  "an unknown provider error",
		},
		{
			name: "secret missing",
			setup: func(t *testing.T) (*Service, *catalog.Catalog, string) {
				p := newFake("fake", provider.SecretSpec{Name: "api_token"})
				p.clusters = []provider.Cluster{{ID: "c-1", Name: "a"}}
				svc, store, _ := newService(t, p)
				if err := svc.SetSecret("fake", "work", "api_token", "s3cret"); err != nil {
					t.Fatal(err)
				}
				cat := importOne(t, svc, src)
				if err := store.Delete(credentials.SecretKey("fake", "work", "api_token")); err != nil {
					t.Fatal(err)
				}
				return svc, cat, id
			},
			check: func(err error) bool { return provider.CodeOf(err) == provider.Unauthenticated },
			want:  "an Unauthenticated error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, cat, clusterID := tt.setup(t)
			_, err := svc.Fetcher(cat)(context.Background(), clusterID)
			if !tt.check(err) {
				t.Fatalf("fetch(%q) error = %v, want %s", clusterID, err, tt.want)
			}
		})
	}
}

func TestClusterLookup(t *testing.T) {
	cat := &catalog.Catalog{Clusters: []catalog.Cluster{{
		ID: "fake/work/c-1", Name: "a", Server: "https://a.example:6443", PrivateServer: "https://10.0.0.1:6443",
		CAData: []byte("CA"), TLSServerName: "a.internal", InsecureSkipTLSVerify: true, ProxyURL: "socks5://127.0.0.1:1080",
		Tunnel: "ssh://bastion",
	}}}
	lookup := ClusterLookup(cat)
	got, err := lookup("fake/work/c-1")
	want := credentials.ExecCluster{
		Server: "https://a.example:6443", CAData: []byte("CA"), TLSServerName: "a.internal",
		InsecureSkipTLSVerify: true, ProxyURL: "socks5://127.0.0.1:1080",
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ClusterLookup(cat)(fake/work/c-1) = %+v, %v; want %+v", got, err, want)
	}
	if _, err := lookup("fake/work/gone"); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("ClusterLookup(cat)(fake/work/gone) error = %v, want catalog.ErrNotFound", err)
	}
}

// rotatingProvider is a fakeProvider that can rotate credentials.
type rotatingProvider struct {
	*fakeProvider
	rotateErr error
	rotated   []provider.CredentialRequest
}

var _ Rotator = (*rotatingProvider)(nil)

func (r *rotatingProvider) Rotate(_ context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	r.rotated = append(r.rotated, req)
	if r.rotateErr != nil {
		return provider.Credential{}, r.rotateErr
	}
	return provider.Token("rotated-"+req.ClusterID, provider.NoExpiry), nil
}

func newRotating(name string) *rotatingProvider {
	f := newFake(name, provider.SecretSpec{Name: "api_token"})
	f.info.Capabilities = provider.CapList | provider.CapCredential | provider.CapRotate
	f.clusters = []provider.Cluster{{ID: "c-1", Name: "a", Credential: tokenPtr(inlineToken)}}
	return &rotatingProvider{fakeProvider: f}
}

func TestRotate(t *testing.T) {
	clearSecretEnv(t, "api_token")
	id := catalog.ClusterID("fake", "work", "c-1")
	cached := func(t *testing.T, svc *Service) string {
		t.Helper()
		c, err := svc.Broker.Credential(context.Background(), id, false)
		if err != nil {
			return ""
		}
		return c.Token
	}

	tests := []struct {
		name       string
		rotateErr  error
		wantErr    bool
		wantCached string // what the broker serves afterwards; "" when nothing is cached
	}{
		{name: "replaces the cached credential", wantCached: "rotated-c-1"},
		{name: "failure drops the cached credential", rotateErr: provider.Errorf(provider.Unavailable, "not ready"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newRotating("fake")
			p.rotateErr = tt.rotateErr
			svc, _, _ := newService(t, p)
			if err := svc.SetSecret("fake", "work", "api_token", "s3cret"); err != nil {
				t.Fatal(err)
			}
			cat := importOne(t, svc, catalog.Source{Provider: "fake", Account: "work"})
			if got := cached(t, svc); got != inlineToken {
				t.Fatalf("before rotate the broker serves %q, want %q", got, inlineToken)
			}
			cl, _ := cat.Get(id)
			if !svc.CanRotate(cl) {
				t.Fatal("CanRotate = false for a provider with CapRotate")
			}

			err := svc.Rotate(context.Background(), cat, id)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Rotate = %v, want error %v", err, tt.wantErr)
			}
			want := provider.CredentialRequest{Account: provider.Account{ID: "work"}, ClusterID: "c-1", Secrets: map[string]string{"api_token": "s3cret"}}
			if len(p.rotated) != 1 || !reflect.DeepEqual(p.rotated[0], want) {
				t.Fatalf("rotate requests = %+v, want [%+v]", p.rotated, want)
			}
			if got := cached(t, svc); got != tt.wantCached {
				t.Fatalf("after rotate the broker serves %q, want %q", got, tt.wantCached)
			}
		})
	}
}

func TestRotateUnsupported(t *testing.T) {
	clearSecretEnv(t, "api_token")
	id := catalog.ClusterID("fake", "work", "c-1")
	tests := []struct {
		name string
		p    registry.Provider
	}{
		{"no Rotate method", func() registry.Provider {
			f := newFake("fake")
			f.clusters = []provider.Cluster{{ID: "c-1", Name: "a"}}
			return f
		}()},
		{"Rotate without the capability", func() registry.Provider {
			r := newRotating("fake")
			r.info.Capabilities = provider.CapList | provider.CapCredential
			r.info.Secrets = nil
			return r
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, _ := newService(t, tt.p)
			cat := importOne(t, svc, catalog.Source{Provider: "fake", Account: "work"})
			cl, _ := cat.Get(id)
			if svc.CanRotate(cl) {
				t.Error("CanRotate = true, want false")
			}
			if err := svc.Rotate(context.Background(), cat, id); provider.CodeOf(err) != provider.Unimplemented {
				t.Errorf("Rotate = %v, want Unimplemented", err)
			}
		})
	}
}
