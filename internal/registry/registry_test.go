package registry

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/paingha/tugcli/pkg/provider"
)

// fake is a registry Provider that records nothing and lists one cluster.
type fake struct {
	info provider.Info
}

func (f fake) Info() provider.Info { return f.info }

func (f fake) Accounts(context.Context) ([]provider.Account, error) {
	return []provider.Account{{ID: f.info.Name + "-account"}}, nil
}

func (f fake) List(context.Context, provider.ListRequest) ([]provider.Cluster, error) {
	return []provider.Cluster{{ID: f.info.Name + "-cluster"}}, nil
}

func (f fake) Credential(_ context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	return provider.Token("tok-"+req.ClusterID, provider.NoExpiry), nil
}

func named(name, version string) fake {
	return fake{info: provider.Info{Name: name, Version: version}}
}

func TestAddGetNamesEntry(t *testing.T) {
	r := New(nil)
	for _, add := range []struct {
		p        Provider
		kind     Kind
		location string
	}{
		{named("kubeconfig", "builtin"), KindBuiltin, ""},
		{named("coreweave", "1.0.0"), KindPlugin, "/plugins/coreweave"},
		{named("aws", "0.1.0"), KindDevPlugin, "/src/aws"},
	} {
		if err := r.Add(add.p, add.kind, add.location); err != nil {
			t.Fatalf("Add(%s): %v", add.p.Info().Name, err)
		}
	}

	if got, want := r.Names(), []string{"aws", "coreweave", "kubeconfig"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}

	p, err := r.Get("coreweave")
	if err != nil {
		t.Fatal(err)
	}
	if v := p.Info().Version; v != "1.0.0" {
		t.Fatalf("Get(coreweave).Info().Version = %q, want %q", v, "1.0.0")
	}

	e, ok := r.Entry("aws")
	if !ok || e.Kind != KindDevPlugin || e.Location != "/src/aws" {
		t.Fatalf("Entry(aws) = %+v, %v; want a dev plugin at /src/aws", e, ok)
	}
	if _, ok := r.Entry("gcp"); ok {
		t.Fatal("Entry(gcp) found an unregistered provider")
	}

	_, err = r.Get("gcp")
	if err == nil || !strings.Contains(err.Error(), `"gcp"`) || !strings.Contains(err.Error(), "kubeconfig") {
		t.Fatalf("Get(gcp) error = %v, want one naming gcp and the known providers", err)
	}
}

func TestAddRejectsNamelessProvider(t *testing.T) {
	r := New(nil)
	err := r.Add(named("", "1"), KindPlugin, "/plugins/broken")
	if err == nil || !strings.Contains(err.Error(), "/plugins/broken") {
		t.Fatalf("Add(nameless) error = %v, want one naming the location", err)
	}
	if n := r.Names(); len(n) != 0 {
		t.Fatalf("Names() = %v after a rejected Add, want none", n)
	}
}

func TestPluginOverridingBuiltin(t *testing.T) {
	tests := []struct {
		name          string
		allowOverride []string
		kind          Kind
		wantErr       bool
		wantVersion   string
	}{
		{"plugin refused", nil, KindPlugin, true, "builtin"},
		{"dev plugin refused", nil, KindDevPlugin, true, "builtin"},
		{"other name allowed only", []string{"aws"}, KindPlugin, true, "builtin"},
		{"plugin allowed", []string{"kubeconfig"}, KindPlugin, false, "plugin"},
		{"dev plugin allowed", []string{"kubeconfig"}, KindDevPlugin, false, "plugin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New(tt.allowOverride)
			if err := r.Add(named("kubeconfig", "builtin"), KindBuiltin, ""); err != nil {
				t.Fatal(err)
			}
			err := r.Add(named("kubeconfig", "plugin"), tt.kind, "/plugins/kubeconfig")
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "allowOverride") {
					t.Fatalf("Add error = %v, want one pointing at plugins.allowOverride", err)
				}
			} else if err != nil {
				t.Fatalf("Add error = %v, want nil", err)
			}
			p, err := r.Get("kubeconfig")
			if err != nil {
				t.Fatal(err)
			}
			if got := p.Info().Version; got != tt.wantVersion {
				t.Fatalf("registered version = %q, want %q", got, tt.wantVersion)
			}
			wantKind := tt.kind
			if tt.wantErr {
				wantKind = KindBuiltin
			}
			if e, _ := r.Entry("kubeconfig"); e.Kind != wantKind {
				t.Fatalf("Entry kind = %v, want %v", e.Kind, wantKind)
			}
		})
	}
}

func TestFirstPluginWins(t *testing.T) {
	tests := []struct {
		name         string
		first, again Kind
	}{
		{"plugin then plugin", KindPlugin, KindPlugin},
		{"dev plugin then plugin", KindDevPlugin, KindPlugin},
		{"plugin then dev plugin", KindPlugin, KindDevPlugin},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New(nil)
			if err := r.Add(named("aws", "first"), tt.first, "/first"); err != nil {
				t.Fatal(err)
			}
			if err := r.Add(named("aws", "second"), tt.again, "/second"); err != nil {
				t.Fatalf("second Add error = %v, want nil (later plugins are skipped silently)", err)
			}
			e, _ := r.Entry("aws")
			if e.Location != "/first" || e.Kind != tt.first || e.Provider.Info().Version != "first" {
				t.Fatalf("Entry(aws) = %+v, want the first plugin at /first", e)
			}
		})
	}
}

