// Package agent is the Local Model Works node agent: it enrolls with the
// controller over a pinned-CA TLS connection, keeps one bidirectional
// control session, runs workload containers through the container runtime,
// samples hardware telemetry, reports cache placements, and serves peer
// artifact transfers on the fabric address.
package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"sync"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"github.com/jj-link/local-model-works/internal/ca"
	"github.com/jj-link/local-model-works/internal/config"
	"github.com/jj-link/local-model-works/internal/hardware"
	"github.com/jj-link/local-model-works/internal/runtime"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
	agentv1connect "github.com/jj-link/local-model-works/proto/agent/v1/agentv1connect"
)

// RotationThreshold is when the agent asks the controller for a renewed
// certificate (90-day lifetime, rotate at 30 days remaining).
const RotationThreshold = 30 * 24 * time.Hour

// Agent is one enrolled node's controller-facing state.
type Agent struct {
	cfg     config.Agent
	version string
	commit  string

	rt  runtime.Runtime
	acc hardware.Driver // accelerator source (NVML); host telemetry is built in
	*workloads
	dockerVersion string
	dockerOK      bool
	startTime     time.Time

	// Identity: created at enrollment, persisted under the state root.
	mu          sync.Mutex
	nodeID      string
	caPEM       []byte // CA certificate captured at bootstrap
	caPub       *ecdsa.PublicKey
	caPersisted bool

	// certMu guards the live node keypair; new TLS connections pick it up.
	certMu   sync.RWMutex
	nodeCert *tls.Certificate

	// sendQ carries agent→server messages that are event-driven (results,
	// state updates, log chunks, placements, progress). The session send
	// loop is the only stream writer.
	sendQ chan *agentv1.AgentMessage
}

// New builds an Agent. drv selects the accelerator driver (nil = NVIDIA).
func New(cfg config.Agent, version, commit string, rt runtime.Runtime, drv hardware.Driver) *Agent {
	if drv == nil {
		drv = hardware.NewNvidia()
	}
	a := &Agent{
		cfg:       cfg,
		version:   version,
		commit:    commit,
		rt:        rt,
		acc:       drv,
		startTime: time.Now(),
		sendQ:     make(chan *agentv1.AgentMessage, 256),
	}
	a.workloads = newWorkloads(a)
	return a
}

// Run is the agent's whole lifetime: enroll once, then keep the session
// alive with bounded backoff until the context is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	dirs := []string{
		a.cfg.StateRoot, a.cfg.CADir(), a.cfg.TransferDir(), a.cfg.LogDir(), a.cfg.Workspace,
	}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("state dir %s: %w", d, err)
		}
	}
	if err := a.loadIdentity(); err != nil {
		return err
	}
	if a.NodeID() == "" {
		if a.cfg.Token == "" {
			return errors.New("not enrolled and no LMW_AGENT_TOKEN set; run lmw-agent install first")
		}
		if err := a.enroll(ctx); err != nil {
			return fmt.Errorf("enroll: %w", err)
		}
	}
	if err := a.loadIdentity(); err != nil {
		return err
	}
	if err := a.loadNodeCert(); err != nil {
		return err
	}

	if ts, err := a.newTransferService(ctx); err != nil {
		log.Printf("agent: peer transfer listener: %v", err)
	} else {
		defer ts.stop()
	}

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := a.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errCleanClose) {
			backoff = time.Second
			log.Printf("agent: session closed by controller; reconnecting in %s", backoff)
		} else {
			log.Printf("agent: session ended (%v); reconnecting in %s", err, backoff)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// NodeID is the enrolled node identity ("" before enrollment).
func (a *Agent) NodeID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.nodeID
}

// ---------------------------------------------------------------------------
// Identity persistence
// ---------------------------------------------------------------------------

func (a *Agent) idPath() string { return a.cfg.StateRoot + "/agent.id.json" }

type identity struct {
	NodeID string `json:"node_id"`
}

