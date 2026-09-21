package assistant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var instructionLinks = regexp.MustCompile(`https?://[^\s<>"\x60]+|(?:href|src)\s*=\s*["']([^"']+)["']|\]\(([^\s)]+)`)

func explicitLinks(file ContextFile) map[string]bool {
	links := make(map[string]bool)
	base, _ := url.Parse(file.Path)
	if file.ResolvedURL != "" {
		base, _ = url.Parse(file.ResolvedURL)
	}
	for _, match := range instructionLinks.FindAllStringSubmatch(file.Content, -1) {
		value := match[0]
		if match[1] != "" {
			value = match[1]
		} else if match[2] != "" {
			value = match[2]
		}
		value = html.UnescapeString(strings.TrimRight(value, ").,;']"))
		ref, err := url.Parse(value)
		if err != nil {
			continue
		}
		if !ref.IsAbs() {
			if base == nil || base.Scheme != "https" || base.Host == "" {
				continue
			}
			ref = base.ResolveReference(ref)
		}
		if ref.Scheme == "https" || ref.Scheme == "http" {
			links[ref.String()] = true
			ref.Fragment = ""
			links[ref.String()] = true
		}
	}
	return links
}

var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/3"),
	netip.MustParsePrefix("::/96"), netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("fe80::/10"),
}

func publicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	// Currently allocated public IPv6 unicast space is 2000::/3. Reject
	// deprecated site-local, translation, and unallocated address families.
	if ip.Is6() && (ip.As16()[0]&0xe0) != 0x20 {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func publicInstructionURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Port() != "" && u.Port() != "443") || strings.Contains(u.Hostname(), "%") {
		return nil, fmt.Errorf("linked instructions require a public HTTPS URL without credentials or a custom port")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !publicAddress(ip) {
		return nil, fmt.Errorf("linked instructions cannot access a non-public address")
	}
	u.Fragment = ""
	return u, nil
}

func dialPublicInstructions(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != "443" {
		return nil, fmt.Errorf("invalid linked-instruction endpoint")
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, fmt.Errorf("linked-instruction host could not be resolved")
	}
	for _, ip := range addresses {
		if !publicAddress(ip) {
			return nil, fmt.Errorf("linked-instruction host resolves to a non-public address")
		}
	}
	// Dial an already validated address, not the hostname: a second DNS lookup
	// would permit rebinding between validation and connection.
	dialer := net.Dialer{Timeout: 10 * time.Second}
	var last error
	for _, ip := range addresses {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}

func readPublicInstructions(ctx context.Context, raw string) (ContextFile, error) {
	u, err := publicInstructionURL(raw)
	if err != nil {
		return ContextFile{}, err
	}
	transport := &http.Transport{
		Proxy: nil, DialContext: dialPublicInstructions,
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("linked instructions exceeded the redirect limit")
		}
		_, err := publicInstructionURL(request.URL.String())
		return err
	}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return ContextFile{}, err
	}
	request.Header.Set("Accept", "text/plain, text/markdown, text/html, application/xhtml+xml, application/json")
	response, err := client.Do(request)
	if err != nil {
		return ContextFile{}, fmt.Errorf("public instruction read failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ContextFile{}, fmt.Errorf("linked instructions returned HTTP %d", response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || !(strings.HasPrefix(mediaType, "text/") || mediaType == "application/xhtml+xml" || mediaType == "application/json" || mediaType == "application/xml") {
		return ContextFile{}, fmt.Errorf("linked instructions are not a supported text document")
	}
	const maxPage = 2 << 20
	content, err := io.ReadAll(io.LimitReader(response.Body, maxPage+1))
	if err != nil {
		return ContextFile{}, fmt.Errorf("linked instructions ended before a complete document was received")
	}
	if len(content) > maxPage {
		return ContextFile{}, fmt.Errorf("linked instructions exceed the 2 MiB page limit; contents were not used")
	}
	if !utf8.Valid(content) || strings.IndexByte(string(content), 0) >= 0 {
		return ContextFile{}, fmt.Errorf("linked instructions are not UTF-8 text")
	}
	hash := sha256.Sum256(content)
	text := string(content)
	endLine := strings.Count(text, "\n") + 1
	if strings.HasSuffix(text, "\n") {
		endLine--
	}
	if endLine < 1 {
		endLine = 1
	}
	return ContextFile{Path: raw, ResolvedURL: response.Request.URL.String(), SHA256: hex.EncodeToString(hash[:]), Origin: "linked_instructions", StartLine: 1, EndLine: endLine, Content: text}, nil
}
