package plugin

import (
	"testing"

	"github.com/paingha/tugcli/pkg/provider"
)

func TestPluginTextIsCleaned(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"line one\nline two\tend", "line one line two end"},
		{"\x1b[2J\x1b[31mred\x1b[0m", "[2J[31mred[0m"},
		{"bell\a and del\x7f", "bell and del"},
		{"csi \u009b31m", "csi 31m"},
		{"evil\u202egnp.exe", "evilgnp.exe"},
		{"café ✓", "café ✓"},
	}
	for _, tt := range tests {
		if got := cleanText(tt.in); got != tt.want {
			t.Errorf("cleanText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}

	p := &Provider{plugin: &Plugin{Manifest: &Manifest{Metadata: Metadata{Name: "x"}}}, runner: &Runner{Name: "x"}}
	out := []byte(`{"apiVersion":"provider/v1alpha1","kind":"Error","code":"Unauthenticated","message":"token \u001b[8mhidden","hint":"run\u001b]0;title\u0007 tug import"}`)
	_, err := decode(p, out, provider.DecodeList)
	pe := provider.AsError(err)
	if pe.Code != provider.Unauthenticated || pe.Message != "token [8mhidden" || pe.Hint != "run]0;title tug import" {
		t.Fatalf("decode gave %+v, want the escape characters gone", pe)
	}
}
