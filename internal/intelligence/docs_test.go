package intelligence

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDocsShowTheGuidance keeps the guide's copy of the standing guidance,
// the manner and the ask word for word what is sent, since the guide
// promises it is.
func TestDocsShowTheGuidance(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "content", "docs", "guides", "ai-explanations.mdx"))
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.ReplaceAll(string(b), "\r\n", "\n")
	for name, text := range map[string]string{"standing guidance": StableGuidance(), "manner": Manner(), "ask": Ask()} {
		if !strings.Contains(doc, "```text\n"+text+"```\n") {
			t.Errorf("docs/content/docs/guides/ai-explanations.mdx does not show the %s word for word; copy it from internal/intelligence/prompts", name)
		}
	}
}
