package passthrough

import (
	"strings"
	"testing"
)

func TestIsRead(t *testing.T) {
	tests := []struct {
		args string
		want bool
	}{
		{"get pods -A", true},
		{"-n kube-system get pods", true},
		{"--context=x describe deploy api", true},
		{"logs -f deploy/api", true},
		{"auth can-i delete pods", true},
		{"rollout status deploy/api", true},
		{"config view", true},
		{"apply -f x.yaml --dry-run=server", true},
		{"--help", true},
		{"delete pod api-0", false},
		{"-n web delete pod api-0", false},
		{"rollout restart deploy/api", false},
		{"exec -it api-0 -- sh", false},
		{"scale deploy/api --replicas 3", false},
		{"config use-context other", false},
		{"proxy", false},
		{"neat get pod x", false}, // unknown plugin: assume it writes
	}
	for _, tt := range tests {
		t.Run(tt.args, func(t *testing.T) {
			if got := IsRead(strings.Fields(tt.args)); got != tt.want {
				t.Errorf("IsRead(%q) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

func TestWords(t *testing.T) {
	got := Words(strings.Fields("-n web --context=prod get pods -o wide -- extra"))
	if strings.Join(got, " ") != "get pods" {
		t.Fatalf("Words = %v", got)
	}
}
