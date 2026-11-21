package session

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/internal/filelock"
)

// AuthCommand is how a session kubeconfig calls back into tug for
// credentials: `<Command> auth token --cluster <id>`.
type AuthCommand struct {
	Command string
	// ProxyURL, when set, sends the session through tugd's local API proxy
	// at <ProxyURL>/s/<session>, and the kubeconfig runs
	// `<Command> auth proxy-token --session <id>` instead.
	ProxyURL string
	// ProxyCA is the PEM certificate the kubeconfig trusts for the proxy.
	ProxyCA []byte
}

// Prompt styles written to the prompt file and read by the shell hook.
const (
	StyleNormal   = "normal"
	StyleReadOnly = "prod"
	StyleUnlocked = "unlocked"
)

// Connect points the session at a cluster: it writes a kubeconfig with one
// context whose user runs `tug auth token`, so no secret is written to disk.
// It ends any unlock and forgets the profile the session was connected
// with; SetProfile records a new one.
func (s *Session) Connect(cl *catalog.Cluster, namespace string, readOnly bool, auth AuthCommand) error {
	if namespace == "" {
		namespace = cl.DefaultNamespace
	}
	if namespace == "" {
		namespace = "default"
	}
	return s.update(func() (bool, error) {
		s.State.ClusterID = cl.ID
		s.State.ClusterName = cl.Name
		s.State.Namespace = namespace
		s.State.ReadOnly = readOnly
		s.State.UnlockedUntil = time.Time{}
		labels := cl.EffectiveLabels()
		s.State.Env = labels["env"]
		s.State.Production = s.m.production(labels)
		s.State.Profile = ""
		s.State.ProxyURL = auth.ProxyURL
		return true, writeKubeconfig(s.KubeconfigPath(), Kubeconfig(cl, namespace, s.State.ID, auth))
	})
}

// SetProxy repoints a proxied session at tugd's current address and
// certificate, for example after tugd restarted on another port. It changes
// nothing else, and nothing at all when the session no longer uses the
// proxy. It reports whether the kubeconfig changed: a client that loaded it
// before still has the old address.
func (s *Session) SetProxy(proxyURL string, ca []byte) (changed bool, err error) {
	err = s.update(func() (bool, error) {
		if s.State.ProxyURL == "" {
			return false, nil
		}
		cfg, err := clientcmd.LoadFromFile(s.KubeconfigPath())
		if err != nil {
			return false, fmt.Errorf("reading session kubeconfig: %w", err)
		}
		if !repoint(cfg, proxyURL, ca, s.State.ID) && s.State.ProxyURL == proxyURL {
			return false, nil
		}
		changed = true
		s.State.ProxyURL = proxyURL
		return true, writeKubeconfig(s.KubeconfigPath(), cfg)
	})
	return changed, err
}

// repoint points a proxied session kubeconfig at tugd's proxyURL for
// sessionID: the server address, the CA it trusts, and the session its exec
// entry asks a token for. It reports whether anything changed.
func repoint(cfg *clientcmdapi.Config, proxyURL string, ca []byte, sessionID string) bool {
	server := ProxyServer(proxyURL, sessionID)
	changed := false
	for _, cl := range cfg.Clusters {
		if cl.Server != server || !bytes.Equal(cl.CertificateAuthorityData, ca) {
			cl.Server, cl.CertificateAuthorityData = server, ca
			changed = true
		}
	}
	for _, ai := range cfg.AuthInfos {
		if ai.Exec == nil {
			continue
		}
		for i := 0; i+1 < len(ai.Exec.Args); i++ {
			if ai.Exec.Args[i] == "--session" && ai.Exec.Args[i+1] != sessionID {
				ai.Exec.Args[i+1] = sessionID
				changed = true
			}
		}
	}
	return changed
}

