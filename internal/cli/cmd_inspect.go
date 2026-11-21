package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/plugin"
	"github.com/paingha/tugcli/internal/registry"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/sandbox"
	"github.com/paingha/tugcli/internal/sigstore"
	"github.com/paingha/tugcli/internal/version"
)

type inspectOptions struct {
	output string
	sha256 string
}

func newPluginInspectCmd(a *app) *cobra.Command {
	var o inspectOptions
	cmd := &cobra.Command{
		Use:   "inspect <archive|url|name[@version]>",
		Short: "Explain what a plugin may access, without installing it",
		Long: `Explain what a plugin may reach and how tug would run it, without installing
or running it: the secrets it would receive, the environment variables,
network hosts, files, sockets and programs it may use, whether it would run
in the sandbox, its CPU, memory and time limits, who signed it, whether an
index has revoked it, and whether it is pinned.

Give an archive file or URL, an installed or linked plugin's name, or
name@version from a plugin index, which tug downloads and checks as
tug plugin install would. An archive whose sha256 an index lists is checked
against that entry, signature included.`,
		Example: `  tug plugin inspect ./dist/tug-provider-acme_0.1.0.tar.gz
  tug plugin inspect acme@0.2.0
  tug plugin inspect acme -o json   # an installed plugin, for a script`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.inspectPlugin(cmd.Context(), args[0], o)
		},
	}
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "output format: table, json or yaml")
	cmd.Flags().StringVar(&o.sha256, "sha256", "", "expected sha256 of an archive from a URL")
	return cmd
}

// inspection is what tug plugin inspect reports. Its JSON and YAML are a
// stable interface.
type inspection struct {
	Name          string           `json:"name"`
	Version       string           `json:"version"`
	Publisher     string           `json:"publisher,omitempty"`
	Homepage      string           `json:"homepage,omitempty"`
	MinTugVersion string           `json:"minTugVersion,omitempty"`
	TugTooOld     bool             `json:"tugTooOld,omitempty"`
	Source        inspectSource    `json:"source"`
	Signature     inspectSignature `json:"signature"`
	Revoked       *inspectRevoked  `json:"revoked,omitempty"`
	Pin           *inspectPin      `json:"pin,omitempty"`
	Capabilities  []string         `json:"capabilities"`
	Secrets       []inspectSecret  `json:"secrets"`
	Environment   []string         `json:"environment"`
	Network       []string         `json:"network"`
	Files         inspectFiles     `json:"files"`
	Programs      []inspectProgram `json:"programs"`
	Runs          inspectRuns      `json:"runs"`
	Limits        inspectLimits    `json:"limits"`
	Platforms     []string         `json:"platforms"`
	Settings      []string         `json:"settings,omitempty"`
	UnknownFields []string         `json:"unknownFields,omitempty"`
}

type inspectSource struct {
	// Kind is archive, installed, linked or index.
	Kind     string `json:"kind"`
	Location string `json:"location"`
	SHA256   string `json:"sha256,omitempty"`
	Index    string `json:"index,omitempty"`
}

