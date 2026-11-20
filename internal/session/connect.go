package session

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/config"
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
func (s *Session) Connect(cl *catalog.Cluster, namespace string, readOnly bool, auth AuthCommand) error {
	if namespace == "" {
		namespace = cl.DefaultNamespace
	}
	if namespace == "" {
		namespace = "default"
	}
	s.State.ClusterID = cl.ID
	s.State.ClusterName = cl.Name
	s.State.Namespace = namespace
	s.State.ReadOnly = readOnly
	s.State.UnlockedUntil = time.Time{}
	s.State.ProxyURL = auth.ProxyURL
	if err := writeKubeconfig(s.KubeconfigPath(), Kubeconfig(cl, namespace, s.State.ID, auth)); err != nil {
		return err
	}
	return s.saveWithPrompt()
}

// SetProxy repoints a proxied session at tugd's current address and
// certificate, for example after tugd restarted on another port.
func (s *Session) SetProxy(proxyURL string, ca []byte) error {
	if s.State.ProxyURL == "" {
		return nil
	}
	cfg, err := clientcmd.LoadFromFile(s.KubeconfigPath())
	if err != nil {
		return fmt.Errorf("reading session kubeconfig: %w", err)
	}
	server := ProxyServer(proxyURL, s.State.ID)
	changed := false
	for _, cl := range cfg.Clusters {
		if cl.Server != server || !bytes.Equal(cl.CertificateAuthorityData, ca) {
			cl.Server, cl.CertificateAuthorityData = server, ca
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := writeKubeconfig(s.KubeconfigPath(), cfg); err != nil {
		return err
	}
	s.State.ProxyURL = proxyURL
	return s.Save()
}

// ProxyServer is a session's API server address on tugd's proxy.
func ProxyServer(proxyURL, sessionID string) string {
	return strings.TrimSuffix(proxyURL, "/") + "/s/" + sessionID
}

// SetNamespace changes the namespace of a connected session.
func (s *Session) SetNamespace(ns string) error {
	if !s.State.Connected() {
		return errors.New("not connected; run `tug connect` first")
	}
	cfg, err := clientcmd.LoadFromFile(s.KubeconfigPath())
	if err != nil {
		return fmt.Errorf("reading session kubeconfig: %w", err)
	}
	kctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok {
		return fmt.Errorf("session kubeconfig has no current context")
	}
	kctx.Namespace = ns
	if err := writeKubeconfig(s.KubeconfigPath(), cfg); err != nil {
		return err
	}
	s.State.Namespace = ns
	return s.saveWithPrompt()
}

// Unlock allows writes on a read-only session for d.
func (s *Session) Unlock(d time.Duration) error {
	s.State.UnlockedUntil = s.m.now().Add(d)
	return s.saveWithPrompt()
}

// Lock ends an unlock early.
func (s *Session) Lock() error {
	s.State.UnlockedUntil = time.Time{}
	return s.saveWithPrompt()
}

// Disconnect forgets the cluster. The shell hook then restores the original
// KUBECONFIG.
func (s *Session) Disconnect() error {
	for _, f := range []string{kubeconfigFileName, promptFileName} {
		if err := os.Remove(filepath.Join(s.dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	s.State = State{ID: s.State.ID, PID: s.State.PID, Created: s.State.Created}
	return s.Save()
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
	// The hook reads "<style>\t<text>" with the shell's read builtin, so the
	// prompt costs no process per command. An unlock expiry is not reflected
	// until the next tug command; the guard itself always checks the time.
	text := s.State.ClusterName
	if s.State.Namespace != "" {
		text += ":" + s.State.Namespace
	}
	text = strings.NewReplacer("\t", " ", "\n", " ").Replace(text)
	line := s.PromptStyle(s.m.now()) + "\t" + text + "\n"
	return config.WriteFileAtomic(filepath.Join(s.dir, promptFileName), []byte(line), 0o600)
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