// ProxyServer is a session's API server address on tugd's proxy.
func ProxyServer(proxyURL, sessionID string) string {
	return strings.TrimSuffix(proxyURL, "/") + "/s/" + sessionID
}

// SetNamespace changes the namespace of a connected session.
func (s *Session) SetNamespace(ns string) error {
	return s.update(func() (bool, error) {
		if !s.State.Connected() {
			return false, errors.New("not connected; run `tug connect` first")
		}
		cfg, err := clientcmd.LoadFromFile(s.KubeconfigPath())
		if err != nil {
			return false, fmt.Errorf("reading session kubeconfig: %w", err)
		}
		kctx, ok := cfg.Contexts[cfg.CurrentContext]
		if !ok {
			return false, fmt.Errorf("session kubeconfig has no current context")
		}
		kctx.Namespace = ns
		s.State.Namespace = ns
		return true, writeKubeconfig(s.KubeconfigPath(), cfg)
	})
}

// Unlock allows writes on a read-only session for d.
func (s *Session) Unlock(d time.Duration) error {
	return s.update(func() (bool, error) {
		s.State.UnlockedUntil = s.m.now().Add(d)
		return true, nil
	})
}

// Lock ends an unlock early.
func (s *Session) Lock() error {
	return s.update(func() (bool, error) {
		s.State.UnlockedUntil = time.Time{}
		return true, nil
	})
}

// SetProfile records the profile the session was connected with, or clears
// it when name is empty.
func (s *Session) SetProfile(name string) error {
	return s.update(func() (bool, error) {
		if !s.State.Connected() {
			return false, errors.New("not connected; run `tug connect` first")
		}
		s.State.Profile = name
		return true, nil
	})
}

// SetPin records the pin that chose the connection, or clears it when p is
// nil, for example when the user connects by hand.
func (s *Session) SetPin(p *Pin) error {
	return s.update(func() (bool, error) {
		s.State.Pin = p
		return true, nil
	})
}

// NotePin records the pin file the user was told about.
func (s *Session) NotePin(noticed string) error {
	return s.update(func() (bool, error) {
		if s.State.PinNoticed == noticed {
			return false, nil
		}
		s.State.PinNoticed = noticed
		return true, nil
	})
}

