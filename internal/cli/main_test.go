package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/paingha/tugcli/internal/passthrough"
)

// testRoleEnv, when set, makes the test binary stand in for kubectl or for
// tug, so tests run real processes without either being installed. Session
// kubeconfigs written during tests run the test binary as `tug auth token`,
// since it is the running executable.
const testRoleEnv = "TUG_CLI_TEST_ROLE"

func TestMain(m *testing.M) {
	switch os.Getenv(testRoleEnv) {
	case "kubectl":
		// The credential plugin this kubectl runs is tug.
		if err := os.Setenv(testRoleEnv, "tug"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(fakeKubectl(os.Args[1:], os.Stdout, os.Stderr))
	case "tug":
		os.Exit(Main())
	}
	os.Exit(m.Run())
}

// useFakeKubectl makes tug run the test binary as kubectl.
func useFakeKubectl(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TUG_KUBECTL", exe)
	t.Setenv(testRoleEnv, "kubectl")
}

// fakeKubectl is just enough of kubectl for tests. `get pods` lists pods
// with the kubeconfig in $KUBECONFIG, through client-go as kubectl does, so
// the exec credential plugin and proxy-url are honoured; with -w it then
// waits to be interrupted. Anything else prints its arguments and succeeds.
func fakeKubectl(args []string, stdout, stderr io.Writer) int {
	words := passthrough.Words(args)
	if len(words) < 2 || words[0] != "get" || !slices.Contains([]string{"pods", "pod", "po"}, words[1]) {
		fmt.Fprintf(stdout, "fake kubectl %s\n", strings.Join(args, " "))
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	overrides := &clientcmd.ConfigOverrides{}
	for i, a := range args {
		if (a == "-n" || a == "--namespace") && i+1 < len(args) {
			overrides.Context.Namespace = args[i+1]
		}
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), overrides)
	cfg, err := cc.ClientConfig()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	ns, _, err := cc.Namespace()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "NAME")
	for _, p := range pods.Items {
		fmt.Fprintln(stdout, p.Name)
	}
	if slices.Contains(args, "-w") || slices.Contains(args, "--watch") {
		<-ctx.Done()
		return 130
	}
	return 0
}
