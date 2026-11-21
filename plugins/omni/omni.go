// Package omni is tug's first-party provider plugin for the Talos Linux
// clusters that Sidero Omni manages. It works through omnictl, which the
// user has already set up with an omniconfig: clusters come from omnictl get
// clusters, and the endpoint, OIDC issuer and client from the kubeconfig
// that omnictl kubeconfig downloads.
//
// Omni serves every cluster's Kubernetes API through one proxy and tells the
// clusters apart by the cluster:<name> scope of the ID token, which Omni's
// own issuer signs. Each cluster therefore gets an oidc credential for that
// issuer and scope, and tug signs the user in itself, as kubectl oidc-login
// does with the kubeconfig Omni writes. An account with an Omni service
// account key, for automation, gets Kubernetes service account tokens from
// omnictl kubeconfig --service-account instead.
package omni

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/internal/apiclient"
)

// Name and version of the plugin.
const (
	Name    = "omni"
	Version = "0.1.0"
)

// Account settings.
const (
	// ConfigContext is the omniconfig context omnictl uses. It defaults to
	// the current context of the omniconfig.
	ConfigContext = "context"
	// ConfigEndpoint is the Omni endpoint, which omnictl gets as
	// OMNI_ENDPOINT in place of the omniconfig context's. A service account
	// key does not name its Omni instance, so it needs this.
	ConfigEndpoint = "endpoint"
	// ConfigServiceAccountUser is the user, the sub claim, of the Kubernetes
	// service account tokens. It defaults to the Omni service account's
	// name.
	ConfigServiceAccountUser = "serviceAccountUser"
	// ConfigServiceAccountGroups is the comma-separated list of groups of
	// those tokens. It defaults to omnictl's, system:masters.
	ConfigServiceAccountGroups = "serviceAccountGroups"
	// ConfigTokenTTL is how long those tokens last, as a Go duration such as
	// 30m or 8h.
	ConfigTokenTTL = "tokenTTL"
)

// SecretServiceAccountKey is an Omni service account key, the
// OMNI_SERVICE_ACCOUNT_KEY that omnictl serviceaccount create prints. It is
// optional: without it, omnictl uses the user's own sign-in.
const SecretServiceAccountKey = "service_account_key"

// ConfigEnv names the omniconfig file when it is not ~/.talos/omni/config.
const ConfigEnv = "OMNICONFIG"

// InstallHint tells users where to get omnictl.
const InstallHint = "install omnictl with `brew install siderolabs/tap/sidero-tools`, or see https://docs.siderolabs.com/omni/getting-started/install-and-configure-omnictl"

const (
	// defaultTokenTTL keeps a leaked service account token short-lived;
	// tug asks for a new one when it runs out.
	defaultTokenTTL = time.Hour
	// maxTokenTTL is the longest service account token Omni issues.
	maxTokenTTL = 10 * 365 * 24 * time.Hour
)

// scopeClusterPrefix starts the OIDC scope that names the cluster an ID
// token is for. Omni's issuer allows only scopes with this prefix besides
// the standard ones.
const scopeClusterPrefix = "cluster:"

// signInMessages start the lines omnictl prints on stderr when it has no
// valid key and is about to start a sign-in in the browser. They come from
// the interceptor in github.com/siderolabs/go-api-signature.
var signInMessages = []string{
	"Could not authenticate:",
	"Please visit this page to authenticate:",
	"Attempting to open URL:",
}

// omnictlEnv are the variables the plugin sets for omnictl itself, so that
// none of them leaks in from the plugin's own environment.
var omnictlEnv = []string{"BROWSER", "OMNI_ENDPOINT", "OMNI_SERVICE_ACCOUNT_KEY", "SIDERO_SERVICE_ACCOUNT_KEY"}

const keyHint = "create a service account with `omnictl serviceaccount create <name>` and give tug the OMNI_SERVICE_ACCOUNT_KEY it prints"