// Disconnect forgets the cluster. The shell hook then restores the original
// KUBECONFIG.
func (s *Session) Disconnect() error {
	return s.update(func() (bool, error) {
		for _, f := range []string{kubeconfigFileName, promptFileName} {
			if err := os.Remove(filepath.Join(s.dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return false, err
			}
		}
		s.State = State{ID: s.State.ID, PID: s.State.PID, Created: s.State.Created}
		return true, nil
	})
}

// update changes the session with every other tug process kept out: it
// takes the session's lock, reads the state from disk again, runs fn, and
// saves when fn says so. A change made through a Session opened earlier
// therefore never undoes one made since, such as `tug lock` while
// `proxy-token` repoints the session. fn must not call update.
func (s *Session) update(fn func() (save bool, err error)) error {
	unlock, err := filelock.Lock(filepath.Join(s.dir, lockFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: session %s has ended", ErrNoSession, s.State.ID)
		}
		return fmt.Errorf("locking session %s: %w", s.State.ID, err)
	}
	defer unlock()
	fresh, err := s.m.Open(s.State.ID)
	if err != nil {
		return err
	}
	s.State = fresh.State
	save, err := fn()
	if err != nil || !save {
		return err
	}
	return s.saveWithPrompt()
}

// PromptStyle returns how the prompt should look at time now.
func (s *Session) PromptStyle(now time.Time) string {
	switch {
	case !s.State.ReadOnly:
		return StyleNormal
	case now.Before(s.State.UnlockedUntil):
		return StyleUnlocked
	default:
		return StyleReadOnly
	}
}

func (s *Session) saveWithPrompt() error {
	if err := s.Save(); err != nil {
		return err
	}
	if !s.State.Connected() {
		return nil
	}
	return config.WriteFileAtomic(filepath.Join(s.dir, promptFileName), []byte(s.promptLines(s.m.now())), 0o600)
}

// promptLines is the prompt file, which the shell hook reads with the
// shell's read builtin so that drawing the prompt starts no process.
//
// The first line, "<style>\t<cluster>:<namespace>", is all that hooks from
// earlier versions read, so shells started before an upgrade keep working.
// The second is "<unlocked-until>\t<env>\t<badge>\t<proxy>": when an unlock
// ends, in seconds since 1970 or 0 for none, which the hook compares with
// the shell's own clock so the prompt stops saying unlocked on time without
// another tug command; the cluster's env label; the badge that marks a
// production cluster; and "proxy" when the session goes through tugd. An
// empty field is "-", since read joins runs of tabs.
func (s *Session) promptLines(now time.Time) string {
	st := s.State
	text := st.ClusterName
	if st.Namespace != "" {
		text += ":" + st.Namespace
	}
	style := s.PromptStyle(now)
	until := int64(0)
	if style == StyleUnlocked {
		// Rounded down, so the prompt never claims a second too many.
		until = st.UnlockedUntil.Unix()
	}
	badge := ""
	if st.Production {
		badge = strings.ToUpper(st.Env)
		if badge == "" {
			badge = "PROD"
		}
	}
	proxy := ""
	if st.ProxyURL != "" {
		proxy = "proxy"
	}
	return style + "\t" + promptField(text) + "\n" +
		strconv.FormatInt(until, 10) + "\t" + promptField(st.Env) + "\t" + promptField(badge) + "\t" + promptField(proxy) + "\n"
}

// promptField makes s safe to print in a prompt and to read back as one
// field: control characters, which could move the cursor or retitle the
// terminal, become spaces, and an empty field becomes "-".
func promptField(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return ' '
		}
		return r
	}, s))
	if s == "" {
		return "-"
	}
	return s
}

// Kubeconfig builds the single-context kubeconfig for a cluster. With a
// proxy URL the cluster is reached through tugd, which holds the real
// credential, so the kubeconfig carries neither a CA nor a credential.
func Kubeconfig(cl *catalog.Cluster, namespace, sessionID string, auth AuthCommand) *clientcmdapi.Config {
	cfg := clientcmdapi.NewConfig()
	name := cl.Name
	args := []string{"auth", "token", "--cluster", cl.ID}
	if auth.ProxyURL != "" {
		cfg.Clusters[name] = &clientcmdapi.Cluster{Server: ProxyServer(auth.ProxyURL, sessionID), CertificateAuthorityData: auth.ProxyCA}
		args = []string{"auth", "proxy-token", "--session", sessionID}
	} else {
		cfg.Clusters[name] = &clientcmdapi.Cluster{
			Server:                   cl.Server,
			CertificateAuthorityData: cl.CAData,
			TLSServerName:            cl.TLSServerName,
			InsecureSkipTLSVerify:    cl.InsecureSkipTLSVerify,
			ProxyURL:                 cl.ProxyURL,
		}
	}
	cfg.AuthInfos[name] = &clientcmdapi.AuthInfo{
		Exec: &clientcmdapi.ExecConfig{
			APIVersion:      "client.authentication.k8s.io/v1",
			Command:         auth.Command,
			Args:            args,
			InteractiveMode: clientcmdapi.IfAvailableExecInteractiveMode,
			InstallHint:     "tug is not on PATH; install it or run `tug init` again",
		},
	}
	cfg.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: name, Namespace: namespace}
	cfg.CurrentContext = name
	return cfg
}

func writeKubeconfig(path string, cfg *clientcmdapi.Config) error {
	b, err := clientcmd.Write(*cfg)
	if err != nil {
		return fmt.Errorf("encoding kubeconfig: %w", err)
	}
	return config.WriteFileAtomic(path, b, 0o600)
}
