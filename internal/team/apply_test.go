package team

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/pkg/provider"
)

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// localCatalog has two clusters from a kubeconfig the user imported.
func localCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat := &catalog.Catalog{}
	src := catalog.Source{Provider: "kubeconfig", Account: "work"}
	cat.AddSource(src)
	cat.Merge(src, []provider.Cluster{
		{ID: "payments-prod", Name: "payments-prod", Endpoints: provider.Endpoints{Public: "https://prod.example.com:443"}},
		{ID: "payments-staging", Name: "payments-staging", Endpoints: provider.Endpoints{Public: "https://staging.example.com"}},
	}, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	return cat
}

func parse(t *testing.T, data string) *Document {
	t.Helper()
	doc, err := parseUnsigned([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func hasChange(p Plan, op, kind, subject, detail string) bool {
	for _, c := range p.Changes {
		if c.Op == op && c.Kind == kind && c.Subject == subject && strings.Contains(c.Detail, detail) {
			return true
		}
	}
	return false
}

func hasConflict(p Plan, subject, detail string) bool {
	for _, c := range p.Conflicts {
		if c.Subject == subject && strings.Contains(c.Detail, detail) {
			return true
		}
	}
	return false
}

func TestReviewFirstVersion(t *testing.T) {
	cat := localCatalog(t)
	doc := parse(t, testCatalog+`
- name: sandbox
  selector: {env: sandbox}
`)
	p := Review(cat, "platform", nil, doc)

	want := []struct{ op, kind, subject, detail string }{
		{OpAdd, KindAccount, "aws/platform", "regions=us-east-1,eu-west-1"},
		{OpAdd, KindCluster, "pay-prod", "renamed from payments-prod; aliases pay; labels env=prod,team=payments"},
		{OpAdd, KindIndex, "platform/acme", "https://plugins.example.com/index.yaml, signed by sha256:"},
		{OpAdd, KindPolicy, "production-read-only", "env=prod: read-only, unlocks up to 30m"},
		{OpAdd, KindPolicy, "staging-write", "env=staging: read-write"},
		{OpAdd, KindPolicy, "sandbox", "env=sandbox"},
	}
	for _, w := range want {
		if !hasChange(p, w.op, w.kind, w.subject, w.detail) {
			t.Errorf("Review() has no change %s %s %s %q; changes: %v", w.op, w.kind, w.subject, w.detail, p.Changes)
		}
	}
	if len(p.Conflicts) != 0 {
		t.Errorf("Review() conflicts = %v, want none", p.Conflicts)
	}

	src, ok := cat.Source("aws", "platform")
	if !ok || src.Team == nil || src.Team.Team != "platform" || src.Team.Version != 1 {
		t.Fatalf("team account = %+v, want provenance platform v1", src)
	}
	cl, err := cat.Get("pay")
	if err != nil || cl.Name != "pay-prod" || cl.Team.Version != 1 || cl.EffectiveLabels()["env"] != "prod" {
		t.Fatalf("Get(pay) = %+v, %v", cl, err)
	}
	if _, err := cat.Get("kubeconfig/work/payments-prod"); err != nil {
		t.Fatalf("the cluster's ID changed: %v", err)
	}

	// Applying the same version again changes nothing.
	if p := Apply(cat, "platform", doc); len(p.Changes) != 0 || len(p.Conflicts) != 0 {
		t.Fatalf("Apply() again = %+v, want no changes", p)
	}
}

func TestApplyConflicts(t *testing.T) {
	cat := localCatalog(t)
	// The user imported the team's account with their own settings, has
	// a cluster named as the team names another, and labelled the
	// production cluster themselves.
	cat.AddSource(catalog.Source{Provider: "aws", Account: "platform", Config: map[string]any{"regions": "us-west-2"}})
	staging, _ := cat.Get("payments-staging")
	staging.Name = "pay-prod"
	prod, _ := cat.Get("payments-prod")
	prod.UserLabels = map[string]string{"env": "dev"}

	p := Apply(cat, "platform", parse(t, testCatalog))
	for _, w := range []struct{ subject, detail string }{
		{"aws/platform", "your settings (regions=us-west-2) win over the team's"},
		{"payments-prod", "the team names it pay-prod, which pay-prod already answers to"},
		{"payments-prod", "your label env=dev wins over the team's env=prod, though team policies still see env=prod"},
	} {
		if !hasConflict(p, w.subject, w.detail) {
			t.Errorf("Apply() has no conflict for %s %q; conflicts: %v", w.subject, w.detail, p.Conflicts)
		}
	}
	src, _ := cat.Source("aws", "platform")
	if src.Team != nil || src.Config["regions"] != "us-west-2" {
		t.Errorf("the user's own account changed: %+v", src)
	}
	prod, _ = cat.Get("kubeconfig/work/payments-prod")
	if prod.Name != "payments-prod" || prod.EffectiveLabels()["env"] != "dev" || prod.TeamLabels()["env"] != "prod" {
		t.Errorf("payments-prod = %+v, labels %v / %v", prod, prod.EffectiveLabels(), prod.TeamLabels())
	}
	// The alias is free, so it applies even though the name does not.
	if got, err := cat.Get("pay"); err != nil || got.ID != prod.ID {
		t.Errorf("Get(pay) = %v, %v; want payments-prod", got, err)
	}

	// A second team that describes the same cluster does not take it over.
	other := strings.Replace(testCatalog, "name: pay-prod", "name: payments", 1)
	other = strings.Replace(other, "aliases: [pay]", "aliases: [p]", 1)
	p = Apply(cat, "infra", parse(t, other))
	if !hasConflict(p, "payments-prod", "team platform describes it already") {
		t.Errorf("Apply(infra) conflicts = %v", p.Conflicts)
	}
	if !hasConflict(p, "aws/platform", "you imported it yourself") {
		t.Errorf("Apply(infra) conflicts = %v", p.Conflicts)
	}
}

func TestApplyLaterVersionsAndDetach(t *testing.T) {
	cat := localCatalog(t)
	v1 := parse(t, testCatalog)
	Apply(cat, "platform", v1)

	// Version 2 renames the alias, changes the account's settings, drops
	// a label and the production entry's policy, and describes a cluster
	// that is not in the catalog yet.
	v2text := withVersion(testCatalog, "2")
	v2text = strings.Replace(v2text, "aliases: [pay]", "aliases: [payments]", 1)
	v2text = strings.Replace(v2text, "regions: us-east-1,eu-west-1", "regions: eu-west-1", 1)
	v2text = strings.Replace(v2text, "  labels: {team: payments}\n", "", 1)
	v2text = strings.Replace(v2text, "  maxUnlock: 30m\n", "  maxUnlock: 15m\n", 1)
	v2text = strings.Replace(v2text, "pluginIndexes:", `- match: {server: https://gpu.example.com}
  name: gpu-train
pluginIndexes:`, 1)
	v2 := parse(t, v2text)
	p := Review(cat, "platform", v1, v2)
	for _, w := range []struct{ op, kind, subject, detail string }{
		{OpUpdate, KindAccount, "aws/platform", "regions=us-east-1,eu-west-1 → regions=eu-west-1"},
		{OpUpdate, KindCluster, "pay-prod", "aliases payments; labels -team"},
		{OpUpdate, KindPolicy, "production-read-only", "unlocks up to 30m → env=prod: read-only, unlocks up to 15m"},
	} {
		if !hasChange(p, w.op, w.kind, w.subject, w.detail) {
			t.Errorf("Review(v2) has no change %s %s %s %q; changes: %v", w.op, w.kind, w.subject, w.detail, p.Changes)
		}
	}
	if len(p.Waiting) != 1 || p.Waiting[0] != "gpu-train" || p.From != 1 || p.To != 2 {
		t.Errorf("Review(v2) = %+v", p)
	}
	if _, err := cat.Get("pay"); err == nil {
		t.Error("the old alias still answers")
	}
	cl, _ := cat.Get("payments")
	if cl == nil || cl.Team.Version != 2 {
		t.Fatalf("Get(payments) = %+v", cl)
	}
	if src, _ := cat.Source("aws", "platform"); src.Team.Version != 2 {
		t.Errorf("account provenance = %+v, want version 2", src.Team)
	}

	// Removing the team takes back its entries; its account stays.
	p = Detach(cat, "platform")
	if !hasChange(p, OpRemove, KindAccount, "aws/platform", "stays as your own account") ||
		!hasChange(p, OpRemove, KindCluster, "pay-prod", "keeps its name") {
		t.Errorf("Detach() changes = %v", p.Changes)
	}
	src, ok := cat.Source("aws", "platform")
	if !ok || src.Team != nil {
		t.Errorf("account after Detach() = %+v, %v", src, ok)
	}
	cl, err := cat.Get("pay-prod")
	if err != nil || cl.Team != nil {
		t.Errorf("cluster after Detach() = %+v, %v", cl, err)
	}
	if _, err := cat.Get("payments"); err == nil {
		t.Error("the team's alias still answers after Detach()")
	}
}

func TestMatch(t *testing.T) {
	cl := &catalog.Cluster{Provider: "aws", Account: "alice", ProviderID: "arn:aws:eks:eu-west-1:1:cluster/pay",
		Server: "https://ABC.eks.amazonaws.com/", PrivateServer: "https://10.0.0.1"}
	tests := []struct {
		m    Match
		want bool
	}{
		{Match{ProviderID: "arn:aws:eks:eu-west-1:1:cluster/pay"}, true},
		{Match{Provider: "aws", ProviderID: "arn:aws:eks:eu-west-1:1:cluster/pay"}, true},
		{Match{Provider: "google", ProviderID: "arn:aws:eks:eu-west-1:1:cluster/pay"}, false},
		{Match{Account: "bob", ProviderID: "arn:aws:eks:eu-west-1:1:cluster/pay"}, false},
		{Match{Server: "https://abc.eks.amazonaws.com"}, true},
		{Match{Server: "https://10.0.0.1/"}, true},
		{Match{Server: "https://other.example.com"}, false},
	}
	for _, tt := range tests {
		if got := tt.m.Matches(cl); got != tt.want {
			t.Errorf("Match{%s}.Matches() = %v, want %v", tt.m, got, tt.want)
		}
	}
}

func TestApplyClustersLeavesAccountsAlone(t *testing.T) {
	cat := localCatalog(t)
	doc := parse(t, testCatalog)
	Apply(cat, "platform", doc)
	// The user removes the account the team added.
	cat.RemoveSource("aws", "platform")
	p := ApplyClusters(cat, "platform", doc)
	if _, ok := cat.Source("aws", "platform"); ok {
		t.Fatal("ApplyClusters() brought back an account the user removed")
	}
	if len(p.Changes) != 0 {
		t.Errorf("ApplyClusters() changes = %v, want none", p.Changes)
	}
	// A cluster listed since is named as the accepted version says.
	cl, _ := cat.Get("pay-prod")
	cl.Team, cl.Name = nil, "payments-prod"
	p = ApplyClusters(cat, "platform", doc)
	if !hasChange(p, OpAdd, KindCluster, "pay-prod", "renamed from payments-prod") {
		t.Errorf("ApplyClusters() changes = %v", p.Changes)
	}
}
