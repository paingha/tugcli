package apiinfo

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/version"
)

func TestUpstreamTable(t *testing.T) {
	up := Upstream()
	if up.Checked == "" || len(up.Sources) == 0 || len(up.Releases) == 0 || len(up.Removals) == 0 {
		t.Fatalf("Upstream() = %+v, want a dated table with sources, releases and removals", up)
	}
	tests := []struct {
		gv, kind  string
		removedIn string
		to        string
	}{
		{"flowcontrol.apiserver.k8s.io/v1beta3", "FlowSchema", "1.32", "flowcontrol.apiserver.k8s.io/v1"},
		{"autoscaling/v2beta2", "HorizontalPodAutoscaler", "1.26", "autoscaling/v2"},
		{"policy/v1beta1", "PodDisruptionBudget", "1.25", "policy/v1"},
		{"policy/v1beta1", "PodSecurityPolicy", "1.25", ""},
		{"storage.k8s.io/v1beta1", "CSIStorageCapacity", "1.27", "storage.k8s.io/v1"},
		{"storage.k8s.io/v1beta1", "StorageClass", "1.22", "storage.k8s.io/v1"},
		{"extensions/v1beta1", "Ingress", "1.22", "networking.k8s.io/v1"},
		{"extensions/v1beta1", "Deployment", "1.16", "apps/v1"},
	}
	for _, tt := range tests {
		r, ok := up.Removal(tt.gv, tt.kind)
		if !ok || r.RemovedIn != tt.removedIn || r.Replacement != tt.to {
			t.Errorf("Removal(%s, %s) = %+v, %v, want removed in %s, replaced by %q", tt.gv, tt.kind, r, ok, tt.removedIn, tt.to)
		}
	}
	if r, _ := up.Removal("policy/v1beta1", "PodSecurityPolicy"); !strings.Contains(r.Note, "Pod Security Admission") {
		t.Errorf("Removal(policy/v1beta1, PodSecurityPolicy).Note = %q, want the way forward", r.Note)
	}
	for _, gvk := range [][2]string{{"apps/v1", "Deployment"}, {"policy/v1beta1", "Eviction"}, {"autoscaling/v2", "HorizontalPodAutoscaler"}} {
		if r, ok := up.Removal(gvk[0], gvk[1]); ok {
			t.Errorf("Removal(%s, %s) = %+v, want none", gvk[0], gvk[1], r)
		}
	}
}

func TestParseTableRejects(t *testing.T) {
	tests := []struct{ name, yaml, want string }{
		{"unknown field", "checked: \"2026-01-01\"\nreleses: []\n", "unknown field"},
		{"bad checked date", "checked: \"soon\"\n", "checked"},
		{"bad end of life", "checked: \"2026-01-01\"\nreleases:\n- {version: \"1.35\", endOfLife: \"2027-02\"}\n", "endOfLife"},
		{"oldest first", "checked: \"2026-01-01\"\nreleases:\n- {version: \"1.34\", endOfLife: \"2026-10-27\"}\n- {version: \"1.35\", endOfLife: \"2027-02-28\"}\n", "newest first"},
		{"not a minor", "checked: \"2026-01-01\"\nreleases:\n- {version: \"1.35.1\", endOfLife: \"2027-02-28\"}\n", "not a minor version"},
		{"no replacement or note", "checked: \"2026-01-01\"\nremovals:\n- {groupVersion: a/v1beta1, kinds: [A], removedIn: \"1.30\"}\n", "replacement or a note"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseTable([]byte(tt.yaml)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("parseTable(%q) = %v, want an error with %q", tt.yaml, err, tt.want)
			}
		})
	}
}

func TestSupport(t *testing.T) {
	table := &Table{Checked: "2026-11-21", Releases: []Release{
		{Version: "1.37", EndOfLife: "2027-10-28"},
		{Version: "1.36", EndOfLife: "2027-06-28"},
		{Version: "1.35", EndOfLife: "2027-02-28"},
		{Version: "1.34", EndOfLife: "2026-10-27"},
	}}
	day := func(s string) time.Time {
		d, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	tests := []struct {
		minor, now string
		want       Support
	}{
		{"1.36", "2026-11-21", Support{Status: Supported, EndOfLife: "2027-06-28", OldestSupported: "1.35", Behind: 1}},
		{"1.35", "2026-11-21", Support{Status: Supported, EndOfLife: "2027-02-28", OldestSupported: "1.35", Behind: 2}},
		{"1.35", "2026-12-28", Support{Status: Maintenance, EndOfLife: "2027-02-28", OldestSupported: "1.35", Behind: 2}},
		{"1.34", "2026-10-27", Support{Status: EndOfLife, EndOfLife: "2026-10-27", OldestSupported: "1.35", Behind: 3}},
		{"1.34", "2026-10-26", Support{Status: Maintenance, EndOfLife: "2026-10-27", OldestSupported: "1.34", Behind: 3}},
		{"1.29", "2026-11-21", Support{Status: EndOfLife, OldestSupported: "1.35", Behind: 8}},
		{"1.38", "2026-11-21", Support{Status: Unknown, OldestSupported: "1.35"}},
		{"", "2026-11-21", Support{Status: Unknown, OldestSupported: "1.35"}},
	}
	for _, tt := range tests {
		tt.want.Newest, tt.want.TableChecked = "1.37", "2026-11-21"
		if got := table.Support(tt.minor, day(tt.now)); got != tt.want {
			t.Errorf("Support(%q, %s) = %+v, want %+v", tt.minor, tt.now, got, tt.want)
		}
	}
}

func TestServerOf(t *testing.T) {
	tests := []struct {
		info version.Info
		want string
	}{
		{version.Info{GitVersion: "v1.35.1", Major: "1", Minor: "35"}, "1.35"},
		{version.Info{GitVersion: "v1.34.3-eks-ac2d5a0", Major: "1", Minor: "34+"}, "1.34"},
		{version.Info{Major: "1", Minor: "33+"}, "1.33"},
		{version.Info{GitVersion: "unknown"}, ""},
	}
	for _, tt := range tests {
		if got := ServerOf(&tt.info); got.Minor != tt.want || got.GitVersion != tt.info.GitVersion {
			t.Errorf("ServerOf(%+v) = %+v, want minor %q", tt.info, got, tt.want)
		}
	}
}

func TestAtLeast(t *testing.T) {
	tests := []struct {
		v, r string
		want bool
	}{
		{"1.25", "1.25", true},
		{"1.26", "1.25", true},
		{"1.9", "1.16", false},
		{"2.0", "1.30", true},
		{"", "1.25", false},
	}
	for _, tt := range tests {
		if got := AtLeast(tt.v, tt.r); got != tt.want {
			t.Errorf("AtLeast(%q, %q) = %v, want %v", tt.v, tt.r, got, tt.want)
		}
	}
}
