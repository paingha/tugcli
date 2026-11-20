package nebius

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/pkg/conformance"
	"github.com/paingha/tugcli/pkg/provider"
)

// fakeScript is a stand-in for the nebius CLI. It logs its arguments, checks
// the flags the plugin always passes, and answers from the files beside it:
// clusters[-<page token>].json for `mk8s cluster list` and
// nodegroups-<cluster ID>.json for `mk8s node-group list`. A file named exit
// makes it fail with that code and the contents of stderr; one named sleep
// makes it hang.
const fakeScript = `#!/bin/sh
dir=$(dirname "$0")
echo "$*" >> "$dir/calls"
cmd="$1 $2 $3"
shift 3
parent= token= format= browser=
while [ $# -gt 0 ]; do
	case "$1" in
	--parent-id) parent=$2; shift ;;
	--page-token) token=$2; shift ;;
	--format) format=$2; shift ;;
	--no-browser) browser=no ;;
	esac
	shift
done
if [ "$format" != json ] || [ "$browser" != no ]; then
	echo "fake nebius: want --format json --no-browser" >&2
	exit 2
fi
if [ -f "$dir/sleep" ]; then
	sleep 5
fi
if [ -f "$dir/exit" ]; then
	cat "$dir/stderr" >&2
	exit "$(cat "$dir/exit")"
fi
case "$cmd" in
"mk8s cluster list") f="$dir/clusters${token:+-$token}.json" ;;
"mk8s node-group list") f="$dir/nodegroups-$parent.json" ;;
*) echo "fake nebius: unknown command $cmd" >&2; exit 2 ;;
esac
if [ ! -f "$f" ]; then
	echo "rpc error: code = NotFound desc = resource not found" >&2
	exit 13
fi
cat "$f"
`

const testCA = "-----BEGIN CERTIFICATE-----\nMIIBdzCCAR2gAwIBAgIBADAKBggqhkjOPQQDAjAjMSEwHwYDVQQDDBhrM3Mtc2Vy\n-----END CERTIFICATE-----\n"

