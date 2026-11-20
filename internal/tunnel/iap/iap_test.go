package iap

import (
	"net/url"
	"strings"
	"testing"
)

func TestParseURL(t *testing.T) {
	tests := []struct {
		url     string
		want    Config
		wantErr string
	}{
		{
			url:  "iap://bastion?project=p1&zone=europe-west2-a",
			want: Config{Project: "p1", Zone: "europe-west2-a", Instance: "bastion"},
		},
		{
			url:  "iap://bastion/?project=p1&zone=europe-west2-a&interface=nic1",
			want: Config{Project: "p1", Zone: "europe-west2-a", Instance: "bastion", Interface: "nic1"},
		},
		{
			url:  "iap://?project=p1&region=europe-west2&network=default&group=gke-masters",
			want: Config{Project: "p1", Region: "europe-west2", Network: "default", Group: "gke-masters"},
		},
		{url: "https://bastion?project=p1&zone=z", wantErr: "not an iap:// URL"},
		{url: "iap://bastion:22?project=p1&zone=z", wantErr: "give the port with the address"},
		{url: "iap://bastion/path?project=p1&zone=z", wantErr: "only an instance name"},
		{url: "iap://bastion?zone=z", wantErr: "needs a project"},
		{url: "iap://bastion?project=p1", wantErr: "needs its zone"},
		{url: "iap://?project=p1&zone=z", wantErr: "needs an instance"},
		{url: "iap://?project=p1", wantErr: "needs an instance (with zone) or a destination group"},
		{url: "iap://bastion?project=p1&zone=z&group=g", wantErr: "not both"},
		{url: "iap://?project=p1&region=r&group=g", wantErr: "needs the group, its region and the VPC network"},
		{url: "iap://bastion?project=p1&zone=z&port=22", wantErr: `unknown setting "port"`},
		{url: "iap://bastion?project=p1&project=p2&zone=z", wantErr: "sets project more than once"},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			u, err := url.Parse(tt.url)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParseURL(u)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseURL(%s) error = %v, want one containing %q", tt.url, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseURL(%s): %v", tt.url, err)
			}
			if got != tt.want {
				t.Errorf("ParseURL(%s) = %+v, want %+v", tt.url, got, tt.want)
			}
			back, err := url.Parse(got.String())
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", got.String(), err)
			}
			if again, err := ParseURL(back); err != nil || again != got {
				t.Errorf("ParseURL(%s) = %+v, %v; want %+v", got.String(), again, err, got)
			}
		})
	}
	if _, err := ParseURL(nil); err == nil {
		t.Error("ParseURL(nil) succeeded")
	}
}

func TestTargetQueries(t *testing.T) {
	instance := Config{Project: "p1", Zone: "europe-west2-a", Instance: "bastion"}
	group := Config{Project: "p1", Region: "europe-west2", Network: "default", Group: "gke-masters"}
	tests := []struct {
		name      string
		cfg       Config
		address   string
		connect   string
		reconnect string
		addr      string
	}{
		{
			name:      "instance",
			cfg:       instance,
			address:   "ignored.example:22",
			connect:   "instance=bastion&interface=nic0&newWebsocket=True&port=22&project=p1&zone=europe-west2-a",
			reconnect: "ack=12345&newWebsocket=True&sid=s1&zone=europe-west2-a",
			addr:      "bastion:22",
		},
		{
			name:      "instance with interface",
			cfg:       Config{Project: "p1", Zone: "z", Instance: "vm", Interface: "nic2"},
			address:   ":8080",
			connect:   "instance=vm&interface=nic2&newWebsocket=True&port=8080&project=p1&zone=z",
			reconnect: "ack=12345&newWebsocket=True&sid=s1&zone=z",
			addr:      "vm:8080",
		},
		{
			name:      "group",
			cfg:       group,
			address:   "172.16.0.2:443",
			connect:   "group=gke-masters&host=172.16.0.2&network=default&newWebsocket=True&port=443&project=p1&region=europe-west2",
			reconnect: "ack=12345&newWebsocket=True&region=europe-west2&sid=s1",
			addr:      "172.16.0.2:443",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tg, err := targetFor(tt.cfg, tt.address)
			if err != nil {
				t.Fatalf("targetFor(%q): %v", tt.address, err)
			}
			if got := tg.connectQuery().Encode(); got != tt.connect {
				t.Errorf("connectQuery() = %s, want %s", got, tt.connect)
			}
			if got := tg.reconnectQuery("s1", 12345).Encode(); got != tt.reconnect {
				t.Errorf("reconnectQuery() = %s, want %s", got, tt.reconnect)
			}
			if got := tg.addr(); got.String() != tt.addr || got.Network() != "iap" {
				t.Errorf("addr() = %s %s, want iap %s", got.Network(), got, tt.addr)
			}
		})
	}
}

func TestTargetForRejects(t *testing.T) {
	instance := Config{Project: "p1", Zone: "z", Instance: "vm"}
	group := Config{Project: "p1", Region: "r", Network: "n", Group: "g"}
	tests := []struct {
		cfg     Config
		address string
		wantErr string
	}{
		{instance, "22", "parsing the IAP tunnel address"},
		{instance, "vm:ssh", "needs a port number"},
		{instance, "vm:0", "needs a port number"},
		{instance, "vm:65536", "needs a port number"},
		{group, ":443", "needs a host"},
		{Config{Project: "p1"}, "vm:22", "needs an instance"},
	}
	for _, tt := range tests {
		if _, err := targetFor(tt.cfg, tt.address); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("targetFor(%+v, %q) error = %v, want one containing %q", tt.cfg, tt.address, err, tt.wantErr)
		}
	}
}
