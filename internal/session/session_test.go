package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/paingha/tugcli/internal/catalog"
)

var prodEU = &catalog.Cluster{
	ID:               "kubeconfig/default/prod-eu",
	Name:             "prod-eu",
	Server:           "https://prod.example",
	CAData:           []byte("CA"),
	DefaultNamespace: "payments",
}

func TestConnectWritesNoSecrets(t *testing.T) {
	m := NewManager(t.TempDir())
	s, err := m.Create(os.Getpid(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Connect(prodEU, "", true, AuthCommand{Command: "/usr/local/bin/tug"}); err != nil {
		t.Fatal(err)
	}

	cfg, err := clientcmd.LoadFromFile(s.KubeconfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Contexts) != 1 || cfg.CurrentContext != "prod-eu" {
		t.Fatalf("want one context named prod-eu: %+v", cfg.Contexts)
	}
	if ns := cfg.Contexts["prod-eu"].Namespace; ns != "payments" {
		t.Fatalf("namespace = %q, want the cluster default", ns)
	}
	user := cfg.AuthInfos["prod-eu"]
	if user.Token != "" || user.TokenFile != "" || len(user.ClientKeyData) > 0 || user.ClientKey != "" {
		t.Fatal("session kubeconfig contains a secret")
	}
	if got := strings.Join(user.Exec.Args, " "); got != "auth token --cluster kubeconfig/default/prod-eu" {
		t.Fatalf("exec args = %q", got)
	}
	st, err := os.Stat(s.KubeconfigPath())
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no Unix permission bits (Go reports 0666); access there is
	// governed by the ACLs of the user's profile directory.
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("kubeconfig mode = %v, want 0600", st.Mode().Perm())
	}

	prompt, _ := os.ReadFile(filepath.Join(s.Dir(), promptFileName))
	if string(prompt) != "prod\tprod-eu:payments\n0\t-\t-\t-\n" {
		t.Fatalf("prompt = %q", prompt)
	}

	if err := s.SetNamespace("kube-system"); err != nil {
		t.Fatal(err)
	}
	cfg, _ = clientcmd.LoadFromFile(s.KubeconfigPath())
	if ns := cfg.Contexts["prod-eu"].Namespace; ns != "kube-system" {
		t.Fatalf("namespace after switch = %q", ns)
	}

	reopened, err := m.Open(s.State.ID)
	if err != nil || reopened.State.Namespace != "kube-system" || !reopened.State.ReadOnly {
		t.Fatalf("state not saved: %+v %v", reopened, err)
	}
}

func TestUnlockAndLock(t *testing.T) {
	m := NewManager(t.TempDir())
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	s, _ := m.Create(1, "")
	if err := s.Connect(prodEU, "", true, AuthCommand{Command: "tug"}); err != nil {
		t.Fatal(err)
	}
	if s.State.Writable(now) {
		t.Fatal("read-only session is writable")
	}
	if err := s.Unlock(15 * time.Minute); err != nil {
		t.Fatal(err)
	}
	if !s.State.Writable(now.Add(14*time.Minute)) || s.State.Writable(now.Add(16*time.Minute)) {
		t.Fatal("unlock window wrong")
	}
	if s.PromptStyle(now) != StyleUnlocked {
		t.Fatalf("style = %s", s.PromptStyle(now))
	}
	if err := s.Lock(); err != nil {
		t.Fatal(err)
	}
	if s.State.Writable(now) {
		t.Fatal("lock did not end the unlock")
	}
}

func TestChildSessionCopiesParent(t *testing.T) {
	m := NewManager(t.TempDir())
	parent, _ := m.Create(1, "")
	if err := parent.Connect(prodEU, "web", false, AuthCommand{Command: "tug"}); err != nil {
		t.Fatal(err)
	}
	child, err := m.Create(2, parent.State.ID)
	if err != nil {
		t.Fatal(err)
	}
	if child.State.ClusterID != prodEU.ID || child.State.Namespace != "web" {
		t.Fatalf("child state: %+v", child.State)
	}
	if err := child.SetNamespace("other"); err != nil {
		t.Fatal(err)
	}
	reparent, _ := m.Open(parent.State.ID)
	if reparent.State.Namespace != "web" {
		t.Fatal("child changed the parent's namespace")
	}
}

var proxied = AuthCommand{Command: "tug", ProxyURL: "https://127.0.0.1:41000", ProxyCA: []byte("PROXY-CA")}

// A nested shell of a proxied session goes through tugd as itself, so its
// own read-only state and lock/unlock govern its clients.
func TestChildOfProxiedSessionUsesItsOwnSession(t *testing.T) {
	m := NewManager(t.TempDir())
	parent, _ := m.Create(1, "")
	if err := parent.Connect(prodEU, "web", true, proxied); err != nil {
		t.Fatal(err)
	}
	if err := parent.Unlock(time.Hour); err != nil {
		t.Fatal(err)
	}
	child, err := m.Create(2, parent.State.ID)
	if err != nil {
		t.Fatal(err)
	}
	if child.State.ProxyURL != proxied.ProxyURL || child.State.Writable(time.Now()) {
		t.Fatalf("child state: %+v", child.State)
	}
	cfg, err := clientcmd.LoadFromFile(child.KubeconfigPath())
	if err != nil {
		t.Fatal(err)
	}
	cl := cfg.Clusters[cfg.Contexts[cfg.CurrentContext].Cluster]
	if want := ProxyServer(proxied.ProxyURL, child.State.ID); cl.Server != want || string(cl.CertificateAuthorityData) != "PROXY-CA" {
		t.Fatalf("child server %s (CA %q), want %s", cl.Server, cl.CertificateAuthorityData, want)
	}
	args := strings.Join(cfg.AuthInfos[cfg.Contexts[cfg.CurrentContext].AuthInfo].Exec.Args, " ")
	if args != "auth proxy-token --session "+child.State.ID {
		t.Fatalf("child exec args: %s", args)
	}
}

// A change made through a Session opened earlier must not undo one made
// since: here proxy-token repointing a session after `tug lock`.
func TestStaleSessionDoesNotUndoChanges(t *testing.T) {
	m := NewManager(t.TempDir())
	s, _ := m.Create(1, "")
	if err := s.Connect(prodEU, "", true, proxied); err != nil {
		t.Fatal(err)
	}
	if err := s.Unlock(time.Hour); err != nil {
		t.Fatal(err)
	}
	stale, _ := m.Open(s.State.ID) // as proxy-token opens it: unlocked
	if err := s.Lock(); err != nil {
		t.Fatal(err)
	}
	changed, err := stale.SetProxy("https://127.0.0.1:42000", []byte("NEW-CA"))
	if err != nil || !changed {
		t.Fatalf("SetProxy = %v, %v; want a change", changed, err)
	}
	now, _ := m.Open(s.State.ID)
	if now.State.Writable(time.Now()) {
		t.Fatal("repointing the session undid tug lock")
	}
	if now.State.ProxyURL != "https://127.0.0.1:42000" {
		t.Fatalf("proxy URL = %s", now.State.ProxyURL)
	}

	// The same address again changes nothing.
	if changed, err := now.SetProxy("https://127.0.0.1:42000", []byte("NEW-CA")); err != nil || changed {
		t.Fatalf("SetProxy with the same address = %v, %v; want no change", changed, err)
	}
	// Nor does repointing a session that has since stopped using the proxy.
	if err := now.Connect(prodEU, "", true, AuthCommand{Command: "tug"}); err != nil {
		t.Fatal(err)
	}
	if changed, err := stale.SetProxy("https://127.0.0.1:43000", nil); err != nil || changed {
		t.Fatalf("SetProxy on a session not using the proxy = %v, %v", changed, err)
	}
	if b, _ := os.ReadFile(s.KubeconfigPath()); strings.Contains(string(b), "43000") {
		t.Fatal("repointed the kubeconfig of a session that no longer uses the proxy")
	}
}

// A runtime directory another user can change is refused: they could
// replace a session kubeconfig, whose exec entry kubectl runs.
func TestRefusesRuntimeDirOthersCanChange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions")
	}
	// Not created yet: Create makes it private, and Open finds no session.
	fresh := NewManager(filepath.Join(t.TempDir(), "run"))
	if _, err := fresh.Open("abcdef123456"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Open before the directory exists: %v, want ErrNoSession", err)
	}
	s, err := fresh.Create(1, "")
	if err != nil {
		t.Fatal(err)
	}

	// Made writable by others afterwards, or by someone else first.
	if err := os.Chmod(fresh.runtime, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Open(s.State.ID); err == nil || !strings.Contains(err.Error(), "refusing to use") {
		t.Fatalf("Open: %v", err)
	}
	if _, err := fresh.Create(1, ""); err == nil || !strings.Contains(err.Error(), "refusing to use") {
		t.Fatalf("Create: %v", err)
	}
	if _, err := fresh.Sweep(time.Hour); err == nil {
		t.Fatal("Sweep worked in a directory others can change")
	}
	if err := fresh.Remove(s.State.ID); err == nil {
		t.Fatal("Remove worked in a directory others can change")
	}
}

