package agent

import (
	"testing"

	"github.com/jj-link/local-model-works/internal/hardware"
)

func TestInterfaceIPv4FollowsNamedInterface(t *testing.T) {
	list := []hardware.NetworkInterface{
		{Name: "eth0", Addresses: []string{"100.102.241.73"}},
		{Name: "tailscale0", Addresses: []string{"fd7a:115c:a1e0::1", "100.74.194.52"}},
		{Name: "lo", Addresses: []string{"127.0.0.1"}},
	}
	if got := interfaceIPv4(list, "tailscale0"); got != "100.74.194.52" {
		t.Fatalf("tailscale0 = %q", got)
	}
	if got := interfaceIPv4(list, "lo"); got != "" {
		t.Fatalf("loopback advertised: %q", got)
	}
	if got := interfaceIPv4(list, "missing"); got != "" {
		t.Fatalf("missing interface advertised: %q", got)
	}
}
