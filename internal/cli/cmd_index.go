package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/plugin"
	"github.com/paingha/tugcli/internal/render"
)

// IndexKeyEnv supplies an index signing key to tug plugin index sign, for
// CI, instead of the credential store.
const IndexKeyEnv = "TUG_INDEX_KEY"

// errNoIndex is returned when a plugin is named but no index is set up.
var errNoIndex = errors.New("no plugin index is set up; add one under plugins.indexes in config.yaml (name, url, publicKey), or install from a file or URL")

func (a *app) indexSources() ([]plugin.IndexSource, error) {
	srcs := make([]plugin.IndexSource, 0, len(a.cfg.Plugins.Indexes))
	for _, ix := range a.cfg.Plugins.Indexes {
		key, err := plugin.ParsePublicKey(ix.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("plugins.indexes %s: %w", ix.Name, err)
		}
		if ix.Name == "" || ix.URL == "" {
			return nil, errors.New("every entry in plugins.indexes needs a name, a url and a publicKey")
		}
		srcs = append(srcs, plugin.IndexSource{Name: ix.Name, URL: ix.URL, PublicKey: key})
	}
	return srcs, nil
}

func (a *app) indexCache() *plugin.IndexCache {
	return &plugin.IndexCache{Dir: filepath.Join(a.paths.Cache, "plugin-index"), Now: a.now}
}

// resolveIndexed finds name[@version] in the configured indexes and
// returns the archive's URL and sha256.
func (a *app) resolveIndexed(ctx context.Context, ref string) (plugin.Match, plugin.IndexVersion, error) {
	srcs, err := a.indexSources()
	if err != nil {
		return plugin.Match{}, plugin.IndexVersion{}, err
	}
	if len(srcs) == 0 {
		return plugin.Match{}, plugin.IndexVersion{}, errNoIndex
	}
	name, version, _ := strings.Cut(ref, "@")
	matches, errs := a.indexCache().Search(ctx, srcs, name)
	for _, m := range matches {
		if m.Plugin.Name != name {
			continue
		}
		v, ok := m.Plugin.Latest()
		if version != "" {
			v, ok = m.Plugin.Version(version)
		}
		if !ok {
			return plugin.Match{}, plugin.IndexVersion{}, fmt.Errorf("index %s has no version %q of %s", m.Index, version, name)
		}
		return m, v, nil
	}
	if err := errors.Join(errs...); err != nil {
		return plugin.Match{}, plugin.IndexVersion{}, fmt.Errorf("no plugin %s in the indexes that answered: %w", name, err)
	}
	return plugin.Match{}, plugin.IndexVersion{}, fmt.Errorf("no plugin %s in the plugin indexes; tug plugin search lists them", name)
}

func newPluginSearchCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "search [query]",
		Short: "Search the signed plugin indexes",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			srcs, err := a.indexSources()
			if err != nil {
				return err
			}
			if len(srcs) == 0 {
				return errNoIndex
			}
			matches, errs := a.indexCache().Search(cmd.Context(), srcs, argOr(args, ""))
			for _, err := range errs {
				fmt.Fprintf(a.stderr, "%s %v\n", a.colors().Yellow("warning:"), err)
			}
			type row struct {
				Name        string `json:"name"`
				Version     string `json:"version"`
				Publisher   string `json:"publisher,omitempty"`
				Description string `json:"description,omitempty"`
				Index       string `json:"index"`
			}
			rows := make([]row, 0, len(matches))
			for _, m := range matches {
				v, _ := m.Plugin.Latest()
				rows = append(rows, row{m.Plugin.Name, v.Version, m.Plugin.Publisher, m.Plugin.Description, m.Index})
			}
			switch format {
			case render.FormatJSON, render.FormatYAML:
				return render.Structured(a.stdout, format, rows)
			case render.FormatName:
				for _, r := range rows {
					fmt.Fprintln(a.stdout, r.Name)
				}
				return nil
			}
			table := make([][]string, 0, len(rows))
			for _, r := range rows {
				table = append(table, []string{r.Name, r.Version, dash(r.Publisher), dash(r.Description), r.Index})
			}
			return render.Table(a.stdout, []string{"NAME", "VERSION", "PUBLISHER", "DESCRIPTION", "INDEX"}, table)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json, yaml or name")
	return cmd
}

func newPluginIndexCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Build and sign a plugin index, for those who publish one",
		Long: `A plugin index lists reviewed plugins with the URL and sha256 of each archive,
and is signed with ed25519 as a whole. Users add it to plugins.indexes in
config.yaml with its public key; tug plugin install <name> then checks the
signature, the archive's sha256 and every binary's sha256.

  tug plugin index keygen                       # once; prints the public key
  tug plugin index add index.yaml dist/x.tar.gz --url https://.../x.tar.gz
  tug plugin index sign index.yaml              # writes index.yaml.sig
  # publish index.yaml and index.yaml.sig side by side`,
	}
	cmd.AddCommand(newIndexKeygenCmd(a), newIndexAddCmd(a), newIndexSignCmd(a), newIndexVerifyCmd(a))
	return cmd
}

func indexKeyName(name string) string { return "index-key/" + name }

func newIndexKeygenCmd(a *app) *cobra.Command {
	var name string
	var force bool
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Make an index signing key, kept in the credential store",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := a.credentialStore()
			if err != nil {
				return err
			}
			if _, err := store.Get(indexKeyName(name)); err == nil && !force {
				return fmt.Errorf("an index key named %s exists; pass --force to replace it, which makes indexes signed with it unverifiable", name)
			}
			priv, pub, err := plugin.NewIndexKey()
			if err != nil {
				return err
			}
			if err := store.Set(indexKeyName(name), priv); err != nil {
				return err
			}
			fmt.Fprintf(a.stderr, "The private key is in %s as %s. For CI, export it once with your store's tools and set %s.\n",
				store.Name(), indexKeyName(name), IndexKeyEnv)
			_, err = fmt.Fprintln(a.stdout, pub)
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "default", "name of the key")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing key")
	return cmd
}

func newIndexAddCmd(a *app) *cobra.Command {
	var archiveURL, description string
	cmd := &cobra.Command{
		Use:   "add <index.yaml> <archive>",
		Short: "Record a packaged plugin archive in an index file",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if archiveURL == "" {
				return errors.New("--url is required: where the archive is published")
			}
			p, err := plugin.AddToIndex(args[0], args[1], archiveURL, description)
			if err != nil {
				return err
			}
			v, _ := p.Latest()
			fmt.Fprintln(a.stderr, a.colors().Dim("sign the index again before publishing it: tug plugin index sign "+args[0]))
			_, err = fmt.Fprintf(a.stdout, "Added %s %s to %s\n", p.Name, v.Version, args[0])
			return err
		},
	}
	cmd.Flags().StringVar(&archiveURL, "url", "", "https URL the archive is published at")
	cmd.Flags().StringVar(&description, "description", "", "what the plugin reaches, shown by tug plugin search")
	return cmd
}

func newIndexSignCmd(a *app) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "sign <index.yaml>",
		Short: "Sign an index file, writing <index.yaml>.sig",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := os.Getenv(IndexKeyEnv)
			if key == "" {
				store, err := a.credentialStore()
				if err != nil {
					return err
				}
				if key, err = store.Get(indexKeyName(name)); errors.Is(err, credentials.ErrNotFound) {
					return fmt.Errorf("no index key named %s; run tug plugin index keygen, or set %s", name, IndexKeyEnv)
				} else if err != nil {
					return err
				}
			}
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			sig, err := plugin.SignIndex(data, key)
			if err != nil {
				return err
			}
			out := args[0] + plugin.SignatureSuffix
			if err := os.WriteFile(out, sig, 0o644); err != nil { //nolint:gosec // a signature is published
				return err
			}
			_, err = fmt.Fprintf(a.stdout, "Wrote %s\n", out)
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "default", "name of the key")
	return cmd
}

func newIndexVerifyCmd(a *app) *cobra.Command {
	var publicKey string
	cmd := &cobra.Command{
		Use:   "verify <index.yaml> --public-key <key>",
		Short: "Check an index file against its signature and public key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := plugin.ParsePublicKey(publicKey)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			sig, err := os.ReadFile(args[0] + plugin.SignatureSuffix)
			if err != nil {
				return err
			}
			ix, err := plugin.ParseIndex(data, sig, key)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.stdout, "%s is signed by that key and lists %d plugins\n", args[0], len(ix.Plugins))
			return err
		},
	}
	cmd.Flags().StringVar(&publicKey, "public-key", "", "the base64 public key from tug plugin index keygen")
	_ = cmd.MarkFlagRequired("public-key")
	return cmd
}
