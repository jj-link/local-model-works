// Package workerssh resolves and checks worker SSH destinations in the agent's
// own account and environment. It never installs keys or changes host trust.
package workerssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const ProtocolFeature = "worker-ssh-v1"

var errProxyConfiguration = errors.New("SSH proxy configuration is not supported by read-only worker checks")

type Request struct {
	Addresses   []string
	Username    string
	Target      string
	ResolveOnly bool
}

type Result struct {
	Target string `json:"target"`
	// Hostname is the first supplied enrolled-worker address verified against
	// the effective SSH hostname; Target retains the actual alias/destination.
	Hostname string `json:"hostname"`
	Username string `json:"username"`
}

type checker struct {
	roots  []configRoot
	home   string
	run    func(context.Context, []string) ([]byte, error)
	lookup func(context.Context, string) ([]netip.Addr, error)
}

// Check resolves an automatic default or checks exactly the supplied destination.
// Remote execution is restricted to the fixed command true, and only after the
// effective host has been matched against this enrolled worker's addresses.
func Check(ctx context.Context, request Request) (Result, error) {
	if err := validateRequest(request); err != nil {
		return Result{}, err
	}
	roots, home, err := configRoots()
	if err != nil {
		return Result{}, err
	}
	return (checker{roots: roots, home: home, run: runSSH, lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}}).check(ctx, request)
}

func (c checker) check(ctx context.Context, request Request) (Result, error) {
	if err := validateRequest(request); err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	aliases, err := discoverAliases(c.roots, c.home)
	if err != nil {
		return Result{}, err
	}
	allowed := make(map[netip.Addr]int)
	for index, address := range request.Addresses {
		ips, err := c.addresses(ctx, address)
		if err != nil {
			return Result{}, fmt.Errorf("resolve enrolled worker address %q: %w", address, err)
		}
		for _, ip := range ips {
			if _, exists := allowed[ip.Unmap()]; !exists {
				allowed[ip.Unmap()] = index
			}
		}
	}
	matches := func(host string) (string, error) {
		ips, err := c.addresses(ctx, host)
		if err != nil {
			return "", err
		}
		preferred := len(request.Addresses)
		for _, ip := range ips {
			index, ok := allowed[ip.Unmap()]
			if !ok {
				return "", nil
			}
			if index < preferred {
				preferred = index
			}
		}
		if preferred == len(request.Addresses) {
			return "", nil
		}
		return request.Addresses[preferred], nil
	}
	if request.Target == "" {
		for _, alias := range aliases {
			resolved, configErr := c.effective(ctx, alias)
			if configErr != nil && !errors.Is(configErr, errProxyConfiguration) {
				return Result{}, configErr
			}
			if resolved.Username != request.Username {
				continue
			}
			endpoint, err := matches(resolved.Hostname)
			if ctx.Err() != nil {
				return Result{}, fmt.Errorf("resolve worker SSH alias: %w", ctx.Err())
			}
			// A configured alias for an unrelated, unavailable DNS host is not
			// a worker candidate. A matching alias must support read-only checks.
			if err == nil && endpoint != "" {
				if configErr != nil {
					return Result{}, configErr
				}
				resolved.Hostname = endpoint
				return resolved, nil
			}
		}
		request.Target = request.Username + "@" + request.Addresses[0]
	}
	resolved, err := c.effective(ctx, request.Target)
	if err != nil {
		return Result{}, err
	}
	endpoint, err := matches(resolved.Hostname)
	if err != nil {
		return Result{}, fmt.Errorf("resolve SSH target %q: %w", request.Target, err)
	}
	if endpoint == "" {
		return Result{}, fmt.Errorf("SSH target %q resolves to %q, which is not an address of the selected enrolled worker", request.Target, resolved.Hostname)
	}
	resolved.Hostname = endpoint
	if request.ResolveOnly {
		return resolved, nil
	}
	probeCtx, stop := context.WithTimeout(ctx, 12*time.Second)
	defer stop()
	args := append(readOnlyOptions(), "-oProxyCommand=none", "-oProxyJump=none", "-T", "-n", "--", request.Target, "true")
	if _, err := c.run(probeCtx, args); err != nil {
		return Result{}, fmt.Errorf("worker SSH check for %q failed (existing keys and trusted host entry required in the head agent account): %w", request.Target, err)
	}
	return resolved, nil
}

func (c checker) addresses(ctx context.Context, host string) ([]netip.Addr, error) {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip}, nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ips, err := c.lookup(lookupCtx, host)
	if err == nil && len(ips) == 0 {
		err = fmt.Errorf("DNS returned no addresses")
	}
	if len(ips) > 64 {
		return nil, fmt.Errorf("DNS returned too many addresses")
	}
	return ips, err
}

