package catalog

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
)

func TestMergeAddsUpdatesAndMarksStale(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	src := Source{Provider: "coreweave", Account: "work"}
	c := &Catalog{}

	res := c.Merge(src, []provider.Cluster{
		{ID: "c-1", Name: "train-us-east", Region: "US-EAST-04", Labels: map[string]string{"gpu": "TRUE"}},
		{ID: "c-2", Name: "payments-prod-eu"},
	}, t0)
	if len(res.Added) != 2 {
		t.Fatalf("added = %v", res.Added)
	}
	train, err := c.Get("train-us-east")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"provider": "coreweave", "account": "work", "region": "us-east-04", "gpu": "true"}
	for k, v := range want {
		if train.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, train.Labels[k], v)
		}
	}
	if prod, _ := c.Get("payments-prod-eu"); prod.Labels[LabelEnv] != "prod" {
		t.Errorf("env not guessed from name: %v", prod.Labels)
	}

	// User labels and first-seen survive a sync; a missing cluster goes stale.
	train.UserLabels = map[string]string{"team": "ml"}
	t1 := t0.Add(time.Hour)
	res = c.Merge(src, []provider.Cluster{{ID: "c-1", Name: "renamed-upstream", Version: "1.34"}}, t1)
	if len(res.Updated) != 1 || len(res.Stale) != 1 {
		t.Fatalf("result = %+v", res)
	}
	train, _ = c.Get("coreweave/work/c-1")
	if train.Name != "train-us-east" || train.Version != "1.34" || !train.FirstSeen.Equal(t0) || !train.LastSeen.Equal(t1) {
		t.Fatalf("updated cluster = %+v", train)
	}
	if train.EffectiveLabels()["team"] != "ml" {
		t.Fatal("user label lost")
	}
	if prod, _ := c.Get("payments-prod-eu"); !prod.Stale {
		t.Fatal("missing cluster not marked stale")
	}

	// It comes back: no longer stale.
	c.Merge(src, []provider.Cluster{{ID: "c-1", Name: "x"}, {ID: "c-2", Name: "payments-prod-eu"}}, t1)
	if prod, _ := c.Get("payments-prod-eu"); prod.Stale {
		t.Fatal("returning cluster still stale")
	}
}

func TestUniqueNames(t *testing.T) {
	c := &Catalog{}
	c.Merge(Source{Provider: "kubeconfig", Account: "default"}, []provider.Cluster{{ID: "kind-dev", Name: "kind-dev"}}, time.Now())
	c.Merge(Source{Provider: "kubeconfig", Account: "laptop"}, []provider.Cluster{{ID: "kind-dev", Name: "kind-dev"}}, time.Now())
	if _, err := c.Get("kind-dev@laptop"); err != nil {
		t.Fatalf("second cluster not disambiguated: %v", err)
	}
}