// Provider implements the plugin.
type Provider struct {
	// now returns the current time; tests replace it.
	now func() time.Time
}

var _ provider.Provider = (*Provider)(nil)

// New returns the plugin.
func New() *Provider { return &Provider{now: time.Now} }

// Info implements provider.Provider.
func (p *Provider) Info() provider.Info {
	return provider.Info{
		Name:         Name,
		Version:      Version,
		Description:  "Talos Linux clusters managed by Sidero Omni, through omnictl",
		Capabilities: provider.CapList | provider.CapCredential,
		Secrets: []provider.SecretSpec{{
			Name:     SecretServiceAccountKey,
			Prompt:   "Omni service account key (OMNI_SERVICE_ACCOUNT_KEY)",
			Optional: true,
		}},
	}
}

// serviceAccount is a decoded Omni service account key: base64 of this
// JSON, as go-api-signature's serviceaccount package encodes it.
type serviceAccount struct {
	Name   string `json:"name"`
	PGPKey string `json:"pgp_key"`
}

// session is how omnictl reaches Omni for one account.
type session struct {
	context  string
	endpoint string
	// key and sa are set for a service account.
	key string
	sa  serviceAccount
}

// newSession checks the account's key and settings. The key is checked
// first, so that a bad key is reported as such whatever the settings.
func newSession(acct provider.Account, secrets map[string]string) (*session, error) {
	s := &session{context: acct.ConfigString(ConfigContext)}
	if key := strings.TrimSpace(secrets[SecretServiceAccountKey]); key != "" {
		data, err := base64.StdEncoding.DecodeString(key)
		if err == nil {
			err = json.Unmarshal(data, &s.sa)
		}
		if err != nil || s.sa.Name == "" || !strings.Contains(s.sa.PGPKey, "BEGIN PGP PRIVATE KEY BLOCK") {
			return nil, provider.Errorf(provider.Unauthenticated, "the %s is not an Omni service account key", SecretServiceAccountKey).WithHint(keyHint)
		}
		s.key = key
	}
	raw := acct.ConfigString(ConfigEndpoint)
	switch {
	case raw != "":
		var err error
		if s.endpoint, err = apiclient.CheckURL(ConfigEndpoint, raw); err != nil {
			return nil, err
		}
	case s.serviceAccount():
		return nil, provider.Errorf(provider.InvalidConfig, "a service account key needs the %s setting", ConfigEndpoint).
			WithHint("import the account again with --set endpoint=<the OMNI_ENDPOINT that omnictl serviceaccount create printed>")
	}
	return s, nil
}

// serviceAccount reports whether the session uses a service account key.
func (s *session) serviceAccount() bool { return s.key != "" }

// signInHint is the command that signs the user in to Omni again.
func (s *session) signInHint() string {
	cmd := "omnictl get clusters"
	if s.context != "" {
		cmd += " --context=" + s.context
	}
	return fmt.Sprintf("run `%s` in a terminal to sign in to Omni", cmd)
}

// env returns the plugin's environment for omnictl, with a browser that
// only prints the sign-in address, and the account's endpoint and service
// account.
func (s *session) env(base []string) []string {
	env := slices.DeleteFunc(slices.Clone(base), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(omnictlEnv, name)
	})
	// omnictl opens a browser to sign in again when its key has expired.
	// BROWSER=echo makes it print the address instead, which the plugin
	// notices and stops at.
	env = append(env, "BROWSER=echo")
	if s.endpoint != "" {
		env = append(env, "OMNI_ENDPOINT="+s.endpoint)
	}
	if s.serviceAccount() {
		env = append(env, "OMNI_SERVICE_ACCOUNT_KEY="+s.key)
	}
	return env
}