func TestKindString(t *testing.T) {
	tests := []struct {
		k    Kind
		want string
	}{
		{KindBuiltin, "built-in"},
		{KindPlugin, "plugin"},
		{KindDevPlugin, "dev plugin"},
		{Kind(0), "unknown"},
		{Kind(42), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.k.String(); got != tt.want {
			t.Errorf("Kind(%d).String() = %q, want %q", int(tt.k), got, tt.want)
		}
	}
}

// sdkOnly implements only the required SDK interface.
type sdkOnly struct{}

func (sdkOnly) Info() provider.Info { return provider.Info{Name: "sdk", Version: "2.0.0"} }

func (sdkOnly) List(_ context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	return []provider.Cluster{{ID: "c-" + req.Account.ID}}, nil
}

func (sdkOnly) Credential(_ context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	return provider.Token("tok-"+req.ClusterID, provider.NoExpiry), nil
}

// sdkWithAccounts also lists accounts.
type sdkWithAccounts struct{ sdkOnly }

func (sdkWithAccounts) Accounts(context.Context) ([]provider.Account, error) {
	return []provider.Account{{ID: "work"}, {ID: "home"}}, nil
}

func TestFromSDK(t *testing.T) {
	ctx := context.Background()

	t.Run("without accounts", func(t *testing.T) {
		p := FromSDK(sdkOnly{})
		info := p.Info()
		if info.Name != "sdk" || info.Version != "2.0.0" {
			t.Fatalf("Info() = %+v, want name sdk version 2.0.0", info)
		}
		if want := provider.CapList | provider.CapCredential; info.Capabilities != want {
			t.Fatalf("Capabilities = %v, want %v (the implied ones)", info.Capabilities, want)
		}
		accts, err := p.Accounts(ctx)
		if provider.CodeOf(err) != provider.Unimplemented {
			t.Fatalf("Accounts() = %v, %v; want an Unimplemented error", accts, err)
		}

		clusters, err := p.List(ctx, provider.ListRequest{Account: provider.Account{ID: "work"}})
		if err != nil || len(clusters) != 1 || clusters[0].ID != "c-work" {
			t.Fatalf("List() = %+v, %v; want the SDK provider's cluster c-work", clusters, err)
		}
		cred, err := p.Credential(ctx, provider.CredentialRequest{ClusterID: "c-work"})
		if err != nil || cred.Token != "tok-c-work" {
			t.Fatalf("Credential() = %+v, %v; want token tok-c-work", cred, err)
		}
	})

	t.Run("with accounts", func(t *testing.T) {
		p := FromSDK(sdkWithAccounts{})
		if caps := p.Info().Capabilities; !caps.Has(provider.CapAccounts) {
			t.Fatalf("Capabilities = %v, want accounts included", caps)
		}
		accts, err := p.Accounts(ctx)
		if err != nil || len(accts) != 2 || accts[0].ID != "work" {
			t.Fatalf("Accounts() = %+v, %v; want work and home", accts, err)
		}
	})
}

func TestToSDK(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name         string
		caps         provider.Capability
		wantAccounts bool
	}{
		{"list and credential", provider.CapList | provider.CapCredential, false},
		{"with accounts", provider.CapList | provider.CapCredential | provider.CapAccounts, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := fake{info: provider.Info{Name: "builtin", Capabilities: tt.caps}}
			p := ToSDK(reg)

			if got := p.Info(); !reflect.DeepEqual(got, reg.info) {
				t.Fatalf("Info() = %+v, want %+v", got, reg.info)
			}
			al, ok := p.(provider.AccountLister)
			if ok != tt.wantAccounts {
				t.Fatalf("implements AccountLister = %v, want %v", ok, tt.wantAccounts)
			}
			if ok {
				accts, err := al.Accounts(ctx)
				if err != nil || len(accts) != 1 || accts[0].ID != "builtin-account" {
					t.Fatalf("Accounts() = %+v, %v; want builtin-account", accts, err)
				}
			}
			if got := provider.Capabilities(p).Has(provider.CapAccounts); got != tt.wantAccounts {
				t.Fatalf("SDK capabilities include accounts = %v, want %v", got, tt.wantAccounts)
			}

			clusters, err := p.List(ctx, provider.ListRequest{})
			if err != nil || len(clusters) != 1 || clusters[0].ID != "builtin-cluster" {
				t.Fatalf("List() = %+v, %v; want builtin-cluster", clusters, err)
			}
			cred, err := p.Credential(ctx, provider.CredentialRequest{ClusterID: "x"})
			if err != nil || cred.Token != "tok-x" {
				t.Fatalf("Credential() = %+v, %v; want token tok-x", cred, err)
			}
		})
	}
}
