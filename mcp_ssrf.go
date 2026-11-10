package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ssrfBlockedMessage is intentionally generic: tool errors and logs must never
// reveal the resolved private IP, internal hostname, or dialer details.
const ssrfBlockedMessage = "target is not a public http or https URL"

// ipClass is the security classification assigned to a resolved address.
type ipClass int

const (
	ipClassPublic ipClass = iota
	ipClassLoopback
	ipClassPrivate
	ipClassLinkLocal
	ipClassUnspecified
	ipClassMulticast
	ipClassIPv4Mapped
	ipClassMetadata
	ipClassInvalid
)

func (c ipClass) String() string {
	switch c {
	case ipClassPublic:
		return "public"
	case ipClassLoopback:
		return "loopback"
	case ipClassPrivate:
		return "private"
	case ipClassLinkLocal:
		return "link_local"
	case ipClassUnspecified:
		return "unspecified"
	case ipClassMulticast:
		return "multicast"
	case ipClassIPv4Mapped:
		return "ipv4_mapped"
	case ipClassMetadata:
		return "metadata"
	default:
		return "invalid"
	}
}

// cgnat is the RFC 6598 shared address space. Go's net.IP.IsPrivate does not
// classify it, but it is not reachable from the public internet.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}

// cloudMetadataIPs lists well-known metadata services. The link-local IPv4
// address is covered by IsLinkLocalUnicast as well; checking it first gives
// operators a useful "metadata" class in logs.
var cloudMetadataIPs = []net.IP{
	net.ParseIP("169.254.169.254").To4(), // AWS, GCP, Azure, OpenStack
	net.ParseIP("169.254.170.2").To4(),   // AWS ECS task metadata
	net.ParseIP("100.100.100.200").To4(), // Alibaba Cloud
	net.ParseIP("fd00:ec2::254"),         // AWS IPv6 IMDS
}

// classifyIP is the pure address classifier used by both validation and the
// dial-time guard. IPv4 values returned by net.ParseIP carry a 16-byte
// IPv4-mapped representation, so callers pass ip.To4() for literal IPv4 hosts.
func classifyIP(ip net.IP) ipClass {
	if ip == nil {
		return ipClassInvalid
	}
	if len(ip) == net.IPv6len && ip.To4() != nil {
		return ipClassIPv4Mapped
	}
	if isCloudMetadata(ip) {
		return ipClassMetadata
	}
	if ip.IsUnspecified() {
		return ipClassUnspecified
	}
	if ip.IsLoopback() {
		return ipClassLoopback
	}
	if ip.IsLinkLocalUnicast() {
		return ipClassLinkLocal
	}
	if ip.To4() == nil && ip.IsLinkLocalMulticast() {
		return ipClassLinkLocal
	}
	if ip.IsMulticast() {
		return ipClassMulticast
	}
	if ip.IsPrivate() {
		return ipClassPrivate
	}
	if ip4 := ip.To4(); ip4 != nil && cgnat.Contains(ip4) {
		return ipClassPrivate
	}
	return ipClassPublic
}

// classifyAddress retains the distinction between an IPv6 literal (where an
// IPv4-mapped value must be rejected) and a dotted IPv4 literal or DNS answer.
// Go's resolver and net.ParseIP expose IPv4 values in the 16-byte mapped form,
// so those are canonicalized to IPv4 before classification.
func classifyAddress(host string, ip net.IP) ipClass {
	if ip == nil {
		return ipClassInvalid
	}
	if strings.Contains(host, ":") {
		return classifyIP(ip)
	}
	if ip4 := ip.To4(); ip4 != nil {
		return classifyIP(ip4)
	}
	return classifyIP(ip)
}

func isCloudMetadata(ip net.IP) bool {
	for _, metadata := range cloudMetadataIPs {
		if ip.Equal(metadata) {
			return true
		}
	}
	return false
}

// ssrfBlockedError is returned for every rejected target. It carries only the
// address class so operators can log why a target was denied.
type ssrfBlockedError struct {
	class string
}

func (e *ssrfBlockedError) Error() string { return ssrfBlockedMessage }

// Class returns the stable address-class label used for operator logs.
func (e *ssrfBlockedError) Class() string { return e.class }

// ssrfPolicy validates crawl targets and dials only addresses that passed the
// public-address check. The lookup seam exists so tests can avoid real DNS;
// production uses net.DefaultResolver.
type ssrfPolicy struct {
	lookupIP func(context.Context, string) ([]net.IPAddr, error)

	// allowPrivate and allowHost are test seams. Production policies leave
	// both unset and never opt into private networks.
	allowPrivate bool
	allowHost    func(string) bool
}

func newSSRFPolicy() *ssrfPolicy {
	return &ssrfPolicy{lookupIP: net.DefaultResolver.LookupIPAddr}
}

func (p *ssrfPolicy) hostAllowed(host string) bool {
	return p != nil && p.allowHost != nil && p.allowHost(host)
}