// omnictl runs omnictl and returns what it printed on stdout. omnictl gets
// no standard input, and it is stopped as soon as it starts to sign in, so
// that it neither waits for a browser nor opens one. A failure is reported
// with omnictl's own message, and code unless the message says otherwise.
func (s *session) omnictl(ctx context.Context, code provider.Code, args ...string) ([]byte, error) {
	if s.context != "" {
		args = append(args, "--context="+s.context)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "omnictl", args...)
	cmd.Env = s.env(os.Environ())
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	stderr := &stderrWatcher{out: os.Stderr, cancel: cancel}
	cmd.Stderr = stderr
	// Children that keep stderr open must not keep the plugin waiting.
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	what := operation(args)
	switch {
	case err == nil:
		return stdout.Bytes(), nil
	case stderr.signingIn():
		code = provider.Unauthenticated
		return nil, provider.Errorf(code, "omnictl %s: omnictl could not authenticate to Omni", what).WithHint(s.hint(code, ""))
	case errors.Is(err, exec.ErrNotFound):
		return nil, provider.Errorf(provider.Unavailable, "omnictl is not installed or not on PATH").WithHint(InstallHint)
	case ctx.Err() != nil:
		return nil, fmt.Errorf("running omnictl %s: %w", what, ctx.Err())
	}
	msg := stderr.message(err)
	code = errorCode(msg, code)
	return nil, provider.Errorf(code, "omnictl %s: %s", what, msg).WithHint(s.hint(code, msg))
}

// operation names an omnictl command for messages, without the path of a
// downloaded kubeconfig.
func operation(args []string) string {
	if len(args) > 1 && args[0] == "get" {
		return "get " + args[1]
	}
	return args[0]
}

// errorCode returns the code for omnictl's error message: the gRPC status
// it names, or def.
func errorCode(msg string, def provider.Code) provider.Code {
	switch {
	case strings.Contains(msg, "code = Unauthenticated"):
		return provider.Unauthenticated
	case strings.Contains(msg, "code = PermissionDenied"):
		return provider.PermissionDenied
	case strings.Contains(msg, "code = NotFound"), strings.HasPrefix(msg, "cluster not found"):
		return provider.NotFound
	case strings.Contains(msg, "code = ResourceExhausted"):
		return provider.RateLimited
	case strings.Contains(msg, "code = InvalidArgument"),
		strings.HasPrefix(msg, "context not found"),
		strings.Contains(msg, "has not been configured"):
		return provider.InvalidConfig
	default:
		return def
	}
}

// hint returns the next step for an omnictl error.
func (s *session) hint(code provider.Code, msg string) string {
	switch {
	case code == provider.Unauthenticated && s.serviceAccount():
		return "the service account key is wrong, expired or revoked; " + keyHint
	case code == provider.Unauthenticated:
		return s.signInHint()
	case code == provider.PermissionDenied && s.serviceAccount():
		return "the service account's role does not allow this; Kubernetes service account tokens need one that may update the cluster, such as Operator, given with `omnictl serviceaccount create <name> --role=Operator --use-user-role=false`"
	case code == provider.PermissionDenied:
		return "your Omni role does not allow this; ask an Omni admin for a role with access to the clusters"
	case strings.HasPrefix(msg, "context not found"):
		return "run `omnictl config contexts` and set context to one of them"
	case strings.Contains(msg, "has not been configured"):
		return "download omniconfig.yaml from the Omni dashboard and run `omnictl config merge ./omniconfig.yaml`"
	case strings.Contains(msg, "omnictl is too old") || strings.Contains(msg, "omnictl is newer"):
		return "install the omnictl version that matches your Omni instance; " + InstallHint
	default:
		return ""
	}
}

// stderrWatcher passes omnictl's stderr on to the plugin's, which tug logs,
// and stops omnictl when it starts to sign in. It keeps the last error line
// for the message.
type stderrWatcher struct {
	out    io.Writer
	cancel context.CancelFunc

	mu      sync.Mutex
	partial []byte
	signIn  bool
	errLine string
	last    string
}

// Write implements io.Writer. exec calls it from its own goroutine.
func (w *stderrWatcher) Write(b []byte) (int, error) {
	_, _ = w.out.Write(b)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial = append(w.partial, b...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		w.line(string(w.partial[:i]))
		w.partial = w.partial[i+1:]
	}
	return len(b), nil
}

