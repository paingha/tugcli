package ini

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	f, err := Parse(strings.NewReader(`
# comment
; also a comment
top = level

[default]
region = us-east-1
output=json

[profile work]
s3 =
  max_concurrent_requests = 20
`))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ section, key, want string }{
		{"", "top", "level"},
		{"default", "region", "us-east-1"},
		{"default", "output", "json"},
		{"profile work", "s3", "\nmax_concurrent_requests = 20"},
		{"missing", "x", ""},
	}
	for _, tt := range tests {
		if got := f.Get(tt.section, tt.key); got != tt.want {
			t.Errorf("Get(%q, %q) = %q, want %q", tt.section, tt.key, got, tt.want)
		}
	}
}

func TestParseFileMissingIsEmpty(t *testing.T) {
	f, err := ParseFile(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(f) != 0 {
		t.Fatalf("got %v, %v", f, err)
	}
}