func (p *ssrfPolicy) resolve(ctx context.Context, host string) ([]net.IPAddr, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IPAddr{{IP: ip}}, nil
	}
	lookup := p.lookupIP
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	return lookup(ctx, host)
}

// validateURL applies the public HTTP(S) policy to a single target.
func (p *ssrfPolicy) validateURL(ctx context.Context, raw string) error {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil {
		return &ssrfBlockedError{class: "malformed"}
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return &ssrfBlockedError{class: "scheme"}
	}
	if parsed.User != nil {
		return &ssrfBlockedError{class: "credentials"}
	}
	host := parsed.Hostname()
	if host == "" {
		return &ssrfBlockedError{class: "host"}
	}
	if p.hostAllowed(host) {
		return nil
	}
	return p.validateHost(ctx, host)
}

func (p *ssrfPolicy) validateHost(ctx context.Context, host string) error {
	addresses, err := p.resolve(ctx, host)
	if err != nil || len(addresses) == 0 {
		return &ssrfBlockedError{class: "resolve"}
	}
	for _, address := range addresses {
		if err := p.checkIP(classifyAddress(host, address.IP)); err != nil {
			return err
		}
	}
	return nil
}

func (p *ssrfPolicy) checkIP(class ipClass) error {
	switch class {
	case ipClassPublic:
		return nil
	case ipClassLoopback, ipClassPrivate:
		if p.allowPrivate {
			return nil
		}
	}
	return &ssrfBlockedError{class: class.String()}
}

// dialContext resolves the destination again at connection time and connects
// to the validated address. Re-resolving here closes the DNS-rebinding window
// between an earlier URL check and the actual dial.
func (p *ssrfPolicy) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, &ssrfBlockedError{class: "malformed"}
	}
	addresses, err := p.resolve(ctx, host)
	if err != nil || len(addresses) == 0 {
		return nil, &ssrfBlockedError{class: "resolve"}
	}
	if !p.hostAllowed(host) {
		for _, resolved := range addresses {
			if err := p.checkIP(classifyAddress(host, resolved.IP)); err != nil {
				return nil, err
			}
		}
	}
	ip := addresses[0].IP
	if !strings.Contains(host, ":") {
		if ip4 := ip.To4(); ip4 != nil {
			ip = ip4
		}
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
}

// ssrfValidatingTransport validates every outbound request URL, including
// redirect targets followed by http.Client. This is the only target check
// available when an explicit proxy is configured.
type ssrfValidatingTransport struct {
	base   *http.Transport
	policy *ssrfPolicy
}

func (t *ssrfValidatingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.policy.validateURL(req.Context(), req.URL.String()); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}

// newSSRFHTTPClient builds the shared validated client used by the spider,
// list mode, and sitemap discovery. TLS verification and SNI remain the Go
// defaults: only the dial address changes, never the request host.
func newSSRFHTTPClient(policy *ssrfPolicy, proxyURL string, timeout time.Duration) *http.Client {
	if policy == nil {
		policy = newSSRFPolicy()
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if strings.TrimSpace(proxyURL) != "" {
		if parsed, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(parsed)
		}
		// With a proxy the request URL is validated before it leaves the
		// process; the dial goes to the operator-configured proxy.
		transport.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	} else {
		transport.DialContext = policy.dialContext
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &ssrfValidatingTransport{base: transport, policy: policy},
	}
}

// blockedClass extracts the stable class label from a blocked-target error.
func blockedClass(err error) string {
	var blocked *ssrfBlockedError
	if errors.As(err, &blocked) {
		return blocked.Class()
	}
	return "blocked"
}

// logBlockedSSRFTarget records only the target class. The URL, host, and
// resolved address must never appear in operator logs for blocked targets.
func logBlockedSSRFTarget(logger *slog.Logger, err error) {
	var blocked *ssrfBlockedError
	if !errors.As(err, &blocked) {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("blocked crawl target", "class", blocked.Class())
}

// ssrfGuard returns the policy attached to the MCP server, defaulting to the
// public-only policy for zero-value servers.
func (s *mcpServer) ssrfGuard() *ssrfPolicy {
	if s != nil && s.ssrf != nil {
		return s.ssrf
	}
	return newSSRFPolicy()
}

func (s *mcpServer) logBlockedTarget(err error) {
	var logger *slog.Logger
	if s != nil && s.jobs != nil {
		logger = s.jobs.logger
	}
	logBlockedSSRFTarget(logger, err)
}

// ssrfGuard returns the policy configured for the crawl, defaulting to the
// public-only policy.
func (settings WizardSettings) ssrfGuard() *ssrfPolicy {
	if settings.ssrfPolicy != nil {
		return settings.ssrfPolicy
	}
	return newSSRFPolicy()
}

// blockedTargetError formats a non-leaky invalid_argument error for a blocked
// MCP crawl target.
func blockedTargetError(field string) error {
	return newToolError(codeInvalidArgument, fmt.Sprintf("%s is not a public http or https URL", field), nil)
}