// line looks at one complete line. The caller holds w.mu.
func (w *stderrWatcher) line(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	for _, m := range signInMessages {
		if strings.HasPrefix(line, m) {
			w.signIn = true
			w.cancel()
		}
	}
	// cobra prints the error omnictl fails with as "Error: <message>".
	if msg, ok := strings.CutPrefix(line, "Error: "); ok {
		w.errLine = msg
	}
	w.last = line
}

func (w *stderrWatcher) signingIn() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.signIn
}

// message returns the reason omnictl gave for failing: its error line, or
// else the last line it printed, or else how it exited.
func (w *stderrWatcher) message(err error) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.partial) > 0 {
		w.line(string(w.partial))
		w.partial = nil
	}
	return cmp.Or(w.errLine, w.last, err.Error())
}

// Omni resources as omnictl get -o json prints them, trimmed to the fields
// the plugin uses. omnictl converts each resource from its YAML form, so the
// spec's keys are the Go field names in lower case.
type clusterResource struct {
	Metadata struct {
		ID     string         `json:"id"`
		Phase  string         `json:"phase"`
		Labels map[string]any `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		KubernetesVersion string `json:"kubernetesversion"`
		TalosVersion      string `json:"talosversion"`
	} `json:"spec"`
}

// clusters lists the clusters omnictl can see. omnictl prints one JSON
// object per resource, one after the other, and nothing when there are none.
func (s *session) clusters(ctx context.Context) ([]clusterResource, error) {
	out, err := s.omnictl(ctx, provider.Unavailable, "get", "clusters", "-o", "json")
	if err != nil {
		return nil, err
	}
	var clusters []clusterResource
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var cl clusterResource
		err := dec.Decode(&cl)
		if errors.Is(err, io.EOF) {
			return clusters, nil
		}
		if err != nil {
			return nil, provider.Errorf(provider.Internal, "reading omnictl get clusters: %v", err)
		}
		clusters = append(clusters, cl)
	}
}

// access is what a kubeconfig from Omni says about reaching a cluster.
type access struct {
	server string
	ca     []byte
	// issuer, clientID and scopes come from the kubectl oidc-login
	// arguments of an OIDC kubeconfig.
	issuer   string
	clientID string
	scopes   []string
	// token is the bearer token of a service account kubeconfig.
	token string
}

// kubeconfig downloads the cluster's kubeconfig with omnictl kubeconfig and
// reads it. It goes to a directory of its own, which is removed afterwards,
// rather than into the user's kubeconfig.
func (s *session) kubeconfig(ctx context.Context, cluster string, flags ...string) (*access, error) {
	dir, err := os.MkdirTemp("", "tug-omni-")
	if err != nil {
		return nil, fmt.Errorf("making a directory for the kubeconfig: %w", err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "kubeconfig")
	args := append([]string{"kubeconfig", path, "--cluster=" + cluster, "--merge=false"}, flags...)
	if _, err := s.omnictl(ctx, provider.Unavailable, args...); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, provider.Errorf(provider.Internal, "reading the kubeconfig omnictl wrote: %v", err)
	}
	acc, err := parseKubeconfig(data)
	if err != nil {
		return nil, provider.Errorf(provider.Internal, "the kubeconfig of cluster %s: %v", cluster, err)
	}
	return acc, nil
}

// parseKubeconfig reads the server of the first cluster and the credential
// of the first user. Omni writes one of each.
func parseKubeconfig(data []byte) (*access, error) {
	var kc struct {
		Clusters []struct {
			Cluster struct {
				Server string `json:"server"`
				CAData string `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
		Users []struct {
			User struct {
				Token string `json:"token"`
				Exec  *struct {
					Args []string `json:"args"`
				} `json:"exec"`
			} `json:"user"`
		} `json:"users"`
	}
	if err := yaml.Unmarshal(data, &kc); err != nil {
		return nil, err
	}
	if len(kc.Clusters) == 0 || kc.Clusters[0].Cluster.Server == "" {
		return nil, errors.New("it has no server")
	}
	acc := &access{server: strings.TrimSuffix(kc.Clusters[0].Cluster.Server, "/")}
	if ca := kc.Clusters[0].Cluster.CAData; ca != "" {
		var err error
		if acc.ca, err = base64.StdEncoding.DecodeString(ca); err != nil {
			return nil, fmt.Errorf("its certificate-authority-data is not base64: %w", err)
		}
	}
	if len(kc.Users) == 0 {
		return acc, nil
	}
	u := kc.Users[0].User
	acc.token = u.Token
	if u.Exec != nil {
		acc.readOIDCLogin(u.Exec.Args)
	}
	return acc, nil
}

