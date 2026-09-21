package assistant

import (
	"net/netip"
	"testing"
)

func TestLinkedInstructionsRejectNonPublicDestinations(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "169.254.169.254", "100.92.139.82", "192.168.1.1", "::ffff:127.0.0.1", "fd00::1", "2002:7f00:1::", "64:ff9b::7f00:1"} {
		t.Run(address, func(t *testing.T) {
			if publicAddress(netip.MustParseAddr(address)) {
				t.Fatalf("untrusted documentation could connect to non-public address %s", address)
			}
		})
	}
	if !publicAddress(netip.MustParseAddr("1.1.1.1")) || !publicAddress(netip.MustParseAddr("2606:4700:4700::1111")) {
		t.Fatal("public documentation address was rejected")
	}
	for _, address := range []string{"http://example.org/install", "https://user:password@example.org/install", "https://example.org:9443/install", "https://[::ffff:127.0.0.1]/install"} {
		if _, err := publicInstructionURL(address); err == nil {
			t.Fatalf("unsafe linked instruction URL was accepted: %s", address)
		}
	}
}

func TestLinkedInstructionsResolveOnlyExplicitReferences(t *testing.T) {
	links := explicitLinks(ContextFile{
		Path: "https://example.org/old", ResolvedURL: "https://docs.example.org/setup/index.html",
		Content: `<a href="../launch.html#gpu">Launch</a> [Install](https://example.org/install)`,
	})
	if !links["https://docs.example.org/launch.html#gpu"] || !links["https://docs.example.org/launch.html"] || !links["https://example.org/install"] {
		t.Fatal("linked instructions lost redirect-relative or fragment-free targets")
	}
	if links["https://example.org/launch.html"] || links["https://docs.example.org/guessed"] {
		t.Fatal("invented documentation target became eligible for fetching")
	}
}
