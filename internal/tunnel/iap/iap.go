// Package iap opens TCP connections through Google Cloud Identity-Aware
// Proxy (IAP) TCP forwarding, as `gcloud compute start-iap-tunnel` does, so
// tug can reach a port on a VM, or a host in a VPC such as a private GKE
// control plane, with no gcloud installed.
//
// Each connection is a WebSocket to the IAP relay carrying a small framed
// protocol. The protocol facts here come from the Cloud SDK's
// iap_tunnel_websocket modules and Google's IAP TCP forwarding
// documentation.
package iap

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// DefaultInterface is the network interface of an instance that a tunnel
// reaches when Config.Interface is empty, as in gcloud.
const DefaultInterface = "nic0"

// Config is one IAP tunnel. Either Instance (with Zone) or Group (with
// Region and Network) is set: the instance mode forwards to a port on a VM,
// the destination group mode forwards to a host in the VPC, such as a
// private GKE endpoint.
type Config struct {
	Project, Zone, Instance, Interface string // instance mode; Interface defaults to nic0
	Region, Network, Group             string // destination group mode
}

// urlSettings are the query parameters an iap:// URL may have.
var urlSettings = []string{"project", "zone", "interface", "region", "network", "group"}

// ParseURL reads iap://<instance>?project=..&zone=.. or
// iap://?project=..&region=..&network=..&group=..
//
// An instance URL may also set interface. The port is not part of the URL:
// it comes with the address given to Dial.
func ParseURL(u *url.URL) (Config, error) {
	if u == nil {
		return Config{}, errors.New("no iap:// URL")
	}
	if u.Scheme != "iap" {
		return Config{}, fmt.Errorf("%s is not an iap:// URL", u.Redacted())
	}
	if u.Port() != "" {
		return Config{}, fmt.Errorf("the IAP URL %s has a port; give the port with the address instead", u.Redacted())
	}
	if u.User != nil || u.Opaque != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return Config{}, fmt.Errorf("the IAP URL %s may have only an instance name and settings", u.Redacted())
	}
	q := u.Query()
	for k, vs := range q {
		if !slices.Contains(urlSettings, k) {
			return Config{}, fmt.Errorf("the IAP URL %s has an unknown setting %q; it may set %s", u.Redacted(), k, strings.Join(urlSettings, ", "))
		}
		if len(vs) > 1 {
			return Config{}, fmt.Errorf("the IAP URL %s sets %s more than once", u.Redacted(), k)
		}
	}
	cfg := Config{
		Project:   q.Get("project"),
		Zone:      q.Get("zone"),
		Instance:  u.Hostname(),
		Interface: q.Get("interface"),
		Region:    q.Get("region"),
		Network:   q.Get("network"),
		Group:     q.Get("group"),
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("reading the IAP URL %s: %w", u.Redacted(), err)
	}
	return cfg, nil
}

// String returns the tunnel as an iap:// URL that ParseURL reads back.
func (c Config) String() string {
	q := url.Values{}
	for k, v := range map[string]string{
		"project": c.Project, "zone": c.Zone, "interface": c.Interface,
		"region": c.Region, "network": c.Network, "group": c.Group,
	} {
		if v != "" {
			q.Set(k, v)
		}
	}
	return "iap://" + c.Instance + "?" + q.Encode()
}

// groupMode reports whether the tunnel goes to a host through a destination
// group rather than to an instance.
func (c Config) groupMode() bool { return c.Instance == "" }

// validate checks that exactly one mode is set, with what it needs.
func (c Config) validate() error {
	instance := c.Instance != "" || c.Zone != "" || c.Interface != ""
	group := c.Group != "" || c.Region != "" || c.Network != ""
	switch {
	case c.Project == "":
		return errors.New("an IAP tunnel needs a project")
	case instance && group:
		return errors.New("an IAP tunnel goes to an instance (with zone) or through a destination group (with region and network), not both")
	case instance && c.Instance == "":
		return errors.New("an IAP tunnel with a zone or interface needs an instance")
	case instance && c.Zone == "":
		return fmt.Errorf("the IAP tunnel to instance %s needs its zone", c.Instance)
	case instance:
		return nil
	case group && (c.Group == "" || c.Region == "" || c.Network == ""):
		return errors.New("an IAP tunnel through a destination group needs the group, its region and the VPC network")
	case group:
		return nil
	default:
		return errors.New("an IAP tunnel needs an instance (with zone) or a destination group (with region and network)")
	}
}

// target is a validated tunnel and the address it forwards to.
type target struct {
	cfg  Config
	host string // the destination group mode's host; unused for an instance
	port int
}

// targetFor checks cfg and parses address, host:port. For an instance only
// the port is used.
func targetFor(cfg Config, address string) (target, error) {
	if err := cfg.validate(); err != nil {
		return target{}, err
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return target{}, fmt.Errorf("parsing the IAP tunnel address %q: %w", address, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return target{}, fmt.Errorf("the IAP tunnel address %q needs a port number from 1 to 65535", address)
	}
	if cfg.groupMode() && host == "" {
		return target{}, fmt.Errorf("the IAP tunnel address %q needs a host to reach through destination group %s", address, cfg.Group)
	}
	return target{cfg: cfg, host: host, port: port}, nil
}

// String describes the destination for messages.
func (t target) String() string {
	if t.cfg.groupMode() {
		return fmt.Sprintf("%s through destination group %s in %s (project %s)",
			net.JoinHostPort(t.host, strconv.Itoa(t.port)), t.cfg.Group, t.cfg.Region, t.cfg.Project)
	}
	return fmt.Sprintf("port %d of instance %s in %s (project %s)", t.port, t.cfg.Instance, t.cfg.Zone, t.cfg.Project)
}

// addr is the destination as a net.Addr.
func (t target) addr() net.Addr {
	host := t.host
	if !t.cfg.groupMode() {
		host = t.cfg.Instance
	}
	return addr(net.JoinHostPort(host, strconv.Itoa(t.port)))
}

// connectQuery is the query of the relay's connect endpoint. The names are
// gcloud's. newWebsocket is what gcloud's current WebSocket client sends,
// in Python's spelling of true.
func (t target) connectQuery() url.Values {
	q := url.Values{}
	q.Set("project", t.cfg.Project)
	q.Set("port", strconv.Itoa(t.port))
	q.Set("newWebsocket", "True")
	if t.cfg.groupMode() {
		q.Set("region", t.cfg.Region)
		q.Set("network", t.cfg.Network)
		q.Set("host", t.host)
		q.Set("group", t.cfg.Group)
		return q
	}
	iface := t.cfg.Interface
	if iface == "" {
		iface = DefaultInterface
	}
	q.Set("zone", t.cfg.Zone)
	q.Set("instance", t.cfg.Instance)
	q.Set("interface", iface)
	return q
}

// notFoundHint says what to check when the relay cannot find the target.
func (t target) notFoundHint() string {
	if t.cfg.groupMode() {
		return "check the project, region and destination group names"
	}
	return "check that the instance exists in that zone and project and is running"
}

// addr is a tunnel end as a net.Addr.
type addr string

// Network implements net.Addr.
func (addr) Network() string { return "iap" }

// String implements net.Addr.
func (a addr) String() string { return string(a) }
