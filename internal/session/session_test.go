package session

import (
	"os"
	"os/exec"
	"path/filepath"
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
	if st, err := os.Stat(s.KubeconfigPath()); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("kubeconfig mode: %v %v", st.Mode(), err)
	}

	prompt, _ := os.ReadFile(filepath.Join(s.Dir(), promptFileName))
	if string(prompt) != "prod\tprod-eu:payments\n" {
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
			if err := os.WriteFile(f, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(bin, append(argv[1:], f)...).CombinedOutput(); err != nil {
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
