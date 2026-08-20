package main

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jj-link/local-model-works/internal/config"
)

var (
	hex64       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	serviceUser = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`)
)

type cacheRoots []string

func (r *cacheRoots) String() string { return strings.Join(*r, ":") }
func (r *cacheRoots) Set(value string) error {
	if !filepath.IsAbs(value) || strings.ContainsAny(value, ":\r\n") {
		return fmt.Errorf("cache root must be an absolute path without ':'")
	}
	*r = append(*r, filepath.Clean(value))
	return nil
}

func install(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("lmw-agent install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var roots cacheRoots
	serverURL := fs.String("server", "", "controller mTLS URL")
	caSHA := fs.String("ca-sha256", "", "controller CA SHA-256 fingerprint")
	token := fs.String("token", "", "one-use enrollment token")
	runAs := fs.String("run-as", "", "systemd service user")
	peerAddr := fs.String("peer-addr", ":9444", "peer transfer listen address")
	peerAdvertise := fs.String("peer-advertise", "", "routable peer transfer address")
	dockerSocket := fs.String("docker-socket", "/var/run/docker.sock", "Docker socket")
	fs.Var(&roots, "cache-root", "existing model/cache root (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	parsed, err := url.Parse(*serverURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("--server must be an absolute HTTPS URL without a path")
	}
	if !hex64.MatchString(*caSHA) {
		return fmt.Errorf("--ca-sha256 must be 64 lowercase hex characters")
	}
	if !hex64.MatchString(*token) {
		return fmt.Errorf("--token must be a 64-character one-use token")
	}
	if !serviceUser.MatchString(*runAs) {
		return fmt.Errorf("--run-as must be a valid service user")
	}
	for _, value := range []string{*peerAddr, *peerAdvertise, *dockerSocket} {
		if strings.ContainsAny(value, "\r\n \t") {
			return fmt.Errorf("installer values must not contain whitespace")
		}
	}

	configDir := os.Getenv(config.EnvConfigDir)
	if configDir == "" {
		configDir = "/etc/local-model-works"
	}
	stateRoot := os.Getenv(config.EnvAgentStateRoot)
	if stateRoot == "" {
		stateRoot = "/var/lib/local-model-works-agent"
	}
	workspace := filepath.Join(stateRoot, "workspace")
	for _, dir := range []string{configDir, stateRoot, workspace, filepath.Join(stateRoot, "ca"), filepath.Join(stateRoot, "transfers"), filepath.Join(stateRoot, "logs")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	env := []string{
		config.EnvAgentServer + "=" + strings.TrimSuffix(*serverURL, "/"),
		config.EnvAgentCASha256 + "=" + *caSHA,
		config.EnvAgentToken + "=" + *token,
		config.EnvAgentStateRoot + "=" + stateRoot,
		config.EnvAgentWorkspace + "=" + workspace,
		config.EnvAgentDockerSock + "=" + *dockerSocket,
		config.EnvAgentCacheRoots + "=" + strings.Join(roots, ":"),
		config.EnvPeerAddr + "=" + *peerAddr,
		config.EnvPeerAdvertise + "=" + *peerAdvertise,
	}
	if err := os.WriteFile(filepath.Join(configDir, "agent.env"), []byte(strings.Join(env, "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("write agent environment: %w", err)
	}

	unitSource, err := locateAgentUnit()
	if err != nil {
		return err
	}
	unit, err := os.ReadFile(unitSource)
	if err != nil {
		return fmt.Errorf("read agent unit: %w", err)
	}
	systemdRoot := os.Getenv("LMW_SYSTEMD_ROOT")
	if systemdRoot == "" {
		systemdRoot = "/etc/systemd/system"
	}
	if err := os.MkdirAll(systemdRoot, 0o755); err != nil {
		return fmt.Errorf("create systemd root: %w", err)
	}
	if err := os.WriteFile(filepath.Join(systemdRoot, "local-model-works-agent.service"), unit, 0o644); err != nil {
		return fmt.Errorf("write agent unit: %w", err)
	}
	dropIn := filepath.Join(systemdRoot, "local-model-works-agent.service.d")
	if err := os.MkdirAll(dropIn, 0o755); err != nil {
		return fmt.Errorf("create agent unit drop-in: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dropIn, "10-run-as.conf"), []byte("[Service]\nUser="+*runAs+"\n"), 0o644); err != nil {
		return fmt.Errorf("write agent user drop-in: %w", err)
	}
	fmt.Fprintf(stdout, "installed local-model-works-agent.service for %s\n", *runAs)
	return nil
}

func locateAgentUnit() (string, error) {
	if configured := os.Getenv("LMW_AGENT_UNIT_SOURCE"); configured != "" {
		return configured, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	root := filepath.Dir(executable)
	candidates := []string{
		filepath.Join(root, "deploy", "systemd", "local-model-works-agent.service"),
		filepath.Join(root, "..", "share", "local-model-works", "local-model-works-agent.service"),
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("agent systemd unit not found beside release bundle or under /usr/local/share/local-model-works")
}
