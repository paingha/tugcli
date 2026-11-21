package editor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestArgs(t *testing.T) {
	tests := []struct {
		editor string
		line   int
		want   string
	}{
		{"vim", 12, "vim +12 f.yaml"},
		{"/usr/local/bin/nvim", 3, "/usr/local/bin/nvim +3 f.yaml"},
		{"emacsclient -t", 7, "emacsclient -t +7 f.yaml"},
		{"code --wait", 12, "code --wait --goto f.yaml:12"},
		{"hx", 4, "hx f.yaml:4"},
		{"Code.exe -w", 2, "Code.exe -w --goto f.yaml:2"},
		{"notepad", 9, "notepad f.yaml"},
		{"vim", 0, "vim f.yaml"},
		{"   ", 1, ""},
	}
	for _, tt := range tests {
		if got := strings.Join(Args(tt.editor, "f.yaml", tt.line), " "); got != tt.want {
			t.Errorf("Args(%q, %d) = %q, want %q", tt.editor, tt.line, got, tt.want)
		}
	}
}

func TestCommand(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cmd, err := Command(context.Background(), env(map[string]string{"VISUAL": "nano", "EDITOR": "vim"}), "f.yaml", 5)
	if err != nil {
		t.Fatal(err)
	}
	if got := cmd.Args; !slices.Equal(got, []string{"nano", "+5", "f.yaml"}) {
		t.Errorf("Command with $VISUAL and $EDITOR runs %v, want $VISUAL", got)
	}
	cmd, err = Command(context.Background(), env(map[string]string{"EDITOR": "vim"}), "f.yaml", 5)
	if err != nil || !slices.Equal(cmd.Args, []string{"vim", "+5", "f.yaml"}) {
		t.Errorf("Command with $EDITOR = %v, %v", cmd, err)
	}
	if _, err := Command(context.Background(), env(nil), "f.yaml", 5); !errors.Is(err, ErrNoEditor) {
		t.Errorf("Command without an editor = %v, want ErrNoEditor", err)
	}
}