func TestDisconnect(t *testing.T) {
	m := NewManager(t.TempDir())
	s, _ := m.Create(1, "")
	_ = s.Connect(prodEU, "", false, AuthCommand{Command: "tug"})
	if err := s.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.KubeconfigPath()); !os.IsNotExist(err) {
		t.Fatal("kubeconfig left after disconnect")
	}
	if s.State.Connected() {
		t.Fatal("still connected")
	}
}

func TestSweepRemovesDeadSessions(t *testing.T) {
	m := NewManager(t.TempDir())
	alive, _ := m.Create(os.Getpid(), "")
	dead, _ := m.Create(999999, "") // assumed unused
	n, err := m.Sweep(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("removed %d sessions, want 1", n)
	}
	if _, err := m.Open(alive.State.ID); err != nil {
		t.Fatal("live session removed")
	}
	if _, err := m.Open(dead.State.ID); err == nil {
		t.Fatal("dead session kept")
	}
}

func TestOpenRejectsBadIDs(t *testing.T) {
	m := NewManager(t.TempDir())
	for _, id := range []string{"../etc", "", "ABC", "a/b"} {
		if _, err := m.Open(id); err == nil {
			t.Errorf("Open(%q) succeeded", id)
		}
	}
}

func TestHooksRenderAndParse(t *testing.T) {
	checkers := map[Shell][]string{
		Zsh:  {"zsh", "-n"},
		Bash: {"bash", "-n"},
		Fish: {"fish", "--no-execute"},
		// PowerShell parses the file named by $TUG_TEST_HOOK.
		PowerShell: {"pwsh", "-NoProfile", "-NonInteractive", "-Command", `$errs = $null; ` +
			`$null = [System.Management.Automation.Language.Parser]::ParseFile($env:TUG_TEST_HOOK, [ref]$null, [ref]$errs); ` +
			`if ($errs) { $errs | ForEach-Object { $_.ToString() }; exit 1 }`},
	}
	for _, sh := range Shells() {
		t.Run(string(sh), func(t *testing.T) {
			script, err := Hook(sh, "/opt/tug's bin/tug")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(script, "session new") || strings.Contains(script, "{{") {
				t.Fatalf("hook not rendered:\n%s", script)
			}
			argv, ok := checkers[sh]
			if !ok {
				return
			}
			bin, err := exec.LookPath(argv[0])
			if err != nil {
				t.Skipf("%s not installed", argv[0])
			}
			f := filepath.Join(t.TempDir(), "hook")
			if sh == PowerShell {
				f += ".ps1"
			}
			if err := os.WriteFile(f, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bin, append(argv[1:], f)...)
			if sh == PowerShell {
				cmd = exec.Command(bin, argv[1:]...)
				cmd.Env = append(os.Environ(), "TUG_TEST_HOOK="+f)
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s rejects the hook: %v\n%s", sh, err, out)
			}
		})
	}
}

