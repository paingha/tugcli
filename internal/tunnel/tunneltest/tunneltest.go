// Package tunneltest provides an SSH jump host for tests of tunnels.
package tunneltest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SSHServer is a jump host that accepts one key and forwards direct-tcpip
// channels, as sshd does for ssh -J.
type SSHServer struct {
	addr      string
	knownHost string
	dials     []string
	mu        sync.Mutex
}

// Dials returns the addresses the jump host has opened, in order.
func (s *SSHServer) Dials() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.dials...)
}

func newSSHServer(t *testing.T, client ssh.PublicKey) *SSHServer {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostKey, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if string(k.Marshal()) != string(client.Marshal()) {
			return nil, fmt.Errorf("unknown key")
		}
		return nil, nil
	}}
	cfg.AddHostKey(hostKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &SSHServer{addr: ln.Addr().String()}
	s.knownHost = knownhosts.Line([]string{knownhosts.Normalize(s.addr)}, hostKey.PublicKey())
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(raw, cfg)
		}
	}()
	return s
}

func (s *SSHServer) serve(raw net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(raw, cfg)
	if err != nil {
		raw.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "direct-tcpip" {
			_ = nc.Reject(ssh.UnknownChannelType, "only direct-tcpip")
			continue
		}
		// RFC 4254 7.2: host to connect, port, originator address, port.
		var req struct {
			Host     string
			Port     uint32
			OrigHost string
			OrigPort uint32
		}
		if err := ssh.Unmarshal(nc.ExtraData(), &req); err != nil {
			_ = nc.Reject(ssh.ConnectionFailed, err.Error())
			continue
		}
		target := net.JoinHostPort(req.Host, fmt.Sprint(req.Port))
		s.mu.Lock()
		s.dials = append(s.dials, target)
		s.mu.Unlock()
		up, err := net.Dial("tcp", target)
		if err != nil {
			_ = nc.Reject(ssh.ConnectionFailed, err.Error())
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			up.Close()
			continue
		}
		go ssh.DiscardRequests(chReqs)
		go func() {
			defer ch.Close()
			defer up.Close()
			go func() {
				_, _ = io.Copy(up, ch)
				_ = up.(*net.TCPConn).CloseWrite() // pass the client's EOF on, as sshd does
			}()
			_, _ = io.Copy(ch, up)
		}()
	}
}

// SSH starts a jump host, writes a client key and, when trustHost is set,
// a known_hosts entry for it, and returns the tunnel spec that uses them.
func SSH(t *testing.T, trustHost bool) (spec string, srv *SSHServer) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	srv = newSSHServer(t, sshPub)
	dir := t.TempDir()
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(key, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(dir, "known_hosts")
	line := ""
	if trustHost {
		line = srv.knownHost + "\n"
	}
	if err := os.WriteFile(known, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	q := url.Values{"identity": {key}, "known_hosts": {known}}
	return "ssh://tester@" + srv.addr + "?" + q.Encode(), srv
}
