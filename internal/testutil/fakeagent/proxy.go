package fakeagent

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

// Proxy is a man-in-the-middle TLS relay between a transfer source and
// destination agent. It presents the destination's node certificate to the
// source and the source's node certificate to the destination (both are
// CA-verified by the real agents, so the relay is transparent to the
// protocol). It counts source→destination bytes and can hold the stream
// open-paused at a threshold (mid-transfer interrupt), then tear the
// connection down.
type Proxy struct {
	t         *testing.T
	ln        net.Listener
	addr      string
	caPool    *x509.CertPool
	dstPair   tls.Certificate
	srcPair   tls.Certificate
	threshold int64 // relay src→dst byte count at which to hold the stream

	mu      sync.Mutex
	conns   []io.Closer
	relayed int64
	held    bool
	holdCh  chan struct{}
	closed  bool
}

// NewProxy builds a relay for one src→dst agent pair. dstCertPEM/dstKeyPEM
// and srcCertPEM/srcKeyPEM are the agents' node certificates (the proxy
// plays the destination to the source and the source to the destination);
// caPEM is the controller CA. threshold is the relay byte count (headers +
// payload) at which the forward copy pauses; 0 disables the pause.
func NewProxy(t *testing.T, caPEM, dstCertPEM, dstKeyPEM, srcCertPEM, srcKeyPEM []byte, threshold int64) *Proxy {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("proxy: no CA certificate parsed")
	}
	dstPair, err := tls.X509KeyPair(dstCertPEM, dstKeyPEM)
	if err != nil {
		t.Fatalf("proxy: dst pair: %v", err)
	}
	srcPair, err := tls.X509KeyPair(srcCertPEM, srcKeyPEM)
	if err != nil {
		t.Fatalf("proxy: src pair: %v", err)
	}
	return &Proxy{t: t, caPool: pool, dstPair: dstPair, srcPair: srcPair, threshold: threshold, holdCh: make(chan struct{})}
}

// Start binds the relay to an ephemeral 127.0.0.1 port.
func (p *Proxy) Start() string {
	return p.StartOn("127.0.0.1:0")
}

// StartOn binds the relay to an exact pre-reserved address (FreeTCPPort).
// The destination agent advertises this address in its inventory before
// the relay exists, so the port must be known up front.
func (p *Proxy) StartOn(addr string) string {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		p.t.Fatalf("proxy listen: %v", err)
	}
	p.ln = ln
	p.addr = ln.Addr().String()
	go p.acceptLoop()
	p.t.Cleanup(p.Close)
	return p.addr
}

// Addr is the relay's listen address.
func (p *Proxy) Addr() string { return p.addr }

// Relayed is the total source→destination byte count.
func (p *Proxy) Relayed() int64 { return atomic.LoadInt64(&p.relayed) }

// Held reports whether the forward stream is paused at the threshold.
func (p *Proxy) Held() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.held
}

// Close releases the hold and tears every relayed connection down.
// holdCh is closed exactly once (Close is idempotent via p.closed): without
// it, a forward copy parked in hold() would leak after Close when the
// stream was already held.
func (p *Proxy) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.holdCh)
	conns := append([]io.Closer(nil), p.conns...)
	if p.ln != nil {
		conns = append(conns, p.ln)
	}
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (p *Proxy) acceptLoop() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.relay(conn)
	}
}

// track registers one closer for teardown.
func (p *Proxy) track(c io.Closer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conns = append(p.conns, c)
}
func (p *Proxy) relay(conn net.Conn) {
	p.track(conn)
	tc := tls.Server(conn, &tls.Config{
		Certificates: []tls.Certificate{p.dstPair},
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return io.ErrUnexpectedEOF
			}
			leaf, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			_, err = leaf.Verify(x509.VerifyOptions{Roots: p.caPool})
			return err
		},
	})
	if err := tc.Handshake(); err != nil {
		p.t.Logf("proxy: server handshake: %v", err)
		return
	}
	target := p.dstAddr()
	raw, err := net.Dial("tcp", target)
	if err != nil {
		p.t.Logf("proxy: dial target: %v", err)
		_ = tc.Close()
		return
	}
	p.track(raw)
	out := tls.Client(raw, &tls.Config{
		Certificates:       []tls.Certificate{p.srcPair},
		InsecureSkipVerify: true, // leaf SANs are node IDs, not the dial addr
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return io.ErrUnexpectedEOF
			}
			leaf, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			_, err = leaf.Verify(x509.VerifyOptions{Roots: p.caPool})
			return err
		},
	})
	if err := out.Handshake(); err != nil {
		_ = out.Close()
		return
	}
	var wg sync.WaitGroup
	wg.Add(2)
	// Destination→source (acks/errors): plain relay.
	go func() {
		defer wg.Done()
		_, _ = io.Copy(tc, out)
	}()
	// Source→destination: counted, pausable.
	go func() {
		defer wg.Done()
		buf := make([]byte, 256*1024)
		for {
			n, rerr := tc.Read(buf)
			if n > 0 {
				total := atomic.AddInt64(&p.relayed, int64(n))
				if _, werr := out.Write(buf[:n]); werr != nil {
					return
				}
				if p.threshold > 0 && total >= p.threshold && !p.heldOnce() {
					p.hold()
					return // held; Close() tears the connection down
				}
			}
			if rerr != nil {
				return
			}
		}
	}()
	wg.Wait()
	_ = tc.Close()
	_ = out.Close()
}

var proxyTargetMu sync.Mutex
var proxyTarget string

// SetDstAddr points the relay at the destination agent's real listener.
func SetDstAddr(addr string) {
	proxyTargetMu.Lock()
	defer proxyTargetMu.Unlock()
	proxyTarget = addr
}

func (p *Proxy) dstAddr() string {
	proxyTargetMu.Lock()
	defer proxyTargetMu.Unlock()
	return proxyTarget
}

func (p *Proxy) heldOnce() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.held
}

func (p *Proxy) hold() {
	p.mu.Lock()
	p.held = true
	p.mu.Unlock()
	<-p.holdCh
}

// NodeCertPEM reads an agent's enrolled node certificate and key.
func NodeCertPEM(t *testing.T, a *Agent) (cert, key []byte) {
	t.Helper()
	cert, err := os.ReadFile(a.cfg.StateRoot + "/node.cert.pem")
	if err != nil {
		t.Fatalf("node cert: %v", err)
	}
	key, err = os.ReadFile(a.cfg.StateRoot + "/node.key.pem")
	if err != nil {
		t.Fatalf("node key: %v", err)
	}
	return cert, key
}

// CAPEM reads the controller CA certificate PEM.
func CAPEM(t *testing.T, s *Server) []byte {
	t.Helper()
	b, err := os.ReadFile(s.Cfg.CACertPath())
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	return b
}