func TestQuote(t *testing.T) {
	tests := []struct {
		sh   Shell
		in   string
		want string
	}{
		{Bash, "it's", `'it'\''s'`},
		{Zsh, "/a b", `'/a b'`},
		{Fish, `a'b\c`, `'a\'b\\c'`},
		{PowerShell, "it's", `'it''s'`},
	}
	for _, tt := range tests {
		if got := tt.sh.Quote(tt.in); got != tt.want {
			t.Errorf("%s.Quote(%q) = %s, want %s", tt.sh, tt.in, got, tt.want)
		}
	}
}

// The prompt file carries what the hook needs to draw the prompt without
// asking tug: the env label, a production badge whatever the mode, whether
// the session goes through tugd, and when an unlock ends.
func TestPromptFile(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	prod := &catalog.Cluster{ID: "x/prod", Name: "prod-eu", Labels: map[string]string{"env": "prod"}}
	staging := &catalog.Cluster{ID: "x/staging", Name: "staging\x1b]0;owned\x07", Labels: map[string]string{"env": "staging", "tier": "critical"}}
	tests := []struct {
		name     string
		opts     []Option
		cl       *catalog.Cluster
		readOnly bool
		auth     AuthCommand
		unlock   time.Duration
		want     string
	}{
		{"production read-only", nil, prod, true, AuthCommand{Command: "tug"}, 0,
			"prod\tprod-eu:web\n0\tprod\tPROD\t-\n"},
		{"production read-write still has its badge", nil, prod, false, AuthCommand{Command: "tug"}, 0,
			"normal\tprod-eu:web\n0\tprod\tPROD\t-\n"},
		{"unlocked through tugd", nil, prod, true, proxied, 15 * time.Minute,
			fmt.Sprintf("unlocked\tprod-eu:web\n%d\tprod\tPROD\tproxy\n", now.Add(15*time.Minute).Unix())},
		{"not production, control characters made harmless", nil, staging, false, AuthCommand{Command: "tug"}, 0,
			"normal\tstaging ]0;owned :web\n0\tstaging\t-\t-\n"},
		{"production by configured labels", []Option{WithProduction(func(l map[string]string) bool { return l["tier"] == "critical" })},
			staging, true, AuthCommand{Command: "tug"}, 0,
			"prod\tstaging ]0;owned :web\n0\tstaging\tSTAGING\t-\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManager(t.TempDir(), tt.opts...)
			m.now = func() time.Time { return now }
			s, err := m.Create(1, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Connect(tt.cl, "web", tt.readOnly, tt.auth); err != nil {
				t.Fatal(err)
			}
			if tt.unlock > 0 {
				if err := s.Unlock(tt.unlock); err != nil {
					t.Fatal(err)
				}
			}
			b, err := os.ReadFile(filepath.Join(s.Dir(), promptFileName))
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tt.want {
				t.Fatalf("prompt file = %q, want %q", b, tt.want)
			}
		})
	}
}

func TestProfileFollowsTheConnection(t *testing.T) {
	m := NewManager(t.TempDir())
	s, _ := m.Create(1, "")
	if err := s.SetProfile("payments"); err == nil {
		t.Fatal("SetProfile on a session that is not connected succeeded")
	}
	if err := s.Connect(prodEU, "", true, AuthCommand{Command: "tug"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProfile("payments"); err != nil {
		t.Fatal(err)
	}
	child, err := m.Create(2, s.State.ID)
	if err != nil || child.State.Profile != "payments" {
		t.Fatalf("child profile = %q, %v; want the parent's", child.State.Profile, err)
	}
	if c := s.State.Current(); c.Profile != "payments" {
		t.Fatalf("Current() = %+v, want the profile", c)
	}
	// Connecting some other way forgets the profile.
	if err := s.Connect(prodEU, "web", true, AuthCommand{Command: "tug"}); err != nil {
		t.Fatal(err)
	}
	if again, _ := m.Open(s.State.ID); again.State.Profile != "" {
		t.Fatalf("profile after connecting by hand = %q", again.State.Profile)
	}
}
