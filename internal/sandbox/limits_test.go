package sandbox

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCommandLimit(t *testing.T) {
	supported := SupportedLimits().CPU || SupportedLimits().Memory
	l := Limits{CPU: 30 * time.Second, Memory: 64 << 20}
	hasLimits := func(c *Command) bool {
		return slices.ContainsFunc(c.Env, func(kv string) bool { return strings.HasPrefix(kv, limitsEnv+"=") })
	}

	c := &Command{Path: "/p/plugin", Args: []string{"list"}, Env: []string{"A=1"}}
	if err := c.Limit("/bin/tug", Limits{}); err != nil || c.Path != "/p/plugin" || hasLimits(c) {
		t.Fatalf("Limit with no limits changed the command: %+v, %v", c, err)
	}

	c = &Command{Path: "/p/plugin", Args: []string{"list"}, Env: []string{"A=1"}}
	if err := c.Limit("/bin/tug", l); err != nil {
		t.Fatal(err)
	}
	switch {
	case !supported && (c.Path != "/p/plugin" || hasLimits(c)):
		t.Fatalf("Limit where limits are not supported changed the command: %+v", c)
	case supported && (c.Path != "/bin/tug" || !slices.Equal(c.Args, []string{ExecCommand, "/p/plugin", "list"}) || !hasLimits(c)):
		t.Fatalf("Limit = %+v, want the plugin run through %s with the limits", c, ExecCommand)
	}

	// A command that already runs through ExecCommand only gains the
	// limits.
	c = &Command{Path: "/bin/tug", Args: []string{ExecCommand, "/p/plugin", "list"}}
	if err := c.Limit("/bin/tug", l); err != nil {
		t.Fatal(err)
	}
	if supported && (len(c.Args) != 3 || !hasLimits(c)) {
		t.Fatalf("Limit of a command already through %s = %+v", ExecCommand, c)
	}

	if supported {
		c = &Command{Path: "/p/plugin"}
		if err := c.Limit("", l); err == nil {
			t.Fatal("Limit without tug's executable succeeded")
		}
	}

	t.Setenv(limitsEnv, `{"cpu":30000000000,"memory":67108864}`)
	got, ok, err := takeLimits()
	if err != nil || !ok || got != l {
		t.Fatalf("takeLimits = %+v, %v, %v; want %+v", got, ok, err, l)
	}
}
