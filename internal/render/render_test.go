package render

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFormat(t *testing.T) {
	tests := []struct {
		in      string
		want    Format
		wantErr bool
	}{
		{"", FormatTable, false},
		{"table", FormatTable, false},
		{"TABLE", FormatTable, false},
		{"json", FormatJSON, false},
		{"Json", FormatJSON, false},
		{"yaml", FormatYAML, false},
		{"name", FormatName, false},
		{"xml", "", true},
		{"yml", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseFormat(tt.in)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), tt.in) {
					t.Fatalf("ParseFormat(%q) = %q, %v; want an error naming %q", tt.in, got, err, tt.in)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ParseFormat(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
			}
		})
	}
}

type item struct {
	Name   string   `json:"name"`
	Labels []string `json:"labels,omitempty"`
}

func TestStructured(t *testing.T) {
	v := []item{{Name: "a", Labels: []string{"x"}}, {Name: "b"}}
	tests := []struct {
		format Format
		want   string
	}{
		{FormatJSON, "[\n  {\n    \"name\": \"a\",\n    \"labels\": [\n      \"x\"\n    ]\n  },\n  {\n    \"name\": \"b\"\n  }\n]\n"},
		{FormatYAML, "- labels:\n  - x\n  name: a\n- name: b\n"},
	}
	for _, tt := range tests {
		t.Run(string(tt.format), func(t *testing.T) {
			var buf bytes.Buffer
			if err := Structured(&buf, tt.format, v); err != nil {
				t.Fatal(err)
			}
			if got := buf.String(); got != tt.want {
				t.Fatalf("Structured(%s) =\n%s\nwant\n%s", tt.format, got, tt.want)
			}
		})
	}
}

func TestStructuredErrors(t *testing.T) {
	tests := []struct {
		name   string
		format Format
		v      any
	}{
		{"table is not structured", FormatTable, item{}},
		{"name is not structured", FormatName, item{}},
		{"json cannot encode a channel", FormatJSON, make(chan int)},
		{"yaml cannot encode a channel", FormatYAML, make(chan int)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Structured(&bytes.Buffer{}, tt.format, tt.v); err == nil {
				t.Fatalf("Structured(%s, %T) succeeded, want an error", tt.format, tt.v)
			}
		})
	}
}

func TestTable(t *testing.T) {
	tests := []struct {
		name   string
		header []string
		rows   [][]string
		want   string
	}{
		{
			name:   "aligned columns",
			header: []string{"NAME", "STATUS", "AGE"},
			rows: [][]string{
				{"a", "Ready", "1d"},
				{"longer-name", "NotReady", "12m"},
			},
			want: "" +
				"NAME          STATUS     AGE\n" +
				"a             Ready      1d\n" +
				"longer-name   NotReady   12m\n",
		},
		{
			name:   "header only",
			header: []string{"NAME", "STATUS"},
			want:   "NAME   STATUS\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := Table(&buf, tt.header, tt.rows); err != nil {
				t.Fatal(err)
			}
			if got := buf.String(); got != tt.want {
				t.Fatalf("Table =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestColorsForDisabled(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })

	tests := []struct {
		name    string
		noColor string
		term    string
		w       io.Writer
	}{
		{"NO_COLOR set", "1", "xterm-256color", os.Stdout},
		{"dumb terminal", "", "dumb", os.Stdout},
		{"writer is not a file", "", "xterm-256color", &bytes.Buffer{}},
		{"file is not a terminal", "", "xterm-256color", file},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("NO_COLOR", tt.noColor)
			t.Setenv("TERM", tt.term)
			if c := ColorsFor(tt.w); c.Enabled {
				t.Fatalf("ColorsFor = %+v, want colour disabled", c)
			}
		})
	}
}

func TestColors(t *testing.T) {
	on, off := Colors{Enabled: true}, Colors{}
	tests := []struct {
		name string
		fn   func(Colors, string) string
		code string
	}{
		{"Red", Colors.Red, "1;31"},
		{"Yellow", Colors.Yellow, "33"},
		{"Green", Colors.Green, "32"},
		{"Dim", Colors.Dim, "2"},
		{"Bold", Colors.Bold, "1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, want := tt.fn(on, "x"), "\x1b["+tt.code+"mx\x1b[0m"; got != want {
				t.Errorf("enabled %s(%q) = %q, want %q", tt.name, "x", got, want)
			}
			if got := tt.fn(off, "x"); got != "x" {
				t.Errorf("disabled %s(%q) = %q, want %q", tt.name, "x", got, "x")
			}
		})
	}
}

func TestPick(t *testing.T) {
	on := Colors{Enabled: true}
	keys := []string{"api-0", "api-1", "worker-0", "worker-1", "db-0", "cache-0", "web-0", "web-1"}
	codes := map[string]bool{}
	for _, k := range keys {
		first := on.Pick(k, "line")
		for range 3 {
			if again := on.Pick(k, "other line"); colourOf(again) != colourOf(first) {
				t.Fatalf("Pick(%q) changed colour: %q then %q", k, first, again)
			}
		}
		if !strings.HasSuffix(first, "line\x1b[0m") {
			t.Fatalf("Pick(%q) = %q, want the text wrapped in a colour", k, first)
		}
		codes[colourOf(first)] = true
	}
	if len(codes) < 2 {
		t.Fatalf("%d keys all got the same colour %v; want a spread", len(keys), codes)
	}
	if got := (Colors{}).Pick("api-0", "line"); got != "line" {
		t.Fatalf("disabled Pick = %q, want %q", got, "line")
	}
}

// colourOf returns the escape sequence that starts s.
func colourOf(s string) string {
	i := strings.IndexByte(s, 'm')
	if i < 0 {
		return ""
	}
	return s[:i+1]
}

func TestPrettyJSON(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"object", `{"level":"info","n":1}`, "{\n  \"level\": \"info\",\n  \"n\": 1\n}"},
		{"surrounding space", "  {\"a\":true}\r", "{\n  \"a\": true\n}"},
		{"plain text", "listening on :8080", "listening on :8080"},
		{"invalid JSON", `{"a":`, `{"a":`},
		{"array", `[1,2]`, `[1,2]`},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PrettyJSON(tt.in); got != tt.want {
				t.Fatalf("PrettyJSON(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
