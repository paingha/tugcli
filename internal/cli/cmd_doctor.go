package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/doctor"
	"github.com/paingha/tugcli/internal/guard"
	"github.com/paingha/tugcli/internal/kube"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/session"
	"github.com/paingha/tugcli/internal/version"
	"github.com/paingha/tugcli/pkg/provider"
)

// accessChecker is implemented by providers that can explain whether the
// caller may reach a cluster, such as plugins with the checkAccess operation.
type accessChecker interface {
	CheckAccess(ctx context.Context, req provider.AccessRequest) ([]provider.Finding, error)
}

type doctorOptions struct {
	output   string
	egressIP string
}

func newDoctorCmd(a *app) *cobra.Command {
	var o doctorOptions
	cmd := &cobra.Command{
		Use:   "doctor [cluster]",
		Short: "Explain why a cluster cannot be reached, and how to fix it",
		Long: `Check tug's setup (shell hook, credential store, kubectl and its credential
plugin policy, the catalog) and one cluster: DNS, TCP, TLS, credentials, the
API server, clock and version skew, who you are signed in as, what you may do
in the namespace, and the provider's access list.

Without a cluster argument, doctor checks this terminal's cluster. It only
reads; it never changes the cluster. It exits 1 when a check fails.`,
		Example: `  tug doctor
  tug doctor prod-eu
  tug doctor lke-prod --egress-ip 198.51.100.9   # also check the provider's access list for this address
  tug doctor -o json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runDoctor(cmd.Context(), args, o)
		},
		ValidArgsFunction: a.completeClusters,
	}
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "output format: table, json or yaml")
	cmd.Flags().StringVar(&o.egressIP, "egress-ip", "", "your public IP address, for providers that check access lists")
	return cmd
}

func (a *app) runDoctor(ctx context.Context, args []string, o doctorOptions) error {
	format, err := render.ParseFormat(o.output)
	if err != nil {
		return err
	}
	if format == render.FormatName {
		return errors.New("doctor supports -o table, json or yaml")
	}
	cat, err := a.loadCatalog()
	if err != nil {
		return err
	}
	cfg := doctor.Config{Now: a.now, Catalog: cat, Kuberc: a.kubercInfo()}

	s, sessErr := a.sessions().Current()
	cfg.Session = doctor.SessionInfo{ID: os.Getenv(session.EnvSession), Env: os.Getenv("KUBECONFIG"), Shell: shellName()}
	switch {
	case sessErr == nil && s.State.Connected():
		cfg.Session.Cluster, cfg.Session.ReadOnly, cfg.Session.Kubeconfig = s.State.ClusterName, s.State.ReadOnly, s.KubeconfigPath()
	case sessErr != nil && !errors.Is(sessErr, session.ErrNoSession):
		cfg.Session.Err = sessErr
	}

	store, storeErr := a.credentialStore()
	if storeErr != nil {
		cfg.Store.Err = storeErr
	} else {
		cfg.Store.Name = store.Name()
	}
	if path, err := exec.LookPath(a.cfg.Kubectl); err == nil {
		cfg.Kubectl = doctor.KubectlInfo{Path: path, Version: func(ctx context.Context) (string, error) {
			return doctor.KubectlVersion(ctx, path)
		}}
	}

	var cl *catalog.Cluster
	switch {
	case len(args) == 1:
		if cl, err = a.chooseCluster(cat, args); err != nil {
			return err
		}
	case sessErr == nil && s.State.Connected():
		if cl, err = cat.Get(s.State.ClusterID); err != nil {
			return fmt.Errorf("this terminal's cluster: %w; run `tug connect` again", err)
		}
	}
	if cl != nil {
		ns := cl.DefaultNamespace
		if sessErr == nil && s.State.ClusterID == cl.ID {
			ns = s.State.Namespace
		}
		a.doctorCluster(&cfg, cat, cl, ns, o.egressIP, storeErr)
	}

	report := doctor.Run(ctx, cfg)
	if format == render.FormatJSON || format == render.FormatYAML {
		if err := render.Structured(a.stdout, format, report); err != nil {
			return err
		}
	} else if err := a.printDoctor(report); err != nil {
		return err
	}
	if report.Failed() {
		return &exitError{code: 1}
	}
	return nil
}

// doctorCluster fills in the cluster part of the doctor's config.
func (a *app) doctorCluster(cfg *doctor.Config, cat *catalog.Catalog, cl *catalog.Cluster, ns, egressIP string, storeErr error) {
	cfg.Cluster, cfg.Namespace = cl, ns
	if storeErr != nil {
		cfg.Credential = func(context.Context) (*credentials.Resolved, error) { return nil, storeErr }
		return
	}
	svc, broker, err := a.services(cat)
	if err != nil {
		cfg.Credential = func(context.Context) (*credentials.Resolved, error) { return nil, err }
		return
	}
	cfg.Credential = func(ctx context.Context) (*credentials.Resolved, error) { return broker.Resolve(ctx, cl.ID, false) }
	f := &kube.Factory{
		Cluster:   cl,
		Namespace: ns,
		Broker:    broker,
		// doctor only reads, whatever the session's mode.
		Guard:     &guard.Policy{Cluster: cl.Name, ReadOnly: true, Now: a.now},
		CacheDir:  a.paths.Cache,
		UserAgent: "tug/" + version.String(),
	}
	cfg.RESTConfig = f.RESTConfig

	p, err := a.registry().Get(cl.Provider)
	if err != nil {
		return
	}
	ac, ok := p.(accessChecker)
	if !ok || !p.Info().Capabilities.Has(provider.CapCheckAccess) {
		return
	}
	cfg.Access = func(ctx context.Context) ([]provider.Finding, error) {
		src, ok := cat.Source(cl.Provider, cl.Account)
		if !ok {
			return nil, fmt.Errorf("%s has no source in the catalog", cl.Name)
		}
		secrets, err := svc.Secrets(p.Info(), cl.Account)
		if err != nil {
			return nil, err
		}
		return ac.CheckAccess(ctx, provider.AccessRequest{
			Account:  src.ProviderAccount(),
			Cluster:  providerCluster(cl),
			EgressIP: egressIP,
			Secrets:  secrets,
		})
	}
}

// providerCluster describes a catalog cluster to its provider.
func providerCluster(cl *catalog.Cluster) provider.Cluster {
	return provider.Cluster{
		ID:        cl.ProviderID,
		Name:      cl.Name,
		Region:    cl.Region,
		Version:   cl.Version,
		Endpoints: provider.Endpoints{Public: cl.Server, Private: cl.PrivateServer},
		CAData:    cl.CAData,
	}
}

// kubercInfo locates kubectl's kuberc. Its path is kubectl's convention, not
// one of tug's directories: $KUBERC, else ~/.kube/kuberc.
func (a *app) kubercInfo() doctor.KubercInfo {
	info := doctor.KubercInfo{TugCommand: selfPath()}
	path := os.Getenv("KUBERC")
	if strings.EqualFold(path, "off") || os.Getenv("KUBECTL_KUBERC") == "false" {
		info.Disabled = true
		return info
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return info
		}
		path = filepath.Join(home, ".kube", "kuberc")
	} else {
		info.Explicit = true
	}
	info.Path = path
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !info.Explicit:
	case err != nil:
		info.ReadErr = err
	default:
		info.Data = data
	}
	return info
}

// shellName guesses the user's shell for the hook instructions.
func shellName() string {
	if sh := filepath.Base(os.Getenv("SHELL")); sh == "bash" || sh == "fish" || sh == "zsh" {
		return sh
	}
	if os.Getenv("PSModulePath") != "" {
		return "powershell"
	}
	return "zsh"
}

func (a *app) printDoctor(r doctor.Report) error {
	c := a.colors()
	status := func(s doctor.Status) string {
		switch s {
		case doctor.OK:
			return c.Green("ok")
		case doctor.Warn:
			return c.Yellow("warn")
		case doctor.Fail:
			return c.Red("FAIL")
		default:
			return c.Dim(string(s))
		}
	}
	section := func(title, group string) error {
		var rows [][]string
		for _, x := range r.Results {
			if x.Group != group {
				continue
			}
			rows = append(rows, []string{status(x.Status), x.Name, x.Detail})
			if x.Fix != "" && (x.Status == doctor.Warn || x.Status == doctor.Fail) {
				rows = append(rows, []string{"", "", c.Dim("fix: " + x.Fix)})
			}
		}
		if len(rows) == 0 {
			return nil
		}
		fmt.Fprintln(a.stdout, c.Bold(title))
		if err := render.Table(a.stdout, []string{"RESULT", "CHECK", "DETAIL"}, rows); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout)
		return nil
	}
	if err := section("tug setup", doctor.GroupSetup); err != nil {
		return err
	}
	if r.Cluster != "" {
		if err := section("cluster "+r.Cluster, doctor.GroupCluster); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(a.stdout, "%d ok, %d warnings, %d failed\n", r.Count(doctor.OK), r.Count(doctor.Warn), r.Count(doctor.Fail))
	return err
}
