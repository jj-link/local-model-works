// Package config resolves Local Model Works settings from LMW_ environment
// variables with sane single-operator defaults.
package config

import (
	"fmt"
	"net"
	"os"
	"time"
)

const (
	EnvHTTPAddr  = "LMW_HTTP_ADDR"
	EnvAgentAddr = "LMW_AGENT_ADDR"
	EnvPeerAddr  = "LMW_PEER_ADDR"
	// EnvPeerAdvertise is the routable host:port a remote source dials;
	// empty falls back to PeerAddr when it names an explicit host.
	EnvPeerAdvertise = "LMW_PEER_ADVERTISE"
	EnvServerName    = "LMW_SERVER_NAME"
	EnvStateRoot     = "LMW_STATE_ROOT"
	EnvConfigDir     = "LMW_CONFIG_DIR"
	EnvSessionTTL    = "LMW_SESSION_TTL"
	EnvTrustKey      = "LMW_TRUST_KEY_PEM"
	EnvAdminPassword = "LMW_ADMIN_PASSWORD"

	EnvAgentServer     = "LMW_AGENT_SERVER"
	EnvAgentCASha256   = "LMW_AGENT_CA_SHA256"
	EnvAgentToken      = "LMW_AGENT_TOKEN"
	EnvAgentStateRoot  = "LMW_AGENT_STATE_ROOT"
	EnvAgentDockerSock = "LMW_AGENT_DOCKER_SOCKET"
	EnvAgentWorkspace  = "LMW_AGENT_WORKSPACE"
	EnvAgentCacheRoots = "LMW_AGENT_CACHE_ROOTS"
	EnvAgentTeleInt    = "LMW_AGENT_TELEMETRY_INTERVAL"
)

// Server holds controller-plane settings.
type Server struct {
	HTTPAddr    string        // browser/CLI listener, default 127.0.0.1:9000
	AgentAddr   string        // agent mTLS listener, default :9443
	PeerAddr    string        // peer-transfer port hint, default :9444
	StateRoot   string        // default /var/lib/local-model-works
	ConfigDir   string        // default /etc/local-model-works
	SessionTTL  time.Duration // default 12h
	ServerName  string        // TLS name for the server leaf cert, default localhost
	TrustKeyPEM string        // PEM public key for recipe/catalog signature verification
}

// Agent holds node-agent settings.
type Agent struct {
	ServerURL     string        // controller agent listener, e.g. https://<tailnet>:9443
	CASha256      string        // pinned CA fingerprint (hex)
	Token         string        // one-time enrollment token (first run only)
	PeerAddr      string        // peer-transfer listener, default :9444
	PeerAdvertise string        // routable peer host:port; "" = derive from PeerAddr
	StateRoot     string        // default /var/lib/local-model-works-agent
	DockerSocket  string        // default /var/run/docker.sock
	Workspace     string        // default <StateRoot>/workspace
	CacheRoots    []string      // existing model/cache roots reported as placements
	TelemetryInt  time.Duration // sample interval, default 1s
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// LoadServer reads controller settings.
func LoadServer() Server {
	return Server{
		HTTPAddr:    envStr(EnvHTTPAddr, "127.0.0.1:9000"),
		AgentAddr:   envStr(EnvAgentAddr, ":9443"),
		PeerAddr:    envStr(EnvPeerAddr, ":9444"),
		StateRoot:   envStr(EnvStateRoot, "/var/lib/local-model-works"),
		ConfigDir:   envStr(EnvConfigDir, "/etc/local-model-works"),
		SessionTTL:  sessionTTL(),
		ServerName:  envStr(EnvServerName, "localhost"),
		TrustKeyPEM: envStr(EnvTrustKey, ""),
	}
}

func sessionTTL() time.Duration {
	v := os.Getenv(EnvSessionTTL)
	if v == "" {
		return 12 * time.Hour
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 12 * time.Hour
	}
	return d
}

// LoadAgent reads node-agent settings.
func LoadAgent() (Agent, error) {
	a := Agent{
		ServerURL:     envStr(EnvAgentServer, ""),
		CASha256:      envStr(EnvAgentCASha256, ""),
		Token:         envStr(EnvAgentToken, ""),
		PeerAddr:      envStr(EnvPeerAddr, ":9444"),
		PeerAdvertise: envStr(EnvPeerAdvertise, ""),
		StateRoot:     envStr(EnvAgentStateRoot, "/var/lib/local-model-works-agent"),
		DockerSocket:  envStr(EnvAgentDockerSock, "/var/run/docker.sock"),
		Workspace:     envStr(EnvAgentWorkspace, ""),
		CacheRoots:    splitColonList(os.Getenv(EnvAgentCacheRoots)),
		TelemetryInt:  time.Second,
	}
	if a.Workspace == "" {
		a.Workspace = a.StateRoot + "/workspace"
	}
	if v := os.Getenv(EnvAgentTeleInt); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 100*time.Millisecond {
			return a, fmt.Errorf("LMW_AGENT_TELEMETRY_INTERVAL: invalid duration %q", v)
		}
		a.TelemetryInt = d
	}
	return a, nil
}

func splitColonList(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// Paths under a server state root.
func (s Server) DBPath() string        { return s.StateRoot + "/lmw.db" }
func (s Server) CAKeyPath() string     { return s.StateRoot + "/ca/ca.key.pem" }
func (s Server) CACertPath() string    { return s.StateRoot + "/ca/ca.cert.pem" }
func (s Server) SecretKeyPath() string { return s.StateRoot + "/secrets.key" }
func (s Server) RecipeRoot() string    { return s.StateRoot + "/recipes" }
func (s Server) RunRoot() string       { return s.StateRoot + "/runs" }

// TrustKeyPath is where the recipe/catalog verification public key (PEM
// PKIX, Ed25519) lives; missing file disables signature verification.
func (s Server) TrustKeyPath() string { return s.StateRoot + "/trust.key.pem" }

// CatalogRoot is the on-disk first-party catalog.
func (s Server) CatalogRoot() string { return s.StateRoot + "/catalog" }

// Paths under an agent state root.
func (a Agent) NodeCertPath() string { return a.StateRoot + "/node.cert.pem" }
func (a Agent) NodeKeyPath() string  { return a.StateRoot + "/node.key.pem" }
func (a Agent) CADir() string        { return a.StateRoot + "/ca" }
func (a Agent) TransferDir() string  { return a.StateRoot + "/transfers" }
func (a Agent) LogDir() string       { return a.StateRoot + "/logs" }

// PeerAdvertiseAddr is the routable peer-transfer address a remote node
// dials. Bind addresses (":9444", "0.0.0.0:9444") carry no host, so with no
// explicit advertise configured they yield "" (node cannot receive from
// peers until LMW_PEER_ADVERTISE is set).
func (a Agent) PeerAdvertiseAddr() string {
	if a.PeerAdvertise != "" {
		return a.PeerAdvertise
	}
	host, _, err := net.SplitHostPort(a.PeerAddr)
	if err != nil {
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		return ""
	}
	return a.PeerAddr
}
