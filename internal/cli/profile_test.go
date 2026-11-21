package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/paingha/tugcli/internal/daemon"
	"github.com/paingha/tugcli/internal/session"
)

// tugIn runs tug with stdin, which is not a terminal.
func tugIn(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), args, strings.NewReader(stdin), &out, &errb)
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

func mustTugIn(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	r := tugIn(t, stdin, args...)
	if r.code != 0 {
		t.Fatalf("tug %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return r.stdout
}

func status(t *testing.T) statusView {
	t.Helper()
	var st statusView
	if err := json.Unmarshal([]byte(mustTug(t, "status", "-o", "json")), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

const paymentsProfile = `# reviewed in the payments repo
cluster: payments-prod
namespace: payments
aliases:
  where: status -o json
  st: status
  bounce: restart api
`

func TestProfiles(t *testing.T) {
	home := isolate(t)
	srv, writes := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)

	if r := tug(t, "profile", "ls"); r.code != 0 || !strings.Contains(r.stderr, "No profiles") {
		t.Fatalf("profile ls with none: %+v", r)
	}
	// Without a terminal, edit reads the profile from stdin.
	if r := tug(t, "profile", "edit", "payments"); r.code == 0 || !strings.Contains(r.stderr, "on stdin") {
		t.Fatalf("profile edit with nothing on stdin: %+v", r)
	}
	path := filepath.Join(home, "config", "profiles", "payments.yaml")
	if out := mustTugIn(t, paymentsProfile, "profile", "edit", "payments"); !strings.Contains(out, "Saved "+path) {
		t.Fatalf("profile edit: %s", out)
	}
	if b, _ := os.ReadFile(path); string(b) != paymentsProfile {
		t.Fatalf("saved profile:\n%s", b)
	}
	// A profile cannot loosen the guard, and a refused one changes nothing.
	r := tugIn(t, "cluster: payments-prod\nsafety: read-write\n", "profile", "edit", "payments")
	if r.code == 0 || !strings.Contains(r.stderr, "never read-write") || !strings.Contains(r.stderr, "tug unlock") {
		t.Fatalf("a read-write profile: %+v", r)
	}
	if b, _ := os.ReadFile(path); string(b) != paymentsProfile {
		t.Fatalf("a refused profile replaced the old one:\n%s", b)
	}
	mustTugIn(t, "cluster: payments-staging\nnamespace: web\nsafety: read-only\n", "profile", "edit", "strict-staging")
	mustTugIn(t, "cluster: payments-staging\n", "profile", "edit", "staging")

	// A cluster whose labels make it read-only stays read-only.
	out := mustTug(t, "profile", "use", "payments")
	if !strings.Contains(out, "Profile payments: connected to payments-prod, namespace payments, read-only") {
		t.Fatalf("profile use: %s", out)
	}
	st := status(t)
	if st.Cluster != "payments-prod" || st.Namespace != "payments" || !st.ReadOnly || st.Profile != "payments" ||
		st.Env != "prod" || !st.Production {
		t.Fatalf("after profile use payments: %+v", st)
	}
	prompt, err := os.ReadFile(filepath.Join(filepath.Dir(st.Kubeconfig), "prompt"))
	if err != nil || !strings.HasPrefix(string(prompt), "prod\tpayments-prod:payments\n0\tprod\tPROD\t-\n") {
		t.Fatalf("prompt file = %q, %v; want a read-only production prompt", prompt, err)
	}

	// Aliases run as typed out, and are held to the same guard.
	var viaAlias statusView
	if err := json.Unmarshal([]byte(mustTug(t, "profile", "run", "where")), &viaAlias); err != nil || viaAlias.Profile != "payments" {
		t.Fatalf("profile run where: %+v, %v", viaAlias, err)
	}
	if out := mustTug(t, "profile", "run", "st", "-o", "name"); strings.TrimSpace(out) != "payments-prod" {
		t.Fatalf("profile run st -o name: %q", out)
	}
	if r := tug(t, "profile", "run", "bounce"); r.code == 0 || !strings.Contains(r.stderr, "read-only") || writes.Load() != 0 {
		t.Fatalf("an alias that writes on a read-only session: %+v (writes %d)", r, writes.Load())
	}
	if r := tug(t, "profile", "run", "nope"); r.code == 0 || !strings.Contains(r.stderr, "bounce, st, where") {
		t.Fatalf("an unknown alias: %+v", r)
	}

	// The profile can make any cluster read-only, and auto leaves it to
	// the labels.
	mustTug(t, "profile", "use", "strict-staging")
	if st := status(t); st.Cluster != "payments-staging" || st.Namespace != "web" || !st.ReadOnly || st.Production {
		t.Fatalf("after profile use strict-staging: %+v", st)
	}
	mustTug(t, "profile", "use", "staging")
	if st := status(t); st.Cluster != "payments-staging" || st.ReadOnly || st.Profile != "staging" {
		t.Fatalf("after profile use staging: %+v", st)
	}

	var views []profileView
	if err := json.Unmarshal([]byte(mustTug(t, "profile", "ls", "-o", "json")), &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 3 || views[0].Name != "payments" || views[0].Path != path || views[0].Current ||
		!views[1].Current || views[2].Safety != "read-only" || len(views[0].Running) != 0 {
		t.Fatalf("profile ls -o json: %+v", views)
	}
	if out := mustTug(t, "profile", "show"); !strings.Contains(out, "staging") || !strings.Contains(out, "this terminal's") {
		t.Fatalf("profile show: %s", out)
	}
	if out := mustTug(t, "profile", "show", "payments"); !strings.Contains(out, "bounce  tug restart api") {
		t.Fatalf("profile show payments: %s", out)
	}

	// A pin replaces the profile while the shell is in its tree, and
	// leaving the tree puts the profile back.
	repo := t.TempDir()
	mustTug(t, "pin", "set", "payments-prod", "--dir", repo)
	if st := status(t); st.Cluster != "payments-prod" || st.Profile != "" {
		t.Fatalf("pinned: %+v", st)
	}
	mustTug(t, "session", "pin", "--dir", t.TempDir())
	if st := status(t); st.Cluster != "payments-staging" || st.Profile != "staging" {
		t.Fatalf("after leaving the pin: %+v", st)
	}

	// Connecting by hand leaves the profile, and its aliases with it.
	mustTug(t, "connect", "payments-staging")
	if st := status(t); st.Profile != "" {
		t.Fatalf("after connecting by hand: %+v", st)
	}
	if r := tug(t, "profile", "run", "where"); r.code == 0 || !strings.Contains(r.stderr, "not using a profile") {
		t.Fatalf("profile run without a profile: %+v", r)
	}
	if r := tug(t, "profile", "use", "missing"); r.code == 0 || !strings.Contains(r.stderr, "tug profile edit missing") {
		t.Fatalf("profile use of a missing profile: %+v", r)
	}

	// Without tugd there is nothing to stop.
	if out := mustTug(t, "profile", "down", "payments"); !strings.Contains(out, "No forwards of profile payments") {
		t.Fatalf("profile down without tugd: %s", out)
	}
}

// fakeForward stands in for client-go's port-forward inside tugd: it binds
// the ports asked for, 5432 as 5432:5432, and runs until stopped.
func fakeForward(ctx context.Context, f daemon.Forward, _ []string, up func(string, []string)) error {
	var bound []string
	for _, p := range f.Ports {
		local, remote, ok := strings.Cut(p, ":")
		if !ok {
			remote = local
		}
		bound = append(bound, local+":"+remote)
	}
	up(strings.ReplaceAll(f.Name, ".", "-")+"-0", bound)
	<-ctx.Done()
	return ctx.Err()
}

// startTugd runs tugd in this process for the isolated home, with forwards
// that do not reach a cluster.
func startTugd(t *testing.T, home string) {
	t.Helper()
	rt := filepath.Join(home, "run")
	srv := daemon.New(daemon.Config{RuntimeDir: rt, Sessions: session.NewManager(rt), Forward: fakeForward, ProxyAddr: "127.0.0.1:0"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("tugd: %v", err)
		}
	})
	c := daemon.NewClient(rt)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := c.Status(ctx); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("tugd did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// freePort returns a local port nothing listens on now.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestProfileForwards(t *testing.T) {
	home := isolate(t)
	srv, _ := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	startTugd(t, home)

	db, api := freePort(t), freePort(t)
	mustTugIn(t, fmt.Sprintf(`cluster: payments-staging
namespace: shop
forwards:
  - name: db
    target: sts/postgres
    ports: [%d]
  - name: api
    target: api
    namespace: web
    ports: ["%d:http"]
`, db, api), "profile", "edit", "payments")

	// The forwards are listed as stopped before they start.
	var states []profileForward
	if err := json.Unmarshal([]byte(mustTug(t, "pf", "profile", "ls", "payments", "-o", "json")), &states); err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || states[0].Name != "payments.db" || states[0].State != forwardStopped || states[1].Namespace != "web" {
		t.Fatalf("pf profile ls before up: %+v", states)
	}

	// A local port another program has is found before anything starts.
	taken, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", db))
	if err != nil {
		t.Fatal(err)
	}
	r := tug(t, "pf", "profile", "up", "payments")
	taken.Close()
	if r.code == 0 || !strings.Contains(r.stderr, fmt.Sprintf("payments.db wants %d, which another program", db)) {
		t.Fatalf("pf profile up with a port taken: %+v", r)
	}
	if out := mustTug(t, "pf", "ls", "-o", "json"); strings.TrimSpace(out) != "[]" {
		t.Fatalf("forwards after a refused start: %s", out)
	}

	// profile up connects the terminal and starts the forwards.
	r = tug(t, "profile", "up", "payments")
	if r.code != 0 || !strings.Contains(r.stdout, "Profile payments: connected to payments-staging") ||
		!strings.Contains(r.stdout, fmt.Sprintf("Forwarding localhost:%d -> %d to sts/postgres (pod/payments-db-0) as payments.db", db, db)) ||
		!strings.Contains(r.stdout, fmt.Sprintf("localhost:%d -> http to api", api)) {
		t.Fatalf("profile up: %+v", r)
	}
	var running []daemon.ForwardState
	if err := json.Unmarshal([]byte(mustTug(t, "pf", "ls", "-o", "json")), &running); err != nil {
		t.Fatal(err)
	}
	if len(running) != 2 || running[0].Group != "payments" || running[0].ClusterID == "" || running[1].Namespace != "shop" {
		t.Fatalf("pf ls after profile up: %+v", running)
	}
	r = tug(t, "pf", "ls")
	if r.code != 0 || !strings.Contains(r.stdout, "PROFILE") || !strings.Contains(r.stdout, "forwarding") ||
		!strings.Contains(r.stderr, "after 1s and then up to 30s apart") {
		t.Fatalf("pf ls: %+v", r)
	}

	// Starting again leaves running forwards alone.
	if out := mustTug(t, "pf", "profile", "up", "payments"); strings.Count(out, "Already forwarding") != 2 {
		t.Fatalf("pf profile up again: %s", out)
	}
	// Another profile cannot take the same port.
	mustTugIn(t, fmt.Sprintf("cluster: payments-staging\nforwards: [{name: db, target: db, ports: [%d]}]\n", db), "profile", "edit", "other")
	if r := tug(t, "pf", "profile", "up", "other"); r.code == 0 || !strings.Contains(r.stderr, "the forward payments.db listens") {
		t.Fatalf("a second profile on the same port: %+v", r)
	}

	// Forwards that left the profile file still belong to it.
	mustTugIn(t, fmt.Sprintf("cluster: payments-staging\nforwards: [{name: db, target: sts/postgres, ports: [%d]}]\n", db), "profile", "edit", "payments")
	if out := mustTug(t, "pf", "profile", "ls", "payments"); !strings.Contains(out, "no longer in the profile") {
		t.Fatalf("pf profile ls after the api forward left the file: %s", out)
	}

	// profile down stops them all and says so; the terminal stays connected.
	out := mustTug(t, "profile", "down", "payments")
	if !strings.Contains(out, "Stopped payments.api") || !strings.Contains(out, "Stopped payments.db") {
		t.Fatalf("profile down: %s", out)
	}
	if out := mustTug(t, "pf", "ls", "-o", "json"); strings.TrimSpace(out) != "[]" {
		t.Fatalf("forwards after profile down: %s", out)
	}
	if st := status(t); st.Profile != "payments" {
		t.Fatalf("status after profile down: %+v", st)
	}
}

// The editor works on a copy: a profile that does not parse is offered for
// editing again and never saved, and an unchanged one is left alone.
func TestProfileEditInEditor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in editor is a shell script")
	}
	isolate(t)
	dir := t.TempDir()
	// The first edit loosens the guard, which is refused; the second is fine.
	editor := filepath.Join(dir, "editor")
	script := `#!/bin/sh
n=$(cat "` + dir + `/n" 2>/dev/null || echo 0); n=$((n + 1)); echo $n > "` + dir + `/n"
if [ $n -eq 1 ]; then printf 'cluster: prod\nsafety: read-write\n' > "$1"; else printf 'cluster: prod\nnamespace: web\n' > "$1"; fi
`
	if err := os.WriteFile(editor, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", editor)
	var out, errb bytes.Buffer
	a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: &errb, now: time.Now}
	if err := a.setup(false); err != nil {
		t.Fatal(err)
	}
	store := a.profiles()
	path, _ := store.Path("dev")
	if err := a.editProfile(context.Background(), store, "dev", path); err != nil {
		t.Fatalf("editProfile: %v\n%s", err, errb.String())
	}
	if !strings.Contains(errb.String(), "never read-write") || !strings.Contains(out.String(), "Saved "+path) {
		t.Fatalf("stdout %q, stderr %q", out.String(), errb.String())
	}
	if b, _ := os.ReadFile(path); string(b) != "cluster: prod\nnamespace: web\n" {
		t.Fatalf("saved %q", b)
	}
	entries, _ := os.ReadDir(store.Dir())
	if len(entries) != 1 {
		t.Fatalf("the editor's copy was left behind: %v", entries)
	}

	// An editor that changes nothing saves nothing.
	if err := os.WriteFile(editor, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := a.editProfile(context.Background(), store, "dev", path); err != nil || !strings.Contains(out.String(), "No change") {
		t.Fatalf("an unchanged edit: %v, %q", err, out.String())
	}
}
