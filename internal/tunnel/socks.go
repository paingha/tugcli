package tunnel

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// localProxy is a SOCKS5 proxy on a loopback port that dials through a
// tunnel. Other users of the machine could reach a loopback port too, so it
// asks for a random username and password that only this process knows.
type localProxy struct {
	ln         net.Listener
	dial       DialFunc
	closer     io.Closer
	log        *slog.Logger
	user, pass string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

func startLocalProxy(dial DialFunc, closer io.Closer, log *slog.Logger) (*localProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listening for the local tunnel proxy: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	lp := &localProxy{ln: ln, dial: dial, closer: closer, log: log, user: randomHex(8), pass: randomHex(16), ctx: ctx, cancel: cancel}
	lp.wg.Go(lp.serve)
	return lp, nil
}

func (lp *localProxy) url() *url.URL {
	return &url.URL{Scheme: "socks5", User: url.UserPassword(lp.user, lp.pass), Host: lp.ln.Addr().String()}
}

func (lp *localProxy) serve() {
	for {
		c, err := lp.ln.Accept()
		if err != nil {
			return // closed
		}
		lp.wg.Go(func() { lp.handle(c) })
	}
}

func (lp *localProxy) close() error {
	var err error
	lp.once.Do(func() {
		lp.cancel()
		err = lp.ln.Close()
		lp.wg.Wait()
		if lp.closer != nil {
			err = errors.Join(err, lp.closer.Close())
		}
	})
	return err
}

// SOCKS5 constants from RFC 1928 and RFC 1929.
const (
	socksVersion    = 5
	authUserPass    = 2
	authNone        = 0xff
	cmdConnect      = 1
	atypIPv4        = 1
	atypDomain      = 3
	atypIPv6        = 4
	replySucceeded  = 0
	replyFailure    = 1
	replyRefused    = 5
	replyCmdUnknown = 7
)

func (lp *localProxy) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second)) // for the handshake only
	r := bufio.NewReader(c)
	target, err := lp.handshake(r, c)
	if err != nil {
		lp.log.Debug("refusing a tunnel proxy client", "err", err)
		return
	}
	upstream, err := lp.dial(lp.ctx, "tcp", target)
	if err != nil {
		lp.log.Warn("opening a connection through the tunnel", "target", target, "err", err)
		_ = reply(c, replyRefused)
		return
	}
	defer upstream.Close()
	if err := reply(c, replySucceeded); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, r) // r holds anything read past the handshake
		closeWrite(upstream)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(c, upstream)
		closeWrite(c)
		done <- struct{}{}
	}()
	// Wait for both directions, unless the pool closes first; returning
	// closes both connections, which ends the copies.
	for range 2 {
		select {
		case <-done:
		case <-lp.ctx.Done():
			return
		}
	}
}

// handshake reads the greeting, checks the credentials and returns the
// address the client wants.
func (lp *localProxy) handshake(r *bufio.Reader, w io.Writer) (string, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", err
	}
	if hdr[0] != socksVersion {
		return "", fmt.Errorf("SOCKS version %d", hdr[0])
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(r, methods); err != nil {
		return "", err
	}
	if !containsByte(methods, authUserPass) {
		_, _ = w.Write([]byte{socksVersion, authNone})
		return "", errors.New("the client offers no username and password")
	}
	if _, err := w.Write([]byte{socksVersion, authUserPass}); err != nil {
		return "", err
	}
	// RFC 1929: version 1, username, password.
	ver, err := r.ReadByte()
	if err != nil || ver != 1 {
		return "", fmt.Errorf("bad authentication version %d: %w", ver, err)
	}
	user, err := readShortString(r)
	if err != nil {
		return "", err
	}
	pass, err := readShortString(r)
	if err != nil {
		return "", err
	}
	ok := subtle.ConstantTimeCompare([]byte(user), []byte(lp.user)) == 1 && subtle.ConstantTimeCompare([]byte(pass), []byte(lp.pass)) == 1
	if !ok {
		_, _ = w.Write([]byte{1, 1})
		return "", errors.New("wrong credentials")
	}
	if _, err := w.Write([]byte{1, 0}); err != nil {
		return "", err
	}

	var req [4]byte
	if _, err := io.ReadFull(r, req[:]); err != nil {
		return "", err
	}
	if req[0] != socksVersion || req[1] != cmdConnect {
		_ = reply(w, replyCmdUnknown)
		return "", fmt.Errorf("SOCKS command %d", req[1])
	}
	var host string
	switch req[3] {
	case atypIPv4, atypIPv6:
		n := net.IPv4len
		if req[3] == atypIPv6 {
			n = net.IPv6len
		}
		ip := make([]byte, n)
		if _, err := io.ReadFull(r, ip); err != nil {
			return "", err
		}
		host = net.IP(ip).String()
	case atypDomain:
		if host, err = readShortString(r); err != nil {
			return "", err
		}
	default:
		_ = reply(w, replyFailure)
		return "", fmt.Errorf("SOCKS address type %d", req[3])
	}
	var port [2]byte
	if _, err := io.ReadFull(r, port[:]); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))), nil
}

func reply(w io.Writer, code byte) error {
	_, err := w.Write([]byte{socksVersion, code, 0, atypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

func readShortString(r *bufio.Reader) (string, error) {
	n, err := r.ReadByte()
	if err != nil {
		return "", err
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

func containsByte(bs []byte, b byte) bool {
	for _, x := range bs {
		if x == b {
			return true
		}
	}
	return false
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return hex.EncodeToString(b)
}

// dialProxy connects to address through a SOCKS5 or HTTP proxy URL.
func dialProxy(ctx context.Context, u *url.URL, address string) (net.Conn, error) {
	switch u.Scheme {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pass, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pass}
		}
		d, err := proxy.SOCKS5("tcp", u.Host, auth, &net.Dialer{Timeout: 30 * time.Second})
		if err != nil {
			return nil, err
		}
		return d.(proxy.ContextDialer).DialContext(ctx, "tcp", address)
	case "http", "https":
		return dialConnect(ctx, u, address)
	default:
		return nil, fmt.Errorf("%s is not a proxy", u.Redacted())
	}
}

// dialConnect opens address through an HTTP proxy with CONNECT.
func dialConnect(ctx context.Context, u *url.URL, address string) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "https" {
		c = tls.Client(c, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: http.Header{}}
	if u.User != nil {
		pass, _ := u.User.Password()
		req.SetBasicAuth(u.User.Username(), pass)
		req.Header.Set("Proxy-Authorization", req.Header.Get("Authorization"))
		req.Header.Del("Authorization")
	}
	if err := req.Write(c); err != nil {
		c.Close()
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), req)
	if err != nil {
		c.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.Close()
		return nil, fmt.Errorf("the proxy answered %s", resp.Status)
	}
	return c, nil
}