type inspectSignature struct {
	// Status is verified (checked now), recorded (checked when it was
	// installed), invalid, unsigned or unknown (installed before tug kept
	// a record).
	Status string `json:"status"`
	Signer string `json:"signer,omitempty"`
	Issuer string `json:"issuer,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type inspectRevoked struct {
	Index  string `json:"index"`
	Reason string `json:"reason,omitempty"`
}

type inspectPin struct {
	Version string `json:"version"`
	// Matches is false when the plugin is pinned to another version, which
	// install then refuses.
	Matches bool `json:"matches"`
}

type inspectSecret struct {
	Name     string `json:"name"`
	Optional bool   `json:"optional,omitempty"`
}

type inspectFiles struct {
	Read    []string `json:"read"`
	Write   []string `json:"write"`
	Sockets []string `json:"sockets"`
}

type inspectProgram struct {
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
}

type inspectRuns struct {
	// As is native, wasm or none (no build for this machine).
	As    string `json:"as"`
	Build string `json:"build,omitempty"`
	// Sandboxed is true when the operating system's sandbox, or tug's
	// WebAssembly runtime, confines it.
	Sandboxed bool     `json:"sandboxed"`
	Ports     []uint16 `json:"ports,omitempty"`
	Detail    string   `json:"detail"`
}

type inspectLimits struct {
	CPUSeconds  int               `json:"cpuSeconds,omitempty"`
	MemoryMiB   uint64            `json:"memoryMiB,omitempty"`
	Timeouts    map[string]string `json:"timeouts"`
	Capped      []string          `json:"capped,omitempty"`
	NotEnforced string            `json:"notEnforced,omitempty"`
}

// inspected is a plugin found for inspect, and what tug knows of where it
// came from.
type inspected struct {
	p         *plugin.Plugin
	runErr    error
	source    inspectSource
	signature inspectSignature
	revoked   *inspectRevoked
	cleanup   func()
}

func (a *app) inspectPlugin(ctx context.Context, ref string, o inspectOptions) error {
	format, err := render.ParseFormat(o.output)
	if err != nil {
		return err
	}
	loaded, errs, err := a.loadIndexes(ctx)
	if err != nil {
		return err
	}
	for _, err := range errs {
		fmt.Fprintf(a.stderr, "%s %v\n", a.colors().Yellow("warning:"), err)
	}
	var in *inspected
	switch u, err := url.Parse(ref); {
	case err == nil && (u.Scheme == "https" || u.Scheme == "http"):
		tmp, err := os.MkdirTemp("", "tug-plugin-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		archive, err := plugin.Fetch(ctx, ref, tmp)
		if err != nil {
			return err
		}
		in, err = a.inspectArchive(ctx, archive, ref, o.sha256, loaded)
		if err != nil {
			return err
		}
	case fileExists(ref):
		if in, err = a.inspectArchive(ctx, ref, ref, o.sha256, loaded); err != nil {
			return err
		}
	default:
		if in, err = a.inspectNamed(ctx, ref, loaded, errs); err != nil {
			return err
		}
	}
	defer in.cleanup()
	report := a.inspection(in)
	switch format {
	case render.FormatJSON, render.FormatYAML:
		return render.Structured(a.stdout, format, report)
	case render.FormatName:
		_, err := fmt.Fprintln(a.stdout, report.Name)
		return err
	}
	a.printInspection(report)
	return nil
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// inspectArchive unpacks an archive and finds what the indexes say of it:
// its entry, by sha256, and through that its signature, and whether it is
// revoked.
func (a *app) inspectArchive(ctx context.Context, archive, from, wantSHA256 string, loaded []plugin.Loaded) (*inspected, error) {
	u, err := a.pluginDirs().Unpack(archive)
	if err != nil {
		return nil, err
	}
	in := &inspected{p: u.Plugin, runErr: u.RunErr, cleanup: func() { _ = u.Close() }}
	if wantSHA256 != "" && !strings.EqualFold(wantSHA256, u.SHA256) {
		in.cleanup()
		return nil, fmt.Errorf("checksum mismatch: %s is %s, expected %s", from, u.SHA256, wantSHA256)
	}
	in.source = inspectSource{Kind: "archive", Location: from, SHA256: u.SHA256}
	in.signature = inspectSignature{Status: "unsigned", Detail: "no index lists this archive, so nothing names who must have signed it"}
	for _, l := range loaded {
		for _, p := range l.Index.Plugins {
			for _, v := range p.Versions {
				if strings.EqualFold(v.SHA256, u.SHA256) {
					in.source.Index = l.Source.Name
					in.signature = a.checkInspectedSignature(ctx, archive, l.Source.Name, p.Name, v)
				}
			}
		}
	}
	if r, by, ok := plugin.FindRevoked(loaded, u.SHA256); ok {
		in.revoked = &inspectRevoked{Index: by, Reason: r.Reason}
	}
	return in, nil
}

// checkInspectedSignature checks an archive's Sigstore signature against
// its index entry, as install would.
func (a *app) checkInspectedSignature(ctx context.Context, archive, index, name string, v plugin.IndexVersion) inspectSignature {
	if v.Sigstore == nil {
		return inspectSignature{Status: "unsigned", Detail: "index " + index + " names no Sigstore signature for it"}
	}
	s, err := a.checkSignature(ctx, archive, installOptions{sig: v.Sigstore, wantName: name, wantVersion: v.Version})
	if err != nil {
		return inspectSignature{Status: "invalid", Detail: err.Error()}
	}
	return inspectSignature{Status: "verified", Signer: s.Subject, Issuer: s.Issuer}
}

// inspectNamed finds an installed or linked plugin by name, or a plugin in
// the indexes by name@version or when none is installed.
func (a *app) inspectNamed(ctx context.Context, ref string, loaded []plugin.Loaded, errs []error) (*inspected, error) {
	name, version, _ := strings.Cut(ref, "@")
	if e, ok := a.registry().Entry(name); ok && version == "" {
		pp, ok := e.Provider.(*plugin.Provider)
		if !ok {
			return nil, fmt.Errorf("%s is built into tug, and runs inside it with tug's own access", name)
		}
		return a.inspectInstalled(pp.Plugin(), e.Kind, loaded), nil
	}
	if len(a.cfg.Plugins.Indexes) == 0 {
		if strings.ContainsAny(ref, `/\`) || strings.HasSuffix(ref, ".gz") || strings.HasSuffix(ref, ".zip") {
			return nil, fmt.Errorf("%s does not exist", ref)
		}
		return nil, fmt.Errorf("no plugin %s is installed or linked, and %w", name, errNoIndex)
	}
	m, v, err := lookupIndexed(loaded, errs, ref)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "tug-plugin-")
	if err != nil {
		return nil, err
	}
	archive, err := plugin.Fetch(ctx, v.URL, tmp)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return nil, err
	}
	in, err := a.inspectArchive(ctx, archive, v.URL, v.SHA256, loaded)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return nil, err
	}
	if in.p.Name() != m.Plugin.Name {
		in.cleanup()
		_ = os.RemoveAll(tmp)
		return nil, fmt.Errorf("the index lists %s, but the archive holds %s", m.Plugin.Name, in.p.Name())
	}
	done := in.cleanup
	in.cleanup = func() { done(); _ = os.RemoveAll(tmp) }
	in.source = inspectSource{Kind: "index", Location: v.URL, SHA256: in.source.SHA256, Index: m.Index}
	return in, nil
}

// inspectInstalled describes an installed or linked plugin from what was
// recorded when it was installed.
func (a *app) inspectInstalled(p *plugin.Plugin, kind registry.Kind, loaded []plugin.Loaded) *inspected {
	in := &inspected{p: p, cleanup: func() {}}
	if kind == registry.KindDevPlugin || p.Dev {
		in.source = inspectSource{Kind: "linked", Location: p.Dir}
		in.signature = inspectSignature{Status: "unsigned", Detail: "a plugin under development is neither checksummed nor signed"}
		return in
	}
	dirs := a.pluginDirs()
	in.source = inspectSource{Kind: "installed", Location: p.Dir}
	rc, ok := dirs.Receipt(p)
	switch {
	case !ok:
		in.signature = inspectSignature{Status: "unknown", Detail: "installed before tug recorded where plugins came from"}
	case rc.Signer != "":
		in.source.SHA256, in.source.Index = rc.SHA256, rc.Index
		in.signature = inspectSignature{Status: "recorded", Signer: rc.Signer, Detail: "checked when it was installed from " + dash(rc.Source)}
	default:
		in.source.SHA256, in.source.Index = rc.SHA256, rc.Index
		in.signature = inspectSignature{Status: "unsigned", Detail: "installed from " + dash(rc.Source) + " without a Sigstore signature"}
	}
	if r, by, revoked := revokedInstall(dirs, loaded, p); revoked {
		in.revoked = &inspectRevoked{Index: by, Reason: r.Reason}
	}
	return in
}

// inspection assembles the report for a plugin.
func (a *app) inspection(in *inspected) inspection {
	p, m := in.p, in.p.Manifest
	access := p.Access(a.pluginCaps(), a.cfg.Plugins.Sandbox.Confines(p.Name()))
	r := inspection{
		Name:          m.Metadata.Name,
		Version:       m.Metadata.Version,
		Publisher:     m.Metadata.Publisher,
		Homepage:      m.Metadata.Homepage,
		MinTugVersion: m.Spec.MinTugVersion,
		TugTooOld:     m.CheckTugVersion(version.String()) != nil,
		Source:        in.source,
		Signature:     in.signature,
		Revoked:       in.revoked,
		Capabilities:  nonNilStrings(m.Spec.Capabilities.Names()),
		Secrets:       []inspectSecret{},
		Environment:   nonNilStrings(access.Env),
		Network:       nonNilStrings(access.Network),
		Files:         inspectFiles{Read: nonNilStrings(access.Read), Write: nonNilStrings(access.Write), Sockets: nonNilStrings(access.Sockets)},
		Programs:      []inspectProgram{},
		UnknownFields: m.Unknown,
	}
	for _, s := range access.Secrets {
		r.Secrets = append(r.Secrets, inspectSecret{Name: s.Name, Optional: s.Optional})
	}
	for _, t := range access.Tools {
		r.Programs = append(r.Programs, inspectProgram{Name: t.Name, Path: t.Path})
	}
	for _, b := range m.Spec.Platforms {
		r.Platforms = append(r.Platforms, b.OS+"/"+b.Arch)
	}
	for _, s := range m.Settings() {
		r.Settings = append(r.Settings, describeSetting(s))
	}
	if pin, ok := a.cfg.Plugins.PluginPin(p.Name()); ok {
		r.Pin = &inspectPin{Version: pin, Matches: strings.TrimPrefix(pin, "v") == strings.TrimPrefix(m.Metadata.Version, "v")}
	}

	switch {
	case in.runErr != nil:
		r.Runs = inspectRuns{As: "none", Detail: "cannot run here: " + in.runErr.Error()}
	case access.WASM:
		r.Runs = inspectRuns{As: "wasm", Build: access.Build.OS + "/" + access.Build.Arch, Sandboxed: true,
			Detail: "WebAssembly in tug's sandbox: https to the network hosts only, no files and no programs"}
	default:
		r.Runs = inspectRuns{As: "native", Build: access.Build.OS + "/" + access.Build.Arch, Sandboxed: access.Confined, Detail: a.describeSandbox(m)}
		if access.Confined {
			r.Runs.Ports = access.Ports
		}
	}

	r.Limits = limitsReport(access.Budget, access.WASM, access.Limits)
	return r
}

// limitsReport describes a plugin's budget: for a WebAssembly build, its
// memory and timeouts, and for a native one, what each process may use as
// well, and what of that this machine does not enforce.
func limitsReport(b plugin.Budget, wasm bool, support sandbox.LimitSupport) inspectLimits {
	l := inspectLimits{Timeouts: map[string]string{}, Capped: b.Capped}
	for op, d := range b.Timeouts {
		l.Timeouts[op] = d.String()
	}
	if wasm {
		l.MemoryMiB = b.WASMMemory >> 20
		return l
	}
	l.CPUSeconds = int(b.Process.CPU / time.Second)
	l.MemoryMiB = b.Process.Memory >> 20
	if !b.Process.IsZero() {
		l.NotEnforced = support.Missing
	}
	return l
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// printInspection writes the report for people.
func (a *app) printInspection(r inspection) {
	c := a.colors()
	w := a.stdout
	line := func(label, value string) { fmt.Fprintf(w, "  %-15s%s\n", label+":", value) }
	fmt.Fprintf(w, "%s %s\n", c.Bold(r.Name), r.Version)
	line("publisher", dash(r.Publisher))
	line("homepage", dash(r.Homepage))

	src := r.Source.Location
	switch r.Source.Kind {
	case "installed":
		src = "installed in " + src
	case "linked":
		src = "linked from " + src + " " + c.Yellow("(dev: not checksummed)")
	case "index":
		src = "index " + r.Source.Index + ", " + src
	}
	if r.Source.Kind != "index" && r.Source.Index != "" {
		src += ", listed in index " + r.Source.Index
	}
	line("source", src)
	if r.Source.SHA256 != "" {
		line("sha256", r.Source.SHA256)
	}
	switch r.Signature.Status {
	case "verified":
		line("signed by", sigstore.Identity{Subject: r.Signature.Signer, Issuer: r.Signature.Issuer}.String()+" (checked now)")
	case "recorded":
		line("signed by", r.Signature.Signer+" ("+r.Signature.Detail+")")
	case "invalid":
		line("signature", c.Red("INVALID")+": "+r.Signature.Detail)
	default:
		line("signature", c.Yellow("none")+": "+r.Signature.Detail)
	}
	if r.Revoked != nil {
		line("revoked", c.Red("REVOKED")+" by index "+r.Revoked.Index+": "+dash(r.Revoked.Reason))
	} else {
		line("revoked", "no")
	}
	switch {
	case r.Pin == nil:
		line("pinned", "-")
	case r.Pin.Matches:
		line("pinned", r.Pin.Version+" in config.yaml")
	default:
		line("pinned", c.Yellow(r.Pin.Version)+" in config.yaml, so tug plugin install refuses "+r.Version)
	}
	line("capabilities", strings.Join(r.Capabilities, ", "))

	secrets := make([]string, 0, len(r.Secrets))
	for _, s := range r.Secrets {
		if s.Optional {
			secrets = append(secrets, s.Name+" (optional)")
		} else {
			secrets = append(secrets, s.Name)
		}
	}
	line("secrets", dash(strings.Join(secrets, ", ")))
	line("environment", dash(strings.Join(r.Environment, ", ")))
	line("network hosts", dash(strings.Join(r.Network, ", ")))
	line("reads", dash(strings.Join(r.Files.Read, ", ")))
	line("writes", dash(strings.Join(r.Files.Write, ", ")))
	line("sockets", dash(strings.Join(r.Files.Sockets, ", ")))
	programs := make([]string, 0, len(r.Programs))
	for _, p := range r.Programs {
		if p.Path == "" {
			programs = append(programs, p.Name+" (not found here)")
		} else if p.Path != p.Name {
			programs = append(programs, p.Name+" ("+p.Path+")")
		} else {
			programs = append(programs, p.Name)
		}
	}
	line("programs", dash(strings.Join(programs, ", ")))
	runs := r.Runs.Detail
	if r.Runs.Build != "" {
		runs = r.Runs.Build + ": " + runs
	}
	line("runs as", runs)
	line("limits", describeLimits(r.Limits))
	line("platforms", strings.Join(r.Platforms, ", "))
	if r.MinTugVersion != "" {
		need := "v" + strings.TrimPrefix(r.MinTugVersion, "v") + " or later"
		if r.TugTooOld {
			need += ", " + c.Red("newer than this tug")
		}
		line("needs tug", need)
	}
	for i, s := range r.Settings {
		label := ""
		if i == 0 {
			label = "settings"
		}
		if label == "" {
			fmt.Fprintf(w, "  %-15s%s\n", "", s)
		} else {
			line(label, s)
		}
	}
	if len(r.UnknownFields) > 0 {
		fmt.Fprintf(a.stderr, "%s %s's plugin.yaml has fields this tug does not know, which it ignores: %s\n",
			c.Yellow("warning:"), r.Name, strings.Join(r.UnknownFields, ", "))
	}
}

// describeLimits says what a plugin's limits are.
func describeLimits(l inspectLimits) string {
	var each []string
	if l.CPUSeconds > 0 {
		each = append(each, strconv.Itoa(l.CPUSeconds)+"s of CPU time")
	}
	if l.MemoryMiB > 0 {
		each = append(each, strconv.FormatUint(l.MemoryMiB, 10)+" MiB of memory")
	}
	var parts []string
	if len(each) > 0 {
		parts = append(parts, strings.Join(each, " and ")+" per process")
	}
	var timeouts []string
	for _, op := range []string{"list", "credential", "rotate", "accounts", "checkAccess", "describe"} {
		if d, ok := l.Timeouts[op]; ok {
			timeouts = append(timeouts, op+" "+d)
		}
	}
	parts = append(parts, "calls time out after "+strings.Join(timeouts, ", "))
	out := strings.Join(parts, "; ")
	if len(l.Capped) > 0 {
		out += "; it asks for more: " + strings.Join(l.Capped, "; ")
	}
	if l.NotEnforced != "" {
		out += "; not enforced here: " + l.NotEnforced
	}
	return out
}
