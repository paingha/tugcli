package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/tunnel"
)

func newTunnelCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tunnel",
		Short: "Reach private clusters through an SSH jump host, a proxy, AWS SSM or Google IAP",
		Long: `Give a cluster a tunnel when its API server is not directly reachable. tug
commands then go through the tunnel, and tug connect routes kubectl, helm and
k9s through tugd, which keeps the tunnel open.

Tunnels:
  ssh://[user@]host[:port][?identity=<key file>]
      an SSH jump host; keys come from ssh-agent or ~/.ssh, and the host key
      must already be in ~/.ssh/known_hosts
  socks5://host:port, http://host:port
      a proxy that needs no password
  ssm://<instance id>[?region=<region>&profile=<profile>]
      an AWS Systems Manager session through a managed instance that can
      reach the private endpoint; needs only AWS credentials
  iap://<instance>?project=<project>&zone=<zone>
  iap://?project=<project>&region=<region>&network=<network>&group=<group>
      Google Cloud IAP TCP forwarding to a VM, or to a host in a destination
      group such as a private GKE endpoint; needs only Google credentials`,
	}
	cmd.AddCommand(newTunnelSetCmd(a), newTunnelRmCmd(a), newTunnelLsCmd(a), newTunnelTestCmd(a))
	return cmd
}

func newTunnelSetCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "set <cluster> <tunnel>",
		Short: "Reach a cluster through a tunnel",
		Example: `  tug tunnel set eks-private ssm://i-0123456789abcdef0?region=eu-west-1
  tug tunnel set gke-private 'iap://?project=acme&region=europe-west1&network=default&group=k8s'
  tug tunnel set lab ssh://admin@bastion.example.com`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := tunnel.Check(args[1]); err != nil {
				return err
			}
			cat, cl, err := a.tunnelCluster(args[0])
			if err != nil {
				return err
			}
			cl.Tunnel = args[1]
			if err := cat.Save(a.catalogPath()); err != nil {
				return err
			}
			fmt.Fprintln(a.stderr, a.colors().Dim("tug connect "+cl.Name+" routes every client through tugd, which opens the tunnel; tug tunnel test checks it"))
			_, err = fmt.Fprintf(a.stdout, "%s goes through %s\n", cl.Name, tunnel.Redacted(cl.Tunnel))
			return err
		},
		ValidArgsFunction: a.completeClusters,
	}
}

func newTunnelRmCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <cluster>",
		Aliases: []string{"unset"},
		Short:   "Reach a cluster directly again",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cat, cl, err := a.tunnelCluster(args[0])
			if err != nil {
				return err
			}
			if cl.Tunnel == "" {
				return fmt.Errorf("%s has no tunnel", cl.Name)
			}
			was := cl.Tunnel
			cl.Tunnel = ""
			if err := cat.Save(a.catalogPath()); err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.stdout, "Removed the tunnel %s from %s\n", tunnel.Redacted(was), cl.Name)
			return err
		},
		ValidArgsFunction: a.completeClusters,
	}
}

func newTunnelLsCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List clusters reached through tunnels",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			cat, err := a.loadCatalog()
			if err != nil {
				return err
			}
			type row struct {
				Cluster string `json:"cluster"`
				Tunnel  string `json:"tunnel"`
			}
			var rows []row
			for _, cl := range cat.Clusters {
				if cl.Tunnel != "" {
					rows = append(rows, row{Cluster: cl.Name, Tunnel: tunnel.Redacted(cl.Tunnel)})
				}
			}
			switch format {
			case render.FormatJSON, render.FormatYAML:
				return render.Structured(a.stdout, format, rows)
			case render.FormatName:
				for _, r := range rows {
					fmt.Fprintln(a.stdout, r.Cluster)
				}
				return nil
			}
			table := make([][]string, 0, len(rows))
			for _, r := range rows {
				table = append(table, []string{r.Cluster, r.Tunnel})
			}
			return render.Table(a.stdout, []string{"CLUSTER", "TUNNEL"}, table)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json, yaml or name")
	return cmd
}

func newTunnelTestCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "test [cluster]",
		Short: "Open a cluster's tunnel and reach its API server through it",
		Long: `Open the tunnel and a TCP connection to the API server through it, and say
how long that took. Without a cluster, test this terminal's cluster.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var cl *catalog.Cluster
			if len(args) == 1 {
				_, c, err := a.tunnelCluster(args[0])
				if err != nil {
					return err
				}
				cl = c
			} else {
				s, err := a.sessions().Current()
				if err != nil || !s.State.Connected() {
					return errors.New("name a cluster, or run `tug connect` first")
				}
				cat, err := a.loadCatalog()
				if err != nil {
					return err
				}
				if cl, err = cat.Get(s.State.ClusterID); err != nil {
					return err
				}
			}
			if cl.Tunnel == "" {
				return fmt.Errorf("%s has no tunnel; tug tunnel set gives it one", cl.Name)
			}
			addr, err := serverAddress(cl.Server)
			if err != nil {
				return err
			}
			return a.testTunnel(cmd.Context(), cl, addr)
		},
		ValidArgsFunction: a.completeClusters,
	}
}

func (a *app) testTunnel(ctx context.Context, cl *catalog.Cluster, addr string) error {
	fmt.Fprintf(a.stderr, "Opening %s...\n", tunnel.Redacted(cl.Tunnel))
	start := a.now()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	conn, err := a.tunnels().Dial(ctx, cl.Tunnel, addr)
	if err != nil {
		return fmt.Errorf("reaching %s through %s: %w; run with --debug to see why the tunnel refused", addr, tunnel.Redacted(cl.Tunnel), err)
	}
	conn.Close()
	_, err = fmt.Fprintf(a.stdout, "Reached %s through %s in %s\n", addr, tunnel.Redacted(cl.Tunnel), a.now().Sub(start).Round(time.Millisecond))
	return err
}

// tunnelCluster loads the catalog and finds a cluster by ID or name.
func (a *app) tunnelCluster(name string) (*catalog.Catalog, *catalog.Cluster, error) {
	cat, err := a.loadCatalog()
	if err != nil {
		return nil, nil, err
	}
	cl, err := cat.Get(name)
	if err != nil {
		if cl, err = a.chooseCluster(cat, []string{name}); err != nil {
			return nil, nil, err
		}
	}
	// chooseCluster may return a copy; edit the catalog's own entry.
	i := slices.IndexFunc(cat.Clusters, func(c catalog.Cluster) bool { return c.ID == cl.ID })
	return cat, &cat.Clusters[i], nil
}

// serverAddress turns an API server URL into host:port.
func serverAddress(server string) (string, error) {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("the API server address %q is not a URL", server)
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}
