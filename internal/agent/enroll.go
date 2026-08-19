package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"github.com/jj-link/local-model-works/internal/ca"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
	agentv1connect "github.com/jj-link/local-model-works/proto/agent/v1/agentv1connect"
)

// enroll performs the first contact with the controller: bootstrap-verified
// TLS to the pinned CA fingerprint, one-time token exchange for a node
// client certificate (90 days), and identity persistence.
func (a *Agent) enroll(ctx context.Context) error {
	// The node keypair is generated once and kept for the node's lifetime;
	// certificate rotation later issues a new serial for the same key.
	keyPath := a.cfg.StateRoot + "/node.key.pem"
	var pubPEM, keyPEM []byte
	if raw, err := os.ReadFile(keyPath); err == nil {
		keyPEM = raw
		priv, err := ca.ParseKeyPEM(keyPEM)
		if err != nil {
			return fmt.Errorf("existing node key: %w", err)
		}
		pubPEM, err = ca.SPKIPEM(&priv.PublicKey)
		if err != nil {
			return fmt.Errorf("derive node public key: %w", err)
		}
	} else if os.IsNotExist(err) {
		var err2 error
		pubPEM, keyPEM, err2 = ca.NewKeyPEMs()
		if err2 != nil {
			return err2
		}
		if werr := os.WriteFile(keyPath, keyPEM, 0o600); werr != nil {
			return fmt.Errorf("write node key: %w", werr)
		}
	} else {
		return err
	}

	host, err := a.serverHost()
	if err != nil {
		return err
	}
	// Fingerprint-only verification; the CA is captured from the server's
	// TLS chain during the handshake and persisted after the RPC succeeds.
	var sink []byte
	tlsCfg, err := ca.BootstrapConfig(host, a.cfg.CASha256, &sink)
	if err != nil {
		return err
	}
	tr := &http2.Transport{TLSClientConfig: tlsCfg, AllowHTTP: false}
	client := agentv1connect.NewEnrollmentServiceClient(httpClient(tr), a.cfg.ServerURL, connect.WithGRPC())

	hostname, _ := os.Hostname()
	resp, err := client.Enroll(ctx, connect.NewRequest(&agentv1.EnrollRequest{
		Token: a.cfg.Token,
		Agent: &agentv1.AgentInfo{
			Version:   a.version,
			Hostname:  hostname,
			Os:        runtime.GOOS,
			Arch:      runtime.GOARCH,
			AgentName: hostname,
		},
		CaFingerprint: a.cfg.CASha256,
		PublicKeyPem:  string(pubPEM),
	}))
	if err != nil {
		return fmt.Errorf("enroll RPC: %w", err)
	}
	body := resp.Msg
	nodeID := body.GetNodeId()
	if nodeID == "" {
		return fmt.Errorf("enroll response missing node_id")
	}
	if len(sink) == 0 {
		return fmt.Errorf("enroll succeeded but the server did not present its CA certificate")
	}
	if werr := os.WriteFile(a.caPEMPath(), sink, 0o644); werr != nil {
		return fmt.Errorf("persist CA: %w", werr)
	}
	if werr := os.WriteFile(a.cfg.NodeCertPath(), []byte(body.GetNodeCertificatePem()), 0o644); werr != nil {
		return fmt.Errorf("persist node cert: %w", werr)
	}
	a.mu.Lock()
	a.caPEM = sink
	a.caPersisted = true
	if pub, ok := caPubFromPEM(sink); ok {
		a.caPub = pub
	}
	a.nodeID = nodeID
	a.mu.Unlock()
	if err := a.saveIdentity(); err != nil {
		return err
	}
	exp := ""
	if t := body.GetCertificateExpiresAt(); t != nil {
		exp = t.AsTime().Format(time.RFC3339)
	}
	log.Printf("agent: enrolled as %s (CA sha256=%s, cert expires %s)", nodeID, a.cfg.CASha256, exp)
	return nil
}