func (c checker) effective(ctx context.Context, target string) (Result, error) {
	configCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// -G does not connect. Do not override proxy settings here: report those
	// as unsupported rather than silently checking a different transport.
	args := append(readOnlyOptions(), "-G", "-T", "-n", "--", target, "true")
	out, err := c.run(configCtx, args)
	if err != nil {
		return Result{}, fmt.Errorf("read effective SSH configuration for %q: %w", target, err)
	}
	result := Result{Target: target}
	port := 0
	proxy := ""
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(key) {
		case "hostname":
			result.Hostname = value
		case "user":
			result.Username = value
		case "port":
			port, _ = strconv.Atoi(value)
		case "proxycommand", "proxyjump":
			if value != "none" && value != "" {
				proxy = key
			}
		}
	}
	if !validHost(result.Hostname) || !validUser(result.Username) || port < 1 || port > 65535 {
		return Result{}, fmt.Errorf("SSH target %q has an invalid effective hostname, username or port", target)
	}
	if proxy != "" {
		return result, fmt.Errorf("%w: target %q configures %s", errProxyConfiguration, target, proxy)
	}
	return result, nil
}

// These command-line settings take precedence over configuration. IdentityFile,
// IdentitiesOnly, User, HostName and Port remain native.
func readOnlyOptions() []string {
	return []string{
		"-oBatchMode=yes", "-oStrictHostKeyChecking=yes", "-oUpdateHostKeys=no",
		"-oVerifyHostKeyDNS=no", "-oNoHostAuthenticationForLocalhost=no",
		"-oAddKeysToAgent=no", "-oForwardAgent=no", "-oForwardX11=no",
		"-oClearAllForwardings=yes", "-oTunnel=no", "-oPermitLocalCommand=no",
		"-oRemoteCommand=none", "-oKnownHostsCommand=none",
		"-oControlMaster=no", "-oControlPath=none", "-oControlPersist=no",
		"-oRequestTTY=no", "-oForkAfterAuthentication=no", "-oStdinNull=yes",
		"-oSessionType=default", "-oConnectionAttempts=1", "-oConnectTimeout=8",
		"-oServerAliveInterval=3", "-oServerAliveCountMax=1",
		"-oGSSAPIDelegateCredentials=no",
		"-oPKCS11Provider=none", "-oSecurityKeyProvider=internal",
		"-oHostbasedAuthentication=no", "-oEnableSSHKeysign=no",
	}
}

func validateRequest(request Request) error {
	if len(request.Addresses) == 0 || len(request.Addresses) > 64 {
		return fmt.Errorf("worker SSH requires between 1 and 64 enrolled worker addresses")
	}
	for _, address := range request.Addresses {
		if !validHost(address) {
			return fmt.Errorf("invalid enrolled worker SSH address")
		}
	}
	if request.Username != "" && !validUser(request.Username) {
		return fmt.Errorf("invalid worker SSH username")
	}
	if request.Target == "" {
		if !request.ResolveOnly || !validUser(request.Username) {
			return fmt.Errorf("automatic worker SSH resolution requires a username and resolve_only; a connection check requires an exact target")
		}
		return nil
	}
	if len(request.Target) > 512 {
		return fmt.Errorf("worker SSH target exceeds length limit")
	}
	parts := strings.Split(request.Target, "@")
	if len(parts) > 2 || !validHost(parts[len(parts)-1]) || (len(parts) == 2 && !validUser(parts[0])) {
		return fmt.Errorf("invalid worker SSH target; use an SSH alias or user@host without options or shell syntax")
	}
	return nil
}

func validUser(value string) bool {
	return len(value) <= 128 && safeName(value, false)
}

func validHost(value string) bool {
	if value == "" || len(value) > 253 {
		return false
	}
	if strings.HasPrefix(value, "[") != strings.HasSuffix(value, "]") {
		return false
	}
	plain := strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	if ip, err := netip.ParseAddr(plain); err == nil {
		return ip.Zone() == "" || safeName(ip.Zone(), false)
	}
	return safeName(value, true)
}

func safeName(value string, trailingDot bool) bool {
	if value == "" || value[0] == '-' || value[0] == '.' {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return trailingDot || value[len(value)-1] != '.'
}

type cappedOutput struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.overflow = true
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func runSSH(ctx context.Context, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.WaitDelay = time.Second
	stdout := cappedOutput{limit: 256 << 10}
	stderr := cappedOutput{limit: 8 << 10}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if stdout.overflow || stderr.overflow {
		return nil, fmt.Errorf("SSH output exceeded safety limit")
	}
	if err != nil {
		detail := strings.Map(func(r rune) rune {
			if r < 32 || r == 127 {
				return ' '
			}
			return r
		}, stderr.String())
		if len(detail) > 2048 {
			detail = detail[:2048] + " (truncated)"
		}
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(detail))
	}
	return stdout.Bytes(), nil
}
