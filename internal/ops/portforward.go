package ops

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// ForwardOptions describe a port-forward to one pod.
type ForwardOptions struct {
	Namespace string
	Pod       string
	// Ports are "local:remote" pairs with numeric ports.
	Ports []string
	// Addresses to listen on; defaults to localhost.
	Addresses []string
	// Ready is closed once the listeners are up.
	Ready chan struct{}
	// Bound, when set, is called once the listeners are up with the
	// local:remote ports actually bound, so a port given as :80 can be
	// kept the same next time.
	Bound  func(ports []string)
	Out    io.Writer
	ErrOut io.Writer
}

// PortForward forwards local ports to a pod until ctx ends or the pod goes
// away. It tunnels SPDY over WebSockets, falling back to plain SPDY.
func PortForward(ctx context.Context, cfg *rest.Config, cs kubernetes.Interface, o ForwardOptions) error {
	u := cs.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(o.Namespace).Name(o.Pod).SubResource("portforward").URL()

	transport, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, u)
	tunnel, err := portforward.NewSPDYOverWebsocketDialer(u, cfg)
	if err != nil {
		return err
	}
	dialer = portforward.NewFallbackDialer(tunnel, dialer, shouldFallback)

	addrs := o.Addresses
	if len(addrs) == 0 {
		addrs = []string{"localhost"}
	}
	ready := o.Ready
	if ready == nil {
		ready = make(chan struct{})
	}
	fw, err := portforward.NewOnAddressesWithContext(ctx, dialer, addrs, o.Ports, ready, o.Out, o.ErrOut)
	if err != nil {
		return err
	}
	if o.Bound != nil {
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ready:
			case <-done:
				return
			}
			fps, err := fw.GetPorts()
			if err != nil {
				return
			}
			bound := make([]string, len(fps))
			for i, p := range fps {
				bound[i] = fmt.Sprintf("%d:%d", p.Local, p.Remote)
			}
			o.Bound(bound)
		}()
	}
	return fw.ForwardPorts()
}

// ResolvePorts turns port arguments into "local:remote" pairs for a pod.
//
//	8080        local 8080 to container port 8080
//	9000:http   local 9000 to the container port named http
//	:80         a free local port to container port 80
//
// With no arguments every declared container port is forwarded to the same
// local port.
func ResolvePorts(p *corev1.Pod, args []string) ([]string, error) {
	if len(args) == 0 {
		var out []string
		for _, c := range p.Spec.Containers {
			for _, cp := range c.Ports {
				if cp.Protocol == "" || cp.Protocol == corev1.ProtocolTCP {
					n := strconv.Itoa(int(cp.ContainerPort))
					out = append(out, n+":"+n)
				}
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("pod %s declares no TCP ports; name one, for example: tug pf <target> 8080", p.Name)
		}
		return out, nil
	}
	out := make([]string, 0, len(args))
	for _, a := range args {
		local, remote, hasColon := strings.Cut(a, ":")
		if !hasColon {
			remote = local
		}
		rport, err := containerPort(p, remote)
		if err != nil {
			return nil, err
		}
		if hasColon && local == "" {
			out = append(out, ":"+rport)
			continue
		}
		if _, err := strconv.ParseUint(local, 10, 16); err != nil {
			if hasColon {
				return nil, fmt.Errorf("local port %q is not a number", local)
			}
			local = rport // a named port: listen on its number
		}
		out = append(out, local+":"+rport)
	}
	return out, nil
}

// containerPort resolves a numeric or named container port.
func containerPort(p *corev1.Pod, s string) (string, error) {
	if n, err := strconv.ParseUint(s, 10, 16); err == nil && n > 0 {
		return s, nil
	}
	for _, c := range p.Spec.Containers {
		for _, cp := range c.Ports {
			if cp.Name == s {
				return strconv.Itoa(int(cp.ContainerPort)), nil
			}
		}
	}
	return "", fmt.Errorf("pod %s has no port named %q", p.Name, s)
}
