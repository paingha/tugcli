package catalog

import (
	"path/filepath"
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
