package tunnel

import (
	"context"
	"errors"
	"net"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testKind registers a tunnel kind called scheme for one test.
func testKind(t *testing.T, scheme string, b builder) {
	t.Helper()
	kinds := Kinds
	Kinds = append(slices.Clone(Kinds), scheme)
	builders[scheme] = b
	t.Cleanup(func() {
		Kinds = kinds
		delete(builders, scheme)
	})
}

// closerFunc is an io.Closer.
type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func TestRouteHandsOutTheEndpoint(t *testing.T) {
	target := echoServer(t)
	ep := &Endpoint{Server: "https://example.com:443/v1/x", CAData: []byte("ca"), ServerName: "root", CertData: []byte("cert"), KeyData: []byte("key")}
	var d net.Dialer
	testKind(t, "fake", func(context.Context, *url.URL, *Pool) (opened, error) {
		return opened{dial: d.DialContext, endpoint: ep}, nil
	})
	p := newTestPool(t)
	ctx := context.Background()

	r, err := p.Route(ctx, "fake://host")
	if err != nil {
		t.Fatal(err)
	}
	if r.Endpoint != ep || r.Proxy == nil || r.Proxy.Scheme != "socks5" {
		t.Fatalf("Route = %+v, want the builder's endpoint behind a local SOCKS5 proxy", r)
	}
	c, err := dialProxy(ctx, r.Proxy, target)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, c)

	if r, err := p.Route(ctx, "socks5://127.0.0.1:1080"); err != nil || r.Endpoint != nil || r.Proxy.String() != "socks5://127.0.0.1:1080" {
		t.Errorf("Route(socks5) = %+v, %v, want the proxy as it is and no endpoint", r, err)
	}
}

func TestRouteOpensATunnelOnce(t *testing.T) {
	var (
		builds  atomic.Int32
		release = make(chan struct{})
		d       net.Dialer
	)
	testKind(t, "fake", func(context.Context, *url.URL, *Pool) (opened, error) {
		builds.Add(1)
		<-release
		return opened{dial: d.DialContext}, nil
	})
	p := newTestPool(t)

	// Callers that ask while the tunnel opens wait for it, rather than
	// opening another; one that gives up gets its own context's error.
	var wg sync.WaitGroup
	urls := make([]string, 3)
	for i := range urls {
		wg.Go(func() {
			r, err := p.Route(context.Background(), "fake://host")
			if err != nil {
				t.Error(err)
				return
			}
			urls[i] = r.Proxy.String()
		})
	}
	waitFor(t, func() bool { return builds.Load() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := p.Route(ctx, "fake://host"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Route with an expired context = %v, want its deadline", err)
	}
	close(release)
	wg.Wait()
	if n := builds.Load(); n != 1 || urls[0] == "" || urls[0] != urls[1] || urls[1] != urls[2] {
		t.Fatalf("%d builds gave %v; want one tunnel for every caller", n, urls)
	}
}

func TestRouteReopensATunnelThatEnded(t *testing.T) {
	var (
		builds atomic.Int32
		closed atomic.Int32
		ended  = make(chan struct{})
		d      net.Dialer
	)
	testKind(t, "fake", func(context.Context, *url.URL, *Pool) (opened, error) {
		o := opened{dial: d.DialContext, closer: closerFunc(func() error { closed.Add(1); return nil })}
		if builds.Add(1) == 1 {
			o.done = ended
		}
		return o, nil
	})
	p := newTestPool(t)
	ctx := context.Background()

	first, err := p.Route(ctx, "fake://host")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := p.Route(ctx, "fake://host"); again.Proxy.String() != first.Proxy.String() || builds.Load() != 1 {
		t.Fatal("the pool opened the tunnel again before it ended")
	}
	close(ended)
	second, err := p.Route(ctx, "fake://host")
	if err != nil {
		t.Fatal(err)
	}
	if builds.Load() != 2 || second.Proxy.String() == first.Proxy.String() || closed.Load() != 1 {
		t.Fatalf("after the tunnel ended: %d builds, %d closed, proxies %s then %s; want it closed and opened again", builds.Load(), closed.Load(), first.Proxy, second.Proxy)
	}
}

func TestRouteRetriesAFailedTunnel(t *testing.T) {
	var (
		builds atomic.Int32
		d      net.Dialer
	)
	testKind(t, "fake", func(context.Context, *url.URL, *Pool) (opened, error) {
		if builds.Add(1) == 1 {
			return opened{}, errors.New("the jump host is down")
		}
		return opened{dial: d.DialContext}, nil
	})
	p := newTestPool(t)
	if _, err := p.Route(context.Background(), "fake://host"); err == nil || err.Error() != "the jump host is down" {
		t.Fatalf("first Route = %v, want the builder's error", err)
	}
	if _, err := p.Route(context.Background(), "fake://host"); err != nil || builds.Load() != 2 {
		t.Fatalf("second Route = %v after %d builds, want a new attempt that works", err, builds.Load())
	}
}

func TestCloseWhileATunnelOpens(t *testing.T) {
	var (
		started = make(chan struct{})
		release = make(chan struct{})
		closed  = make(chan struct{})
		d       net.Dialer
	)
	testKind(t, "fake", func(context.Context, *url.URL, *Pool) (opened, error) {
		close(started)
		<-release
		return opened{dial: d.DialContext, closer: closerFunc(func() error { close(closed); return nil })}, nil
	})
	p := newTestPool(t)
	errc := make(chan error, 1)
	go func() {
		_, err := p.Route(context.Background(), "fake://host")
		errc <- err
	}()
	<-started
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-errc; err == nil {
		t.Fatal("Route succeeded on a pool closed while the tunnel opened")
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the tunnel opened after Close was never closed")
	}
}

func TestReplacesEndpoint(t *testing.T) {
	endpointKinds = append(endpointKinds, "fake")
	t.Cleanup(func() { endpointKinds = slices.DeleteFunc(endpointKinds, func(k string) bool { return k == "fake" }) })
	for spec, want := range map[string]bool{
		"fake://host":           true,
		"ssh://admin@bastion":   false,
		"socks5://127.0.0.1:80": false,
		"::":                    false,
	} {
		if got := ReplacesEndpoint(spec); got != want {
			t.Errorf("ReplacesEndpoint(%q) = %t, want %t", spec, got, want)
		}
	}
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(time.Millisecond)
	}
}
