package workerssh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func fixtureChecker(t *testing.T, config string) checker {
	t.Helper()
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH is required for native configuration regression tests")
	}
	home := t.TempDir()
	path := filepath.Join(home, "config")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return checker{
		roots: []configRoot{{path: path, base: home}}, home: home,
		run: func(ctx context.Context, args []string) ([]byte, error) {
			return runSSH(ctx, append([]string{"-F", path}, args...))
		},
		lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
			return nil, fmt.Errorf("unexpected DNS lookup for %s", host)
		},
	}
}

func TestConfiguredAliasWinsOverManagementIP(t *testing.T) {
	c := fixtureChecker(t, `Host unrelated-proxy
  HostName 192.0.2.99
  User worker
  ProxyCommand arbitrary-command %h %p
Host wrong-user
  HostName 192.0.2.42
  User somebody-else
Host wrong-worker
  HostName 192.0.2.43
  User worker
Host "configured-worker"
  HostName = 192.0.2.42
  User = "worker"
  IdentityFile ~/.ssh/worker_ed25519
  IdentitiesOnly yes
`)
	got, err := c.check(context.Background(), Request{
		Addresses: []string{"198.51.100.42", "192.0.2.42"}, Username: "worker", ResolveOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Target != "configured-worker" || got.Hostname != "192.0.2.42" || got.Username != "worker" {
		t.Fatalf("resolved destination = %+v; expected configured worker alias instead of management-IP default", got)
	}
}

func TestIncludedAliasesUseNativeOrdering(t *testing.T) {
	includes := filepath.Join(t.TempDir(), "with spaces=equals")
	if err := os.Mkdir(includes, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"20-second.conf":  "Host second\n HostName 192.0.2.42\n User worker\n",
		"10-first.conf":   "Host first\n HostName 192.0.2.42\n User worker\n IdentityFile ~/.ssh/specific_key\n",
		"x-excluded.conf": "Host excluded\n HostName 192.0.2.42\n User worker\n",
	} {
		if err := os.WriteFile(filepath.Join(includes, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pattern := filepath.ToSlash(filepath.Join(includes, "[!x]*.conf"))
	c := fixtureChecker(t, fmt.Sprintf("Include = %q\nHost * !excluded wildcard-*\n User worker\n", pattern))
	got, err := c.check(context.Background(), Request{Addresses: []string{"192.0.2.42"}, Username: "worker", ResolveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Target != "first" {
		t.Fatalf("resolved %q; expected first alias from lexically ordered includes", got.Target)
	}
}

func TestMismatchedAliasesDoNotReplaceFallback(t *testing.T) {
	c := fixtureChecker(t, `Host wrong-user
 HostName 192.0.2.42
 User other
Host wrong-host
 HostName 192.0.2.43
 User worker
`)
	got, err := c.check(context.Background(), Request{Addresses: []string{"192.0.2.42"}, Username: "worker", ResolveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Target != "worker@192.0.2.42" {
		t.Fatalf("resolved %q; mismatched aliases must not be selected", got.Target)
	}
}

func TestExplicitTargetPreservesAliasAndUserOverride(t *testing.T) {
	c := fixtureChecker(t, `Host automatic explicit
 HostName 192.0.2.42
 User worker
`)
	got, err := c.check(context.Background(), Request{
		Addresses: []string{"192.0.2.42"}, Username: "worker", Target: "administrator@explicit", ResolveOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Target != "administrator@explicit" || got.Username != "administrator" {
		t.Fatalf("explicit alias/user was replaced: %+v", got)
	}
}

func TestExplicitTargetCannotReachAnotherWorker(t *testing.T) {
	c := fixtureChecker(t, "Host elsewhere\n HostName 192.0.2.43\n User worker\n")
	_, err := c.check(context.Background(), Request{
		Addresses: []string{"192.0.2.42"}, Username: "worker", Target: "elsewhere", ResolveOnly: true,
	})
	if err == nil || !strings.Contains(err.Error(), "not an address of the selected enrolled worker") {
		t.Fatalf("wrong worker error = %v", err)
	}
}

func TestCanonicalDNSMustContainOnlyEnrolledAddresses(t *testing.T) {
	c := fixtureChecker(t, "Host worker-alias\n HostName worker.example\n User worker\n")
	c.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.0.2.42"), netip.MustParseAddr("192.0.2.43")}, nil
	}
	request := Request{Addresses: []string{"192.0.2.42"}, Target: "worker-alias", ResolveOnly: true}
	if _, err := c.check(context.Background(), request); err == nil {
		t.Fatal("mixed-worker DNS result was accepted")
	}
	request.Addresses = append(request.Addresses, "192.0.2.43")
	got, err := c.check(context.Background(), request)
	if err != nil {
		t.Fatalf("all enrolled DNS addresses should match: %v", err)
	}
	if got.Hostname != "192.0.2.42" {
		t.Fatalf("verified endpoint = %q; expected first enrolled address rather than unverifiable DNS name", got.Hostname)
	}
}

func TestUnsafeRequestsRejectedBeforeSSH(t *testing.T) {
	requests := []Request{
		{Addresses: []string{"192.0.2.42"}, Target: "-oProxyCommand=evil"},
		{Addresses: []string{"192.0.2.42"}, Target: "worker@host;evil"},
		{Addresses: []string{"192.0.2.42"}, Target: "worker@$(evil)"},
		{Addresses: []string{"192.0.2.42"}, Target: "worker@host\nevil"},
		{Addresses: []string{"192.0.2.42"}, Target: "-worker@host"},
		{Addresses: []string{"192.0.2.42"}, Target: "worker@[::1"},
		{Addresses: []string{"192.0.2.42"}, Username: "worker;evil", ResolveOnly: true},
		{Addresses: []string{"-oPort=22"}, Username: "worker", ResolveOnly: true},
		{Addresses: []string{"192.0.2.42"}, Username: "worker"},
	}
	for _, request := range requests {
		if _, err := (checker{}).check(context.Background(), request); err == nil {
			t.Fatalf("unsafe request accepted: %+v", request)
		}
	}
}

func TestNativeSSHFailureNeverReturnsSuccess(t *testing.T) {
	c := fixtureChecker(t, "Host worker-alias\n HostName 192.0.2.42\n User worker\n")
	native := c.run
	c.run = func(ctx context.Context, args []string) ([]byte, error) {
		if slices.Contains(args, "-G") {
			return native(ctx, args)
		}
		return nil, errors.New("Permission denied (publickey)")
	}
	result, err := c.check(context.Background(), Request{Addresses: []string{"192.0.2.42"}, Target: "worker-alias"})
	if err == nil || !strings.Contains(err.Error(), "Permission denied") || result != (Result{}) {
		t.Fatalf("failed authentication returned result=%+v error=%v", result, err)
	}
}

func TestProbeHonorsCallerDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// This listener never sends an SSH greeting. A real ssh process must be
	// canceled, not left waiting on a stalled worker or converted to success.
	port := listener.Addr().(*net.TCPAddr).Port
	c := fixtureChecker(t, fmt.Sprintf("Host stalled\n HostName 127.0.0.1\n User worker\n Port %d\n", port))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err = c.check(ctx, Request{Addresses: []string{"127.0.0.1"}, Target: "stalled"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled SSH error = %v; expected caller deadline", err)
	}
}

func TestReadOnlyChecksRejectExecutableConfiguration(t *testing.T) {
	for name, config := range map[string]string{
		"match-exec":    "Match exec \"exit 0\"\n User worker\n",
		"proxy-command": "Host worker\n HostName 192.0.2.42\n User worker\n ProxyCommand arbitrary-command %h %p\n",
		"proxy-jump":    "Host worker\n HostName 192.0.2.42\n User worker\n ProxyJump jump-host\n",
		"token-include": "Include %h.conf\n",
	} {
		t.Run(name, func(t *testing.T) {
			c := fixtureChecker(t, config)
			marker := filepath.Join(c.home, "executed")
			if name == "match-exec" {
				config = fmt.Sprintf("Match exec \"touch '%s'\"\n User worker\n", marker)
				if err := os.WriteFile(c.roots[0].path, []byte(config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := c.check(context.Background(), Request{Addresses: []string{"192.0.2.42"}, Target: "worker", ResolveOnly: true})
			if err == nil {
				t.Fatalf("executable/unenumerable configuration error = %v", err)
			}
			if strings.HasPrefix(name, "proxy-") && !errors.Is(err, errProxyConfiguration) {
				t.Fatalf("proxy configuration was not rejected before the connection: %v", err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("configuration executed a command: %v", err)
			}
		})
	}
}

func TestIncludeCyclesAreRejected(t *testing.T) {
	c := fixtureChecker(t, "")
	if err := os.WriteFile(c.roots[0].path, []byte(fmt.Sprintf("Include %q\n", filepath.ToSlash(c.roots[0].path))), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := c.check(context.Background(), Request{Addresses: []string{"192.0.2.42"}, Username: "worker", ResolveOnly: true})
	if err == nil || !strings.Contains(err.Error(), "include cycle") {
		t.Fatalf("cyclic include error = %v", err)
	}
}

func TestIncludedHashFilenameIsNotMistakenForComment(t *testing.T) {
	path := filepath.ToSlash(filepath.Join(t.TempDir(), "worker#config"))
	if err := os.WriteFile(path, []byte("Match exec \"exit 0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := fixtureChecker(t, "Include "+path+"\n")
	_, err := c.check(context.Background(), Request{Addresses: []string{"192.0.2.42"}, Target: "worker", ResolveOnly: true})
	if err == nil || !strings.Contains(err.Error(), "Match exec") {
		t.Fatalf("executable included configuration was missed: %v", err)
	}
}

func TestConfigurationLimitsFailClosed(t *testing.T) {
	var aliases strings.Builder
	for index := range maxAliases + 1 {
		fmt.Fprintf(&aliases, "Host worker-%d\n", index)
	}
	for name, config := range map[string]string{
		"alias limit": aliases.String(),
		"byte limit":  strings.Repeat("#", maxConfigBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			c := fixtureChecker(t, config)
			_, err := c.check(context.Background(), Request{Addresses: []string{"192.0.2.42"}, Username: "worker", ResolveOnly: true})
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("configuration limit error = %v", err)
			}
		})
	}
}