// readOIDCLogin reads the flags of `kubectl oidc-login get-token`, as
// --flag=value or --flag value.
func (a *access) readOIDCLogin(args []string) {
	for i := 0; i < len(args); i++ {
		name, value, ok := strings.Cut(args[i], "=")
		if !ok && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			value = args[i+1]
			i++
		}
		switch name {
		case "--oidc-issuer-url":
			a.issuer = value
		case "--oidc-client-id":
			a.clientID = value
		case "--oidc-extra-scope":
			for scope := range strings.SplitSeq(value, ",") {
				if scope = strings.TrimSpace(scope); scope != "" {
					a.scopes = append(a.scopes, scope)
				}
			}
		}
	}
}

// oidc returns the credential for a cluster: Omni's issuer and client, with
// the scope that names the cluster in place of any other cluster's.
func (a *access) oidc(cluster string) (provider.Credential, error) {
	if a.issuer == "" || a.clientID == "" {
		return provider.Credential{}, provider.Errorf(provider.Internal, "the kubeconfig omnictl downloaded for %s does not sign in through an OIDC issuer", cluster)
	}
	scopes := slices.DeleteFunc(slices.Clone(a.scopes), func(s string) bool { return strings.HasPrefix(s, scopeClusterPrefix) })
	return provider.OIDC(a.issuer, a.clientID, append(scopes, scopeClusterPrefix+cluster)...), nil
}

// List returns the clusters omnictl can see, all reached through Omni's
// Kubernetes proxy. Clusters being torn down are skipped.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	s, err := newSession(req.Account, req.Secrets)
	if err != nil {
		return nil, err
	}
	resources, err := s.clusters(ctx)
	if err != nil {
		return nil, err
	}
	resources = slices.DeleteFunc(resources, func(cl clusterResource) bool {
		if cl.Metadata.ID == "" {
			return true
		}
		if cl.Metadata.Phase == "tearingDown" {
			fmt.Fprintf(os.Stderr, "omni: skipping %s: it is being torn down\n", cl.Metadata.ID)
			return true
		}
		return false
	})
	out := make([]provider.Cluster, 0, len(resources))
	if len(resources) == 0 {
		return out, nil
	}
	// Omni writes the same proxy address and issuer into every cluster's
	// kubeconfig, so one kubeconfig serves the whole listing. An OIDC
	// kubeconfig holds no secret, and asking for one creates nothing.
	acc, err := s.kubeconfig(ctx, resources[0].Metadata.ID)
	if err != nil {
		return nil, err
	}
	for _, cl := range resources {
		pc := provider.Cluster{
			ID:        cl.Metadata.ID,
			Name:      cl.Metadata.ID,
			Version:   cl.Spec.KubernetesVersion,
			Endpoints: provider.Endpoints{Public: acc.server},
			CAData:    acc.ca,
			Labels:    clusterLabels(cl),
		}
		// The OIDC credential is a sign-in to make, not a secret. Returning
		// it here lets import and sync replace the cached one when the
		// issuer changes. Service account tokens are created only when tug
		// needs one.
		if !s.serviceAccount() {
			cred, err := acc.oidc(pc.ID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "omni: listing %s without a credential: %v\n", pc.ID, err)
			} else {
				pc.Credential = &cred
			}
		}
		out = append(out, pc)
	}
	return out, nil
}