func (a *Agent) loadIdentity() error {
	raw, err := os.ReadFile(a.idPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var id identity
	if err := json.Unmarshal(raw, &id); err != nil {
		return fmt.Errorf("agent.id.json: %w", err)
	}
	if id.NodeID == "" {
		return errors.New("agent.id.json: empty node id")
	}
	a.mu.Lock()
	a.nodeID = id.NodeID
	a.mu.Unlock()
	return nil
}

func (a *Agent) saveIdentity() error {
	a.mu.Lock()
	nodeID := a.nodeID
	a.mu.Unlock()
	if nodeID == "" {
		return nil
	}
	raw, err := json.Marshal(identity{NodeID: nodeID})
	if err != nil {
		return err
	}
	return os.WriteFile(a.idPath(), raw, 0o600)
}

func (a *Agent) caPEMPath() string { return a.cfg.CADir() + "/ca.cert.pem" }

// loadNodeCert loads the enrolled node keypair and the captured CA (whose
// public key verifies peer transfer credentials).
func (a *Agent) loadNodeCert() error {
	certPEM, err := os.ReadFile(a.cfg.NodeCertPath())
	if err != nil {
		return fmt.Errorf("node certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(a.cfg.NodeKeyPath())
	if err != nil {
		return fmt.Errorf("node key: %w", err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("node keypair: %w", err)
	}
	a.certMu.Lock()
	a.nodeCert = &pair
	a.certMu.Unlock()

	caRaw, err := os.ReadFile(a.caPEMPath())
	if err != nil {
		return fmt.Errorf("CA certificate missing at %s: re-enroll required", a.caPEMPath())
	}
	a.mu.Lock()
	a.caPEM = caRaw
	if c, err := ca.ParseCertPEM(caRaw); err == nil {
		if pub, ok := c.PublicKey.(*ecdsa.PublicKey); ok {
			a.caPub = pub
		}
	}
	a.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// TLS construction
// ---------------------------------------------------------------------------

// serverHost is the controller hostname for TLS verification.
func (a *Agent) serverHost() (string, error) {
	u, err := url.Parse(a.cfg.ServerURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", fmt.Errorf("LMW_AGENT_SERVER must be an https URL, got %q", a.cfg.ServerURL)
	}
	return u.Hostname(), nil
}

// clientTransport returns an HTTP/2 transport to the controller. Before the
// CA has been captured, the connection uses fingerprint-only bootstrap
// verification and persists the CA from the server's TLS chain.
func (a *Agent) clientTransport() (*http2.Transport, error) {
	a.mu.Lock()
	haveCA := len(a.caPEM) > 0
	caPEM := a.caPEM
	a.mu.Unlock()
	fp := a.cfg.CASha256

	var clientCert, clientKey []byte
	a.certMu.RLock()
	if pair := a.nodeCert; pair != nil {
		clientCert = concatPEM(pair.Certificate)
		if k, err := keyPEM(pair); err == nil {
			clientKey = k
		}
	}
	a.certMu.RUnlock()

	var tlsCfg *tls.Config
	var err error
	if haveCA {
		tlsCfg, err = ca.ClientTLSConfig(caPEM, fp, clientCert, clientKey)
	} else {
		host, herr := a.serverHost()
		if herr != nil {
			return nil, herr
		}
		var sink []byte
		tlsCfg, err = ca.BootstrapConfig(host, fp, &sink)
		if err != nil {
			return nil, err
		}
		orig := tlsCfg.VerifyConnection
		tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if err := orig(cs); err != nil {
				return err
			}
			// The verifier captured the CA from the chain; persist it
			// once so later connections verify normally.
			a.mu.Lock()
			defer a.mu.Unlock()
			if len(sink) > 0 && !a.caPersisted {
				a.caPEM = sink
				a.caPersisted = true
				if pub, ok := caPubFromPEM(sink); ok {
					a.caPub = pub
				}
				if werr := os.WriteFile(a.caPEMPath(), sink, 0o644); werr != nil {
					log.Printf("agent: persist CA: %v", werr)
				}
			}
			return nil
		}
	}
	if err != nil {
		return nil, err
	}
	return &http2.Transport{
		TLSClientConfig: tlsCfg,
		AllowHTTP:       false,
	}, nil
}

// sessionClient builds the Connect client for the control channel.
func (a *Agent) sessionClient(ctx context.Context) agentv1connect.AgentServiceClient {
	tr, err := a.clientTransport()
	if err != nil {
		log.Printf("agent: transport: %v", err)
		return nil
	}
	return agentv1connect.NewAgentServiceClient(httpClient(tr), a.cfg.ServerURL, connect.WithGRPC())
}

// certRemaining returns how long the current node certificate is valid.
func (a *Agent) certRemaining() time.Duration {
	a.certMu.RLock()
	pair := a.nodeCert
	a.certMu.RUnlock()
	if pair == nil || len(pair.Certificate) == 0 {
		return 0
	}
	block, _ := pem.Decode(concatPEM(pair.Certificate))
	if block == nil {
		return 0
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return 0
	}
	return time.Until(cert.NotAfter)
}

// ---------------------------------------------------------------------------
// Outbound messages
// ---------------------------------------------------------------------------

// send queues one agent→server message; it drops (with a log line) rather
// than block the producer when the session is wedged.
func (a *Agent) send(m *agentv1.AgentMessage) {
	select {
	case a.sendQ <- m:
	default:
		log.Printf("agent: send queue full; dropping %T", m.GetBody())
	}
}
