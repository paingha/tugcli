package plugin

import (
	"errors"
	"strings"
	"unicode"

	"github.com/paingha/tugcli/pkg/provider"
)

// unprintable reports whether r could change what a terminal shows rather
// than show itself: a control character, which can move the cursor, change
// colours or rewrite earlier output, or a bidirectional control, which can
// reorder the text around it.
func unprintable(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r)
}

// cleanText makes text a plugin wrote safe for tug to print: tabs and line
// breaks become spaces, and other unprintable characters are dropped.
func cleanText(s string) string {
	if !strings.ContainsFunc(s, unprintable) {
		return s
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case unprintable(r):
			return -1
		default:
			return r
		}
	}, s)
}

// cleanError cleans the text of an error a plugin answered with.
func cleanError(err error) error {
	var pe *provider.Error
	if !errors.As(err, &pe) {
		return err
	}
	c := *pe
	c.Code = provider.Code(cleanText(string(c.Code)))
	c.Message, c.Hint = cleanText(c.Message), cleanText(c.Hint)
	return &c
}
