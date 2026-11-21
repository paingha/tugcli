package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/plugin"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/sigstore"
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
	return &plugin.IndexCache{
		Dir:      filepath.Join(a.paths.Cache, "plugin-index"),
		Now:      a.now,
		KeysFile: filepath.Join(a.paths.Data, "plugin-index-keys.yaml"),
	}
}

// loadIndexes loads the configured indexes, and warns about each one that
// is now trusted under a key other than the one in config.yaml. Indexes
// that fail to load are in errs; the error is for config.yaml itself.
func (a *app) loadIndexes(ctx context.Context) (loaded []plugin.Loaded, errs []error, err error) {
	srcs, err := a.indexSources()
	if err != nil || len(srcs) == 0 {
		return nil, nil, err
	}
	loaded, errs = a.indexCache().LoadAll(ctx, srcs)
	for _, l := range loaded {
		if key, changed := l.KeyChanged(); changed {
			fmt.Fprintf(a.stderr, "%s index %s has handed over from the key in config.yaml to %s, which tug now trusts for it; set its publicKey in plugins.indexes to that key\n",
				a.colors().Yellow("warning:"), l.Source.Name, key)
		}
	}
	return loaded, errs, nil
}

// resolveIndexed finds name[@version] in the loaded indexes and returns
// the archive's URL and sha256. Without a version it is the highest one no
// index has revoked; a revoked version is refused.
func resolveIndexed(loaded []plugin.Loaded, errs []error, ref string) (plugin.Match, plugin.IndexVersion, error) {
	name, version, _ := strings.Cut(ref, "@")
	for _, m := range plugin.Search(loaded, name) {
		if m.Plugin.Name != name {
			continue
		}
		v, ok := m.Plugin.LatestFunc(func(v plugin.IndexVersion) bool {
			_, _, revoked := plugin.FindRevoked(loaded, v.SHA256)
			return !revoked
		})
		if version != "" {
			v, ok = m.Plugin.Version(version)
		}
		switch {
		case !ok && version == "":
			return plugin.Match{}, plugin.IndexVersion{}, fmt.Errorf("index %s has revoked every version of %s", m.Index, name)
		case !ok:
			return plugin.Match{}, plugin.IndexVersion{}, fmt.Errorf("index %s has no version %q of %s", m.Index, version, name)
		}
		if r, by, revoked := plugin.FindRevoked(loaded, v.SHA256); revoked {
			return plugin.Match{}, plugin.IndexVersion{}, fmt.Errorf("index %s has revoked %s %s: %s; not installed", by, name, v.Version, dash(r.Reason))
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
			loaded, errs, err := a.loadIndexes(cmd.Context())
			if err != nil {
				return err
			}
			if len(loaded) == 0 && len(errs) == 0 {
				return errNoIndex
			}
			matches := plugin.Search(loaded, argOr(args, ""))
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
				v, ok := m.Plugin.LatestFunc(func(v plugin.IndexVersion) bool {
					_, _, revoked := plugin.FindRevoked(loaded, v.SHA256)
					return !revoked
				})
				if !ok {
					continue // nothing left to install
				}
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
and is signed with ed25519 as a whole. An archive may also have a Sigstore
signature from its publisher's release workflow. Users add the index to
plugins.indexes in config.yaml with its public key; tug plugin install <name>
then checks the index's signature, the archive's Sigstore signature when the
index names one, the archive's sha256 and every binary's sha256.

  tug plugin index keygen                       # once; prints the public key
  tug plugin index add index.yaml dist/x.tar.gz --url https://.../x.tar.gz \
    --bundle https://.../x.tar.gz.sigstore.json --identity <workflow> --issuer <issuer>
  tug plugin index sign index.yaml              # writes index.yaml.sig
  # publish index.yaml and index.yaml.sig side by side`,
	}
	cmd.AddCommand(newIndexKeygenCmd(a), newIndexAddCmd(a), newIndexRevokeCmd(a), newIndexRotateCmd(a), newIndexSignCmd(a), newIndexVerifyCmd(a))
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

type indexAddOptions struct {
	url         string
	description string
	// The archive's Sigstore bundle and who must have signed it.
	bundle         string
	identity       string
	identityRegexp string
	issuer         string
}

// sigstore returns the signature to record, or nil when no Sigstore flag
// is set.
func (o indexAddOptions) sigstore() (*plugin.IndexSigstore, error) {
	if o.bundle == "" && o.identity == "" && o.identityRegexp == "" && o.issuer == "" {
		return nil, nil
	}
	switch {
	case o.bundle == "":
		return nil, errors.New("--identity, --identity-regexp and --issuer describe a Sigstore signature; pass the bundle's URL with --bundle")
	case o.identity == "" && o.identityRegexp == "":
		return nil, errors.New("--bundle needs the signer's identity: --identity, or --identity-regexp")
	case o.issuer == "":
		return nil, fmt.Errorf("--bundle needs the signer's OIDC issuer: --issuer (for GitHub Actions, %s)", sigstore.GitHubIssuer)
	}
	s := &plugin.IndexSigstore{Bundle: o.bundle, Identity: o.identity, IdentityRegexp: o.identityRegexp, Issuer: o.issuer}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}

func newIndexAddCmd(a *app) *cobra.Command {
	var o indexAddOptions
	cmd := &cobra.Command{
		Use:   "add <index.yaml> <archive>",
		Short: "Record a packaged plugin archive in an index file",
		Long: `Record a packaged plugin archive in an index file, with its URL and sha256.

When the archive is signed with Sigstore, as the release workflow of
tug plugin new signs it, --bundle gives the URL of its bundle and
--identity and --issuer who must have signed it. tug plugin install then
checks the signature before the sha256. The release workflow prints these
flags for each release.`,
		Example: `  tug plugin index add index.yaml dist/tug-provider-acme_0.1.0.tar.gz \
    --url https://github.com/acme/tug-provider-acme/releases/download/v0.1.0/tug-provider-acme_0.1.0.tar.gz \
    --bundle https://github.com/acme/tug-provider-acme/releases/download/v0.1.0/tug-provider-acme_0.1.0.tar.gz.sigstore.json \
    --identity https://github.com/acme/tug-provider-acme/.github/workflows/release.yml@refs/tags/v0.1.0 \
    --issuer https://token.actions.githubusercontent.com`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if o.url == "" {
				return errors.New("--url is required: where the archive is published")
			}
			sig, err := o.sigstore()
			if err != nil {
				return err
			}
			p, v, err := plugin.AddToIndex(args[0], args[1], plugin.IndexAddition{URL: o.url, Description: o.description, Sigstore: sig})
			if err != nil {
				return err
			}
			fmt.Fprintln(a.stderr, a.colors().Dim("sign the index again before publishing it: tug plugin index sign "+args[0]))
			if sig != nil {
				_, err = fmt.Fprintf(a.stdout, "Added %s %s to %s, signed by %s\n", p.Name, v.Version, args[0], sig.Signer())
				return err
			}
			_, err = fmt.Fprintf(a.stdout, "Added %s %s to %s\n", p.Name, v.Version, args[0])
			return err
		},
	}
	cmd.Flags().StringVar(&o.url, "url", "", "https URL the archive is published at")
	cmd.Flags().StringVar(&o.description, "description", "", "what the plugin reaches, shown by tug plugin search")
	cmd.Flags().StringVar(&o.bundle, "bundle", "", "https URL of the archive's Sigstore bundle")
	cmd.Flags().StringVar(&o.identity, "identity", "", "certificate identity that must have signed the archive, such as a release workflow's URL and ref")
	cmd.Flags().StringVar(&o.identityRegexp, "identity-regexp", "", "regular expression the whole certificate identity must match, instead of --identity")
	cmd.Flags().StringVar(&o.issuer, "issuer", "", "OIDC issuer of the signer's identity")
	cmd.MarkFlagsMutuallyExclusive("identity", "identity-regexp")
	return cmd
}

// indexKey returns the private index key with this name from the
// credential store, or from TUG_INDEX_KEY when fromEnv is set and it is.
func (a *app) indexKey(name string, fromEnv bool) (string, error) {
	if key := os.Getenv(IndexKeyEnv); fromEnv && key != "" {
		return key, nil
	}
	store, err := a.credentialStore()
	if err != nil {
		return "", err
	}
	key, err := store.Get(indexKeyName(name))
	if errors.Is(err, credentials.ErrNotFound) {
		return "", fmt.Errorf("no index key named %s; run tug plugin index keygen --name %s, or set %s", name, name, IndexKeyEnv)
	}
	return key, err
}

type indexRevokeOptions struct {
	reason  string
	plugin  string
	version string
}

func newIndexRevokeCmd(a *app) *cobra.Command {
	var o indexRevokeOptions
	cmd := &cobra.Command{
		Use:   "revoke <index.yaml> <archive|sha256>",
		Short: "Withdraw a plugin archive from an index",
		Long: `Record in an index that an archive must not be installed, such as a release
found to be malicious or to leak secrets. tug plugin install then refuses
the archive, by name or from a file, and tug plugin ls warns everyone who
has it installed. The version stays listed, so people can see why it went.

Give the archive itself or its sha256. The plugin and version come from
the index's entry for it, or from the archive; --plugin and --version name
them for an archive the index never listed.`,
		Example: `  tug plugin index revoke index.yaml dist/tug-provider-acme_0.3.0.tar.gz --reason "sends tokens to a third party"
  tug plugin index revoke index.yaml 9f2c... --reason "built from a compromised workflow"
  tug plugin index sign index.yaml`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r := plugin.Revocation{SHA256: args[1], Plugin: o.plugin, Version: o.version, Reason: o.reason}
			if _, err := os.Stat(args[1]); err == nil {
				u, err := a.pluginDirs().Unpack(args[1])
				if err != nil {
					return err
				}
				_ = u.Close() // only the manifest and sha256 were needed
				r.SHA256 = u.SHA256
				if r.Plugin == "" {
					r.Plugin, r.Version = u.Plugin.Name(), u.Plugin.Manifest.Metadata.Version
				}
			}
			r, err := plugin.RevokeInIndex(args[0], r)
			if err != nil {
				return err
			}
			fmt.Fprintln(a.stderr, a.colors().Dim("sign the index again before publishing it: tug plugin index sign "+args[0]))
			_, err = fmt.Fprintf(a.stdout, "Revoked %s in %s\n", r, args[0])
			return err
		},
	}
	cmd.Flags().StringVar(&o.reason, "reason", "", "why, in a sentence for the people who installed it")
	cmd.Flags().StringVar(&o.plugin, "plugin", "", "plugin the archive holds (default: from the index or the archive)")
	cmd.Flags().StringVar(&o.version, "version", "", "version the archive holds (default: from the index or the archive)")
	return cmd
}