// clusterLabels keeps the cluster's labels with plain keys, such as env.
// Keys with a prefix or dots, such as omni.sidero.dev/..., are Omni's own.
func clusterLabels(cl clusterResource) map[string]string {
	labels := map[string]string{}
	for k, v := range cl.Metadata.Labels {
		value := fmt.Sprint(v)
		if k != "" && v != nil && value != "" && !strings.ContainsAny(k, "./") {
			labels[strings.ToLower(k)] = value
		}
	}
	if cl.Spec.TalosVersion != "" {
		labels["talos-version"] = cl.Spec.TalosVersion
	}
	return labels
}

// Credential returns Omni's OIDC sign-in for the cluster, or, for an
// account with a service account key, a new Kubernetes service account
// token.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	s, err := newSession(req.Account, req.Secrets)
	if err != nil {
		return provider.Credential{}, err
	}
	if s.serviceAccount() {
		return p.serviceAccountToken(ctx, s, req.Account, req.ClusterID)
	}
	acc, err := s.kubeconfig(ctx, req.ClusterID)
	if err != nil {
		return provider.Credential{}, goneHint(err)
	}
	return acc.oidc(req.ClusterID)
}

// goneHint explains a cluster that omnictl cannot find.
func goneHint(err error) error {
	if provider.CodeOf(err) == provider.NotFound {
		return provider.AsError(err).WithHint("the cluster is gone from Omni, or you can no longer see it; run tug sync")
	}
	return err
}

// serviceAccountToken creates a Kubernetes service account token for the
// cluster with omnictl kubeconfig --service-account. Omni signs it as a JWT
// that expires after the account's tokenTTL.
func (p *Provider) serviceAccountToken(ctx context.Context, s *session, acct provider.Account, cluster string) (provider.Credential, error) {
	ttl, err := tokenTTL(acct)
	if err != nil {
		return provider.Credential{}, err
	}
	now := p.now()
	user := cmp.Or(acct.ConfigString(ConfigServiceAccountUser), s.sa.Name)
	flags := []string{"--service-account", "--user=" + user, "--ttl=" + ttl.String()}
	if groups := groupsSetting(acct); groups != "" {
		flags = append(flags, "--groups="+groups)
	}
	acc, err := s.kubeconfig(ctx, cluster, flags...)
	if err != nil {
		return provider.Credential{}, goneHint(err)
	}
	if acc.token == "" {
		return provider.Credential{}, provider.Errorf(provider.Internal, "the service account kubeconfig of cluster %s has no token", cluster)
	}
	expires, ok := jwtExpiry(acc.token)
	if !ok {
		expires = now.Add(ttl)
	}
	return provider.Token(acc.token, expires), nil
}

// tokenTTL reads the tokenTTL setting.
func tokenTTL(acct provider.Account) (time.Duration, error) {
	raw := acct.ConfigString(ConfigTokenTTL)
	if raw == "" {
		return defaultTokenTTL, nil
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil || ttl < time.Minute || ttl > maxTokenTTL {
		return 0, provider.Errorf(provider.InvalidConfig, "%s must be a duration from 1m to %s, such as 8h; got %q", ConfigTokenTTL, maxTokenTTL, raw)
	}
	return ttl, nil
}

// groupsSetting returns the serviceAccountGroups setting in the form
// omnictl's --groups takes, without spaces.
func groupsSetting(acct provider.Account) string {
	var groups []string
	for g := range strings.SplitSeq(acct.ConfigString(ConfigServiceAccountGroups), ",") {
		if g = strings.TrimSpace(g); g != "" {
			groups = append(groups, g)
		}
	}
	return strings.Join(groups, ",")
}

// jwtExpiry reads the exp claim of a JWT without checking its signature,
// which is the API server's job.
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0).UTC(), true
}