// The listings follow the mk8s v1 ClusterService and NodeGroupService
// messages as the CLI prints them: snake_case fields, enums by name, empty
// fields left out.
var listing = map[string]string{
	"clusters.json": `{"items":[
	  {"metadata":{"id":"mk8scluster-e00train","name":"train","parent_id":"project-e00test","labels":{"env":"prod","team":"ml"}},
	   "spec":{"control_plane":{"version":"1.31","subnet_id":"vpcsubnet-e00a"}},
	   "status":{"state":"RUNNING","control_plane":{"version":"1.31.2-nebius-cp.3",
	     "endpoints":{"public_endpoint":"pu.mk8scluster-e00train.mk8s.eu-north1.nebius.cloud","private_endpoint":"pr.mk8scluster-e00train.mk8s.eu-north1.nebius.cloud"},
	     "etcd_cluster_size":"3","auth":{"cluster_ca_certificate":"` + strings.ReplaceAll(testCA, "\n", `\n`) + `"}}}},
	  {"metadata":{"id":"mk8scluster-e00new","name":"creating","parent_id":"project-e00test"},
	   "spec":{"control_plane":{"version":"1.32"}},
	   "status":{"state":"PROVISIONING","reconciling":true}}
	],"next_page_token":"page-2"}`,
	"clusters-page-2.json": `{"items":[
	  {"metadata":{"id":"mk8scluster-e00infer","name":"infer","parent_id":"project-e00test"},
	   "spec":{"control_plane":{"version":"1.30"}},
	   "status":{"state":"RUNNING","control_plane":{"endpoints":{"private_endpoint":"10.0.1.7"}}}}
	]}`,
	"nodegroups-mk8scluster-e00train.json": `{"items":[
	  {"metadata":{"id":"mk8snodegroup-e00cpu","name":"system","parent_id":"mk8scluster-e00train"},
	   "spec":{"fixed_node_count":"2","template":{"resources":{"platform":"cpu-d3","preset":"4vcpu-16gb"}}}},
	  {"metadata":{"id":"mk8snodegroup-e00gpu","name":"h100","parent_id":"mk8scluster-e00train"},
	   "spec":{"fixed_node_count":"4","template":{"resources":{"platform":"gpu-h100-sxm","preset":"8gpu-128vcpu-1600gb"}}}}
	]}`,
	"nodegroups-mk8scluster-e00infer.json": `{"items":[
	  {"metadata":{"id":"mk8snodegroup-e00e2","name":"cpu","parent_id":"mk8scluster-e00infer"},
	   "spec":{"template":{"resources":{"platform":"cpu-e2","preset":"2vcpu-8gb"}}}}
	]}`,
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// fakeCLI puts a fake nebius CLI that answers with files on PATH, ahead of
// any real one, and returns its directory.
func fakeCLI(t *testing.T, files map[string]string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake nebius CLI is a shell script")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, CLI), fakeScript, 0o755)
	for name, body := range files {
		writeFile(t, filepath.Join(dir, name), body, 0o600)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	return dir
}

func calls(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func account(cfg map[string]any) provider.Account {
	return provider.Account{ID: "work", Config: cfg}
}

func TestList(t *testing.T) {
	dir := fakeCLI(t, listing)
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: account(map[string]any{ConfigProject: "project-e00test"})})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 {
		t.Fatalf("List = %d clusters, want 2 (the one being created is skipped): %+v", len(clusters), clusters)
	}
	train, infer := clusters[0], clusters[1]
	if train.ID != "mk8scluster-e00train" || train.Name != "train" || train.Version != "1.31.2" ||
		train.Endpoints.Public != "https://pu.mk8scluster-e00train.mk8s.eu-north1.nebius.cloud" ||
		train.Endpoints.Private != "https://pr.mk8scluster-e00train.mk8s.eu-north1.nebius.cloud" ||
		string(train.CAData) != testCA {
		t.Errorf("train = %+v", train)
	}
	if train.Labels["gpu"] != "true" || train.Labels["env"] != "prod" || train.Labels["team"] != "ml" {
		t.Errorf("train labels = %v, want gpu, env and team", train.Labels)
	}
	if infer.Endpoints.Public != "" || infer.Endpoints.Private != "https://10.0.1.7" || infer.Version != "1.30" || infer.CAData != nil {
		t.Errorf("infer = %+v", infer)
	}
	if _, ok := infer.Labels["gpu"]; ok {
		t.Errorf("infer labels = %v, want no gpu label", infer.Labels)
	}
	for _, c := range clusters {
		if c.Credential == nil || c.Credential.Type != provider.CredentialDelegate {
			t.Errorf("%s: inline credential = %+v, want the get-token delegate", c.Name, c.Credential)
		}
	}

	got := calls(t, dir)
	want := []string{
		"mk8s cluster list --parent-id project-e00test --page-size 100 --format json --no-browser",
		"mk8s cluster list --parent-id project-e00test --page-size 100 --page-token page-2 --format json --no-browser",
		"mk8s node-group list --parent-id mk8scluster-e00infer --page-size 100 --format json --no-browser",
		"mk8s node-group list --parent-id mk8scluster-e00train --page-size 100 --format json --no-browser",
	}
	// Node groups are listed in parallel, so their order varies.
	slices.Sort(got[2:])
	if !slices.Equal(got, want) {
		t.Errorf("CLI calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestListUsesTheProfile(t *testing.T) {
	dir := fakeCLI(t, listing)
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: account(map[string]any{ConfigProfile: "ci"})})
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range calls(t, dir) {
		if !strings.HasSuffix(call, "--profile ci") || strings.Contains(call, "--parent-id project") {
			t.Errorf("CLI call %q, want --profile ci and the profile's parent-id", call)
		}
	}
	if args := clusters[0].Credential.Exec.Args; !slices.Contains(args, "ci") {
		t.Errorf("credential args = %v, want --profile ci", args)
	}
}

func TestCredential(t *testing.T) {
	dir := fakeCLI(t, nil)
	bin := filepath.Join(dir, CLI)
	tests := []struct {
		name    string
		profile string
		args    []string
	}{
		{"active profile", "", []string{"mk8s", "v1", "cluster", "get-token", "--format", "json"}},
		{"named profile", "ci", []string{"mk8s", "v1", "cluster", "get-token", "--profile", "ci", "--format", "json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New().Credential(context.Background(), provider.CredentialRequest{
				Account: account(map[string]any{ConfigProfile: tt.profile}), ClusterID: "mk8scluster-e00train",
			})
			if err != nil {
				t.Fatal(err)
			}
			e := c.Exec
			if c.Type != provider.CredentialDelegate || e == nil || e.Command != bin || !slices.Equal(e.Args, tt.args) ||
				e.APIVersion != "client.authentication.k8s.io/v1beta1" || e.InteractiveMode != "IfAvailable" || e.InstallHint == "" {
				t.Fatalf("Credential = %+v, exec %+v; want %s %v", c, e, bin, tt.args)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(dir, "calls")); err == nil {
		t.Error("Credential ran the CLI; it should leave that to kubectl")
	}
}

func TestCLIErrors(t *testing.T) {
	tests := []struct {
		exit   string
		stderr string
		want   provider.Code
		hint   string
	}{
		{"7", "authentication error: token expired", provider.Unauthenticated, "nebius iam whoami"},
		{"2", "unknown flag: --page-size", provider.InvalidConfig, "parent-id"},
		{"3", "rpc error: code = InvalidArgument desc = parent_id is required", provider.InvalidConfig, "parent-id"},
		{"4", "profile \"ci\" not found", provider.InvalidConfig, "nebius profile"},
		{"13", "rpc error: code = NotFound desc = project not found", provider.NotFound, "project"},
		{"15", "rpc error: code = PermissionDenied desc = permission denied", provider.PermissionDenied, "viewer"},
		{"55", "rpc error: code = PermissionDenied", provider.PermissionDenied, "viewer"},
		{"23", "too many requests", provider.RateLimited, "wait"},
		{"20", "rpc error: code = Unavailable", provider.Unavailable, ""},
		{"5", "internal error", provider.Internal, ""},
	}
	for _, tt := range tests {
		t.Run("exit "+tt.exit, func(t *testing.T) {
			fakeCLI(t, map[string]string{"exit": tt.exit, "stderr": "\n" + tt.stderr + "\nmore detail\n"})
			_, err := New().List(context.Background(), provider.ListRequest{Account: account(nil)})
			pe := provider.AsError(err)
			if pe.Code != tt.want || !strings.Contains(pe.Message, tt.stderr) || !strings.Contains(pe.Hint, tt.hint) {
				t.Errorf("List = %v (hint %q), want %s with %q and a hint about %q", err, pe.Hint, tt.want, tt.stderr, tt.hint)
			}
			if strings.Contains(pe.Message, "more detail") {
				t.Errorf("message %q has more than the first line of stderr", pe.Message)
			}
		})
	}
}

func TestListFailures(t *testing.T) {
	t.Run("output is not JSON", func(t *testing.T) {
		fakeCLI(t, map[string]string{"clusters.json": "train  RUNNING\n"})
		_, err := New().List(context.Background(), provider.ListRequest{Account: account(nil)})
		if provider.CodeOf(err) != provider.Internal {
			t.Errorf("List = %v, want Internal", err)
		}
	})
	t.Run("node groups fail", func(t *testing.T) {
		files := map[string]string{"clusters.json": listing["clusters-page-2.json"]}
		fakeCLI(t, files)
		_, err := New().List(context.Background(), provider.ListRequest{Account: account(nil)})
		if provider.CodeOf(err) != provider.NotFound {
			t.Errorf("List = %v, want the node group listing's NotFound", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		fakeCLI(t, map[string]string{"sleep": ""})
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, err := New().List(ctx, provider.ListRequest{Account: account(map[string]any{ConfigProfile: "ci"})})
		if pe := provider.AsError(err); pe.Code != provider.Unavailable || !strings.Contains(pe.Hint, "nebius iam whoami --profile ci") {
			t.Errorf("List = %v (hint %q), want Unavailable with a sign-in hint", err, pe.Hint)
		}
	})
}

func TestCLINotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	_, err := New().List(context.Background(), provider.ListRequest{Account: account(nil)})
	if pe := provider.AsError(err); pe.Code != provider.Unavailable || !strings.Contains(pe.Hint, "docs.nebius.com/cli/install") {
		t.Errorf("List = %v (hint %q), want Unavailable with an install hint", err, pe.Hint)
	}
	_, err = New().Credential(context.Background(), provider.CredentialRequest{Account: account(nil), ClusterID: "mk8scluster-e00train"})
	if provider.CodeOf(err) != provider.Unavailable {
		t.Errorf("Credential = %v, want Unavailable", err)
	}
}

func TestCLIInInstallDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake nebius CLI is a shell script")
	}
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".nebius", "bin", CLI), fakeScript, 0o755)
	for name, body := range listing {
		writeFile(t, filepath.Join(home, ".nebius", "bin", name), body, 0o600)
	}
	// PATH has the tools the script uses, and no nebius.
	t.Setenv("PATH", strings.Join([]string{"/usr/bin", "/bin"}, string(os.PathListSeparator)))
	t.Setenv("HOME", home)
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: account(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".nebius", "bin", CLI); clusters[0].Credential.Exec.Command != want {
		t.Errorf("credential command = %q, want %q", clusters[0].Credential.Exec.Command, want)
	}
}

