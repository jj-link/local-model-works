package agonrunner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var sshToken = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
var sshHostname = regexp.MustCompile(`^[A-Za-z0-9.:-]+$`)

func preflightSSH(ctx context.Context, config projectConfig, scratch string) error {
	secretName, _ := config.Input["ssh_secret_name"].(string)
	if secretName == "" {
		return nil
	}
	if len(config.Worker.SSHHosts) == 0 {
		return errors.New("autoresearch.ssh_hosts_missing")
	}
	keyPath, err := credentialPath(secretName)
	if err != nil {
		return err
	}
	sshDirectory := filepath.Join(scratch, "ssh")
	if err := os.MkdirAll(sshDirectory, 0o700); err != nil {
		return err
	}
	configPath := filepath.Join(sshDirectory, "config")
	knownHosts := filepath.Join(sshDirectory, "known_hosts")
	var contents strings.Builder
	for _, host := range config.Worker.SSHHosts {
		if !sshToken.MatchString(host.Alias) || !sshToken.MatchString(host.User) || !sshHostname.MatchString(host.Hostname) {
			return errors.New("autoresearch.ssh_host_invalid")
		}
		if parsed := net.ParseIP(strings.Trim(host.Hostname, "[]")); parsed != nil && (parsed.IsLoopback() || parsed.IsPrivate() || parsed.IsLinkLocalUnicast()) {
			// Tailscale 100.64/10 addresses are not classified Private by net.IP;
			// ordinary private/loopback addresses remain forbidden.
			return errors.New("autoresearch.ssh_host_private")
		}
		fmt.Fprintf(&contents, "Host %s\n  HostName %s\n  User %s\n  IdentityFile %s\n  IdentitiesOnly yes\n\n", host.Alias, host.Hostname, host.User, keyPath)
	}
	if err := os.WriteFile(configPath, []byte(contents.String()), 0o600); err != nil {
		return err
	}
	for _, host := range config.Worker.SSHHosts {
		probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		command := exec.CommandContext(probeCtx, "ssh", "-F", configPath, "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile="+knownHosts, host.Alias, "true")
		output, err := command.CombinedOutput()
		cancel()
		if err != nil {
			return fmt.Errorf("autoresearch.ssh_preflight_failed: %s: %w: %s", host.Alias, err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}