func TestGuessEnv(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"payments-prod-eu", "prod"},
		{"PRODUCTION_us", "prod"},
		{"staging-us", "staging"},
		{"kind-dev", "dev"},
		{"product-catalog", ""},
		{"api", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GuessEnv(tt.name); got != tt.want {
				t.Errorf("GuessEnv(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	c := &Catalog{}
	c.AddSource(Source{Provider: "kubeconfig", Account: "default", Config: map[string]any{"path": "/x"}})
	c.Merge(Source{Provider: "kubeconfig", Account: "default"}, []provider.Cluster{
		{ID: "a", Name: "a", Endpoints: provider.Endpoints{Public: "https://a"}, CAData: []byte("CA")},
	}, time.Now())
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := got.Get("a")
	if err != nil || string(cl.CAData) != "CA" || cl.Server != "https://a" {
		t.Fatalf("round trip: %+v %v", cl, err)
	}
	if len(got.Sources) != 1 || got.Sources[0].Config["path"] != "/x" {
		t.Fatalf("sources: %+v", got.Sources)
	}
}

func TestMergeKeepsTheUsersTunnel(t *testing.T) {
	src := Source{Provider: "teleport", Account: "work"}
	c := &Catalog{}
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	listed := func(tunnel string) []provider.Cluster {
		return []provider.Cluster{{ID: "root/prod", Name: "prod", Endpoints: provider.Endpoints{Public: "https://teleport.example.com:443"}, Tunnel: tunnel}}
	}
	const (
		fromProvider = "tsh://teleport.example.com:443/root/prod"
		fromUser     = "socks5://127.0.0.1:1080"
	)
	steps := []struct {
		name                string
		userSets            string // set by the user before the sync, if not ""
		listed              string
		provider, effective string
	}{
		{"the provider's tunnel", "", fromProvider, fromProvider, fromProvider},
		{"the user's wins", fromUser, fromProvider, fromProvider, fromUser},
		{"the provider drops its tunnel", "", "", "", fromUser},
	}
	for i, st := range steps {
		if st.userSets != "" {
			cl, err := c.Get("prod")
			if err != nil {
				t.Fatal(err)
			}
			cl.Tunnel = st.userSets
		}
		c.Merge(src, listed(st.listed), t0.Add(time.Duration(i)*time.Hour))
		cl, err := c.Get("prod")
		if err != nil {
			t.Fatal(err)
		}
		if cl.ProviderTunnel != st.provider || cl.EffectiveTunnel() != st.effective {
			t.Errorf("%s: provider tunnel %q, effective %q; want %q, %q", st.name, cl.ProviderTunnel, cl.EffectiveTunnel(), st.provider, st.effective)
		}
	}
}

func TestMergeKeepsCAWhenAListingOmitsIt(t *testing.T) {
	src := Source{Provider: "akamai", Account: "work"}
	c := &Catalog{}
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	c.Merge(src, []provider.Cluster{{ID: "101", Name: "a", Endpoints: provider.Endpoints{Public: "https://a:443"}, CAData: []byte("CA")}}, t0)

	tests := []struct {
		name   string
		listed provider.Cluster
		wantCA string
	}{
		{"CA missing, same server", provider.Cluster{ID: "101", Name: "a", Endpoints: provider.Endpoints{Public: "https://a:443"}}, "CA"},
		{"new CA", provider.Cluster{ID: "101", Name: "a", Endpoints: provider.Endpoints{Public: "https://a:443"}, CAData: []byte("CA2")}, "CA2"},
		{"CA missing, server moved", provider.Cluster{ID: "101", Name: "a", Endpoints: provider.Endpoints{Public: "https://b:443"}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.Merge(src, []provider.Cluster{tt.listed}, t0.Add(time.Hour))
			got, err := c.Get("a")
			if err != nil {
				t.Fatal(err)
			}
			if string(got.CAData) != tt.wantCA {
				t.Fatalf("CAData = %q, want %q", got.CAData, tt.wantCA)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	tests := []struct {
		name    string
		path    string
		wantErr string // "" means success
	}{
		{"missing file is empty", filepath.Join(dir, "missing.yaml"), ""},
		{"no apiVersion", write("none.yaml", "clusters: []\n"), ""},
		{"current apiVersion", write("current.yaml", "apiVersion: "+APIVersion+"\n"), ""},
		{"unsupported apiVersion", write("future.yaml", "apiVersion: tug.catalog/v9\n"), `unsupported apiVersion "tug.catalog/v9"`},
		{"not YAML", write("bad.yaml", "clusters: [\n"), "parsing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load(tt.path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load() = %+v, %v; want an error containing %q", c, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.APIVersion != APIVersion || len(c.Clusters) != 0 {
				t.Fatalf("Load() = %+v, want an empty catalog at %s", c, APIVersion)
			}
		})
	}
}

// names returns the names of clusters, in order.
func names(cls []*Cluster) []string {
	out := make([]string, 0, len(cls))
	for _, cl := range cls {
		out = append(out, cl.Name)
	}
	return out
}

func TestSelectHonoursUserLabels(t *testing.T) {
	c := &Catalog{}
	c.Merge(Source{Provider: "kubeconfig", Account: "default"}, []provider.Cluster{
		{ID: "a", Name: "payments-prod", Labels: map[string]string{"team": "payments"}},
		{ID: "b", Name: "payments-staging", Labels: map[string]string{"team": "payments"}},
		{ID: "c", Name: "search-prod"},
	}, time.Now())

	tests := []struct {
		name string
		sel  map[string]string
		want []string
	}{
		{"empty selector matches all", nil, []string{"payments-prod", "payments-staging", "search-prod"}},
		{"provider label", map[string]string{"provider": "kubeconfig"}, []string{"payments-prod", "payments-staging", "search-prod"}},
		{"guessed env", map[string]string{"env": "prod"}, []string{"payments-prod", "search-prod"}},
		{"every label must match", map[string]string{"env": "prod", "team": "payments"}, []string{"payments-prod"}},
		{"no match", map[string]string{"team": "search"}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := names(c.Select(tt.sel)); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Select(%v) = %v, want %v", tt.sel, got, tt.want)
			}
		})
	}

	// A user label overrides the provider's, and an empty one removes it.
	search, _ := c.Get("search-prod")
	search.UserLabels = map[string]string{"env": "staging"}
	staging, _ := c.Get("payments-staging")
	staging.UserLabels = map[string]string{"team": ""}

	if got, want := names(c.Select(map[string]string{"env": "prod"})), []string{"payments-prod"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Select(env=prod) with a user override = %v, want %v", got, want)
	}
	if got, want := names(c.Select(map[string]string{"team": "payments"})), []string{"payments-prod"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Select(team=payments) after removing a label = %v, want %v", got, want)
	}
	if _, ok := staging.EffectiveLabels()["team"]; ok {
		t.Fatalf("EffectiveLabels() = %v, want team removed", staging.EffectiveLabels())
	}
}

func TestRemoveSource(t *testing.T) {
	c := &Catalog{}
	work := Source{Provider: "coreweave", Account: "work"}
	home := Source{Provider: "coreweave", Account: "home"}
	for _, src := range []Source{work, home} {
		c.AddSource(src)
		c.Merge(src, []provider.Cluster{{ID: "1", Name: "train"}, {ID: "2", Name: "infer"}}, time.Now())
	}

	removed := c.RemoveSource("coreweave", "work")
	var ids []string
	for _, cl := range removed {
		ids = append(ids, cl.ID)
	}
	slices.Sort(ids)
	if want := []string{"coreweave/work/1", "coreweave/work/2"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("RemoveSource removed %v, want %v", ids, want)
	}
	if len(c.Sources) != 1 || c.Sources[0].Key() != home.Key() {
		t.Fatalf("sources after RemoveSource = %+v, want only %s", c.Sources, home.Key())
	}
	for _, cl := range c.Clusters {
		if cl.Account != "home" {
			t.Fatalf("cluster %s from the removed source is still in the catalog", cl.ID)
		}
	}
	if len(c.Clusters) != 2 {
		t.Fatalf("catalog has %d clusters, want the 2 from home", len(c.Clusters))
	}
	if got := c.RemoveSource("coreweave", "nope"); len(got) != 0 {
		t.Fatalf("RemoveSource of an unknown source removed %v", got)
	}
}

func TestNormaliseLabels(t *testing.T) {
	tests := []struct {
		name   string
		pc     provider.Cluster
		labels map[string]string // expected subset
		absent []string
	}{
		{"env alias", provider.Cluster{Name: "x", Labels: map[string]string{"Env": "Production"}}, map[string]string{"env": "prod"}, nil},
		{"qa is test", provider.Cluster{Name: "x", Labels: map[string]string{"env": "QA"}}, map[string]string{"env": "test"}, nil},
		{"unknown env kept lowercased", provider.Cluster{Name: "x", Labels: map[string]string{"env": "Sandbox"}}, map[string]string{"env": "sandbox"}, nil},
		{"provider env wins over name", provider.Cluster{Name: "api-prod", Labels: map[string]string{"env": "stg"}}, map[string]string{"env": "staging"}, nil},
		{"env guessed from name", provider.Cluster{Name: "api-stage-1"}, map[string]string{"env": "staging"}, nil},
		{"no env", provider.Cluster{Name: "api"}, nil, []string{"env"}},
		{"region and gpu lowercased", provider.Cluster{Name: "x", Region: "EU-West", Labels: map[string]string{"GPU": "H100"}}, map[string]string{"region": "eu-west", "gpu": "h100"}, nil},
		{"no region", provider.Cluster{Name: "x"}, nil, []string{"region"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormaliseLabels("aws", "work", tt.pc)
			if got[LabelProvider] != "aws" || got[LabelAccount] != "work" {
				t.Errorf("provider, account labels = %q, %q; want aws, work", got[LabelProvider], got[LabelAccount])
			}
			for k, v := range tt.labels {
				if got[k] != v {
					t.Errorf("label %s = %q, want %q (all: %v)", k, got[k], v, got)
				}
			}
			for _, k := range tt.absent {
				if v, ok := got[k]; ok {
					t.Errorf("label %s = %q, want it absent", k, v)
				}
			}
		})
	}
}

func TestTeamLayer(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	src := Source{Provider: "aws", Account: "work"}
	c := &Catalog{}
	c.Merge(src, []provider.Cluster{{ID: "arn-1", Name: "arn-1", Labels: map[string]string{"env": "dev", "tier": "1"}}}, t0)
	cl, err := c.Get("arn-1")
	if err != nil {
		t.Fatal(err)
	}
	cl.Name = "payments-prod"
	cl.Team = &TeamEntry{
		Provenance: Provenance{Team: "platform", Version: 3},
		Name:       "payments-prod",
		Aliases:    []string{"pay"},
		Labels:     map[string]string{"env": "prod", "team": "payments"},
	}
	cl.UserLabels = map[string]string{"env": "staging"}

	tests := []struct {
		name string
		got  map[string]string
		want map[string]string
	}{
		{"EffectiveLabels", cl.EffectiveLabels(), map[string]string{"env": "staging", "team": "payments", "tier": "1"}},
		{"TeamLabels", cl.TeamLabels(), map[string]string{"env": "prod", "team": "payments", "tier": "1"}},
	}
	for _, tt := range tests {
		for k, v := range tt.want {
			if tt.got[k] != v {
				t.Errorf("%s()[%s] = %q, want %q", tt.name, k, tt.got[k], v)
			}
		}
	}

	if got, err := c.Get("pay"); err != nil || got.ID != cl.ID {
		t.Fatalf(`Get("pay") = %v, %v; want the aliased cluster`, got, err)
	}
	// A sync keeps the team's entry and the name it gave.
	c.Merge(src, []provider.Cluster{{ID: "arn-1", Name: "arn-1"}, {ID: "arn-2", Name: "pay"}}, t0.Add(time.Hour))
	cl, _ = c.Get("aws/work/arn-1")
	if cl.Name != "payments-prod" || cl.Team == nil || cl.Team.Version != 3 {
		t.Fatalf("after merge: %+v", cl)
	}
	// A new cluster does not take a name another cluster answers to.
	if other, _ := c.Get("aws/work/arn-2"); other.Name == "pay" {
		t.Fatalf("new cluster took the alias pay as its name")
	}

	path := filepath.Join(t.TempDir(), FileName)
	c.AddSource(Source{Provider: "aws", Account: "work", Team: &Provenance{Team: "platform", Version: 3}})
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Sources[0].Team, c.Sources[0].Team) {
		t.Fatalf("source provenance after load = %+v", loaded.Sources[0].Team)
	}
	lcl, _ := loaded.Get("payments-prod")
	if !reflect.DeepEqual(lcl.Team, cl.Team) {
		t.Fatalf("team entry after load = %+v, want %+v", lcl.Team, cl.Team)
	}
}