func TestKubernetesVersion(t *testing.T) {
	tests := []struct{ actual, desired, want string }{
		{"1.30.0-nebius-cp.3", "1.30", "1.30.0"},
		{"1.31.2", "1.31", "1.31.2"},
		{"", "1.32", "1.32"},
	}
	for _, tt := range tests {
		if got := kubernetesVersion(tt.actual, tt.desired); got != tt.want {
			t.Errorf("kubernetesVersion(%q, %q) = %q, want %q", tt.actual, tt.desired, got, tt.want)
		}
	}
}

func TestEndpointURL(t *testing.T) {
	for in, want := range map[string]string{
		"pu.example.nebius.cloud":         "https://pu.example.nebius.cloud",
		"10.0.1.7":                        "https://10.0.1.7",
		"https://pu.example.nebius.cloud": "https://pu.example.nebius.cloud",
		"":                                "",
	} {
		if got := endpointURL(in); got != want {
			t.Errorf("endpointURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestManifestMatchesInfo(t *testing.T) {
	b, err := os.ReadFile("plugin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Metadata struct{ Name, Version string }
		Spec     struct {
			Capabilities provider.Capability
			Secrets      []provider.SecretSpec
		}
	}
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	info := New().Info()
	if m.Metadata.Name != info.Name || m.Metadata.Version != info.Version ||
		m.Spec.Capabilities != provider.Capabilities(New()) || !slices.Equal(m.Spec.Secrets, info.Secrets) {
		t.Errorf("plugin.yaml = %+v, want Info() = %+v", m, info)
	}
}

func TestConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the plugin")
	}
	fakeCLI(t, listing)
	bin := filepath.Join(t.TempDir(), "tug-provider-nebius")
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/tug-provider-nebius").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	r := conformance.Run(context.Background(), conformance.Target{Command: bin}, conformance.Options{
		Name:            Name,
		Account:         account(map[string]any{ConfigProject: "project-e00test"}),
		DeclaredSecrets: New().Info().Secrets,
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("the nebius plugin does not conform")
	}
}