func newIndexRotateCmd(a *app) *cobra.Command {
	var from, to string
	cmd := &cobra.Command{
		Use:   "rotate <index.yaml> --to <key name>",
		Short: "Hand an index over from its signing key to a new one",
		Long: `Record in an index that its signing key has been replaced. The old key signs
the new one, and the index lists the old key under supersededKeys. tug
then checks the index against the new key for everyone whose config.yaml
still names the old one, tells them to update it, and from then on refuses
an index signed with the old key.

Make the new key with tug plugin index keygen --name <new> first, then sign
the index with the new key. The old key comes from the credential store, or
from TUG_INDEX_KEY when that is set. Keep the old key's entry in the index:
it is how users who have not updated config.yaml reach the new key.`,
		Example: `  tug plugin index keygen --name 2027
  tug plugin index rotate index.yaml --to 2027
  tug plugin index sign index.yaml --name 2027`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			oldKey, err := a.indexKey(from, true)
			if err != nil {
				return err
			}
			newKey, err := a.indexKey(to, false)
			if err != nil {
				return err
			}
			oldPub, newPub, err := plugin.RotateIndexKey(args[0], oldKey, newKey)
			if err != nil {
				return err
			}
			fmt.Fprintln(a.stderr, a.colors().Dim("sign the index with the new key before publishing it: tug plugin index sign "+args[0]+" --name "+to))
			_, err = fmt.Fprintf(a.stdout, "%s hands over from %s to %s\n", args[0], oldPub, newPub)
			return err
		},
	}
	cmd.Flags().StringVar(&from, "from", "default", "name of the key being replaced")
	cmd.Flags().StringVar(&to, "to", "", "name of the new key, from tug plugin index keygen --name")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func newIndexSignCmd(a *app) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "sign <index.yaml>",
		Short: "Sign an index file, writing <index.yaml>.sig",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := a.indexKey(name, true)
			if err != nil {
				return err
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
			by := "that key"
			if !ix.SignedBy().Equal(key) {
				by = base64.StdEncoding.EncodeToString(ix.SignedBy()) + ", which that key handed over to,"
			}
			_, err = fmt.Fprintf(a.stdout, "%s is signed by %s and lists %d plugins and %d revoked archives\n", args[0], by, len(ix.Plugins), len(ix.Revoked))
			return err
		},
	}
	cmd.Flags().StringVar(&publicKey, "public-key", "", "the base64 public key from tug plugin index keygen")
	_ = cmd.MarkFlagRequired("public-key")
	return cmd
}
