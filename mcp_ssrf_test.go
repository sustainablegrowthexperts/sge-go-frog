package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testSSRFPolicy allows the loopback host used by httptest servers so existing
// crawl integration tests keep working, while still rejecting metadata,
// link-local, multicast, unspecified, and IPv4-mapped IPv6 targets. Hostnames
// resolve to a fixed public documentation address so tests never perform real
// DNS lookups.
func testSSRFPolicy() *ssrfPolicy {
	p := newSSRFPolicy()
	p.allowHost = func(host string) bool {
		switch strings.ToLower(host) {
		case "127.0.0.1", "::1", "localhost":
			return true
		default:
			return false
		}
	}
	p.lookupIP = func(_ context.Context, host string) ([]net.IPAddr, error) {
		if ip := net.ParseIP(host); ip != nil {
			return []net.IPAddr{{IP: ip}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34").To4()}}, nil
	}
	return p
}

func staticSSRFResolver(hosts map[string][]string) func(context.Context, string) ([]net.IPAddr, error) {
	return func(_ context.Context, host string) ([]net.IPAddr, error) {
		if ip := net.ParseIP(host); ip != nil {
			return []net.IPAddr{{IP: ip}}, nil
		}
		raw, ok := hosts[host]
		if !ok {
			return nil, fmt.Errorf("unexpected lookup for %q", host)
		}
		out := make([]net.IPAddr, 0, len(raw))
		for _, entry := range raw {
			ip := net.ParseIP(entry)
			if ip != nil && !strings.Contains(entry, ":") {
				ip = ip.To4()
			}
			out = append(out, net.IPAddr{IP: ip})
		}
		return out, nil
	}
}

func TestSSRFClassifyIP(t *testing.T) {
	tests := []struct {
		name string
		ip   net.IP
		want ipClass
	}{
		{"public ipv4", net.ParseIP("93.184.216.34").To4(), ipClassPublic},
		{"public ipv6", net.ParseIP("2606:4700:4700::1111"), ipClassPublic},
		{"loopback ipv4", net.ParseIP("127.0.0.1").To4(), ipClassLoopback},
		{"loopback ipv6", net.ParseIP("::1"), ipClassLoopback},
		{"private 10", net.ParseIP("10.1.2.3").To4(), ipClassPrivate},
		{"private 172.16", net.ParseIP("172.16.5.4").To4(), ipClassPrivate},
		{"private 192.168", net.ParseIP("192.168.1.10").To4(), ipClassPrivate},
		{"private ipv6 ula", net.ParseIP("fd12:3456::1"), ipClassPrivate},
		{"cgnat", net.ParseIP("100.64.0.1").To4(), ipClassPrivate},
		{"link-local ipv4", net.ParseIP("169.254.1.1").To4(), ipClassLinkLocal},
		{"link-local ipv6", net.ParseIP("fe80::1"), ipClassLinkLocal},
		{"link-local multicast ipv6", net.ParseIP("ff02::1"), ipClassLinkLocal},
		{"unspecified ipv4", net.ParseIP("0.0.0.0").To4(), ipClassUnspecified},
		{"unspecified ipv6", net.ParseIP("::"), ipClassUnspecified},
		{"multicast ipv4", net.ParseIP("224.0.0.1").To4(), ipClassMulticast},
		{"multicast ipv6", net.ParseIP("ff05::1"), ipClassMulticast},
		{"metadata ipv4", net.ParseIP("169.254.169.254").To4(), ipClassMetadata},
		{"metadata ecs", net.ParseIP("169.254.170.2").To4(), ipClassMetadata},
		{"metadata alibaba", net.ParseIP("100.100.100.200").To4(), ipClassMetadata},
		{"metadata ipv6", net.ParseIP("fd00:ec2::254"), ipClassMetadata},
		{"ipv4-mapped public", net.ParseIP("::ffff:8.8.8.8"), ipClassIPv4Mapped},
		{"ipv4-mapped loopback", net.ParseIP("::ffff:127.0.0.1"), ipClassIPv4Mapped},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyIP(tc.ip); got != tc.want {
				t.Fatalf("classifyIP(%v) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

func TestSSRFValidateURL(t *testing.T) {
	policy := newSSRFPolicy()
	policy.lookupIP = staticSSRFResolver(map[string][]string{
		"example.com":    {"93.184.216.34"},
		"example.org":    {"2606:4700:4700::1111"},
		"internal.test":  {"10.0.0.7"},
		"mixed.test":     {"93.184.216.34", "10.0.0.7"},
		"metadata.test":  {"169.254.169.254"},
		"linklocal.test": {"fe80::1"},
	})

	blocked := []struct {
		name   string
		target string
	}{
		{"loopback ipv4", "http://127.0.0.1/"},
		{"loopback ipv6", "http://[::1]/"},
		{"private 10/8", "http://10.0.0.1/"},
		{"private 172.16/12", "http://172.16.0.1/"},
		{"private 192.168/16", "http://192.168.0.1/"},
		{"link-local ipv4", "http://169.254.1.1/"},
		{"link-local ipv6", "http://[fe80::1]/"},
		{"link-local multicast", "http://[ff02::1]/"},
		{"unspecified ipv4", "http://0.0.0.0/"},
		{"unspecified ipv6", "http://[::]/"},
		{"multicast ipv4", "http://224.0.0.1/"},
		{"metadata ipv4", "http://169.254.169.254/latest/meta-data"},
		{"metadata ipv6", "http://[fd00:ec2::254]/latest/meta-data"},
		{"metadata alibaba", "http://100.100.100.200/"},
		{"ipv4-mapped public", "http://[::ffff:8.8.8.8]/"},
		{"ipv4-mapped loopback", "http://[::ffff:127.0.0.1]/"},
		{"hostname resolves private", "http://internal.test/path"},
		{"hostname resolves to mixed", "http://mixed.test/path"},
		{"hostname resolves metadata", "http://metadata.test/"},
		{"hostname resolves link-local", "http://linklocal.test/"},
		{"credentials", "https://user:pass@example.com/"},
		{"ftp scheme", "ftp://example.com/file"},
		{"file scheme", "file:///etc/passwd"},
		{"empty host", "http:///path"},
		{"malformed", "http://[::1"},
		{"scheme-relative", "//example.com/path"},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			err := policy.validateURL(context.Background(), tc.target)
			if err == nil {
				t.Fatalf("validateURL(%q) allowed a blocked target", tc.target)
			}
			if !strings.Contains(err.Error(), "public http or https") {
				t.Fatalf("error = %q, want a public-target message", err)
			}
		})
	}

	allowed := []struct {
		name   string
		target string
	}{
		{"public ipv4", "http://93.184.216.34/path"},
		{"public ipv6", "http://[2606:4700:4700::1111]/path"},
		{"public hostname", "https://example.com/path?q=1#frag"},
		{"public ipv6 hostname", "https://example.org/"},
		{"port preserved", "https://example.com:8443/path"},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			if err := policy.validateURL(context.Background(), tc.target); err != nil {
				t.Fatalf("validateURL(%q) = %v, want nil", tc.target, err)
			}
		})
	}

	t.Run("hostname ipv4 representation is canonicalized", func(t *testing.T) {
		resolved := newSSRFPolicy()
		resolved.lookupIP = func(_ context.Context, _ string) ([]net.IPAddr, error) {
			// net.DefaultResolver and net.ParseIP expose IPv4 addresses in
			// the 16-byte IPv4-mapped form; that must not be mistaken for an
			// IPv6 AAAA target.
			return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
		}
		if err := resolved.validateURL(context.Background(), "https://example.com/"); err != nil {
			t.Fatalf("public IPv4 hostname was rejected: %v", err)
		}
	})

	t.Run("errors do not leak host or ip", func(t *testing.T) {
		err := policy.validateURL(context.Background(), "http://internal.test/secret")
		if err == nil {
			t.Fatal("expected blocked target")
		}
		for _, leak := range []string{"internal.test", "10.0.0.7"} {
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("error %q leaked %q", err, leak)
			}
		}
	})
}

func TestSSRFStartCrawlRejectsBlockedTargets(t *testing.T) {
	s := &mcpServer{config: defaultMCPConfig(), ssrf: newSSRFPolicy()}

	t.Run("spider", func(t *testing.T) {
		_, err := s.validateStartCrawl(startCrawlInput{
			Mode:     "spider",
			StartURL: "http://127.0.0.1/",
		})
		if err == nil {
			t.Fatal("start_crawl accepted a loopback start URL")
		}
		if !strings.Contains(err.Error(), "invalid_argument") {
			t.Fatalf("error = %q, want invalid_argument", err)
		}
		if strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatalf("error leaked the target: %q", err)
		}
	})

	t.Run("list", func(t *testing.T) {
		_, err := s.validateStartCrawl(startCrawlInput{
			Mode: "list",
			URLs: []string{"http://93.184.216.34/", "http://169.254.169.254/"},
		})
		if err == nil {
			t.Fatal("start_crawl accepted a metadata URL")
		}
		if !strings.Contains(err.Error(), "invalid_argument") || !strings.Contains(err.Error(), "urls[2]") {
			t.Fatalf("error = %q, want invalid_argument naming urls[2]", err)
		}
		if strings.Contains(err.Error(), "169.254.169.254") {
			t.Fatalf("error leaked the target: %q", err)
		}
	})

	t.Run("public target accepted", func(t *testing.T) {
		if _, err := s.validateStartCrawl(startCrawlInput{
			Mode:     "spider",
			StartURL: "http://93.184.216.34/",
		}); err != nil {
			t.Fatalf("start_crawl rejected a public target: %v", err)
		}
	})
}

func TestSSRFDialBlocksRebinding(t *testing.T) {
	var lookups atomic.Int32
	policy := newSSRFPolicy()
	policy.lookupIP = func(_ context.Context, _ string) ([]net.IPAddr, error) {
		if lookups.Add(1) == 1 {
			return []net.IPAddr{{IP: net.ParseIP("93.184.216.34").To4()}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("10.9.8.7").To4()}}, nil
	}

	if err := policy.validateURL(context.Background(), "http://rebind.test/"); err != nil {
		t.Fatalf("first validation should see the public address: %v", err)
	}

	conn, err := policy.dialContext(context.Background(), "tcp", "rebind.test:80")
	if err == nil {
		_ = conn.Close()
		t.Fatal("dialContext connected after the resolver changed to a private address")
	}
	if strings.Contains(err.Error(), "10.9.8.7") {
		t.Fatalf("dial error leaked the private address: %q", err)
	}
}

func TestSSRFDialUsesValidatedAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	accepted := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = conn.Close()
			close(accepted)
		}
	}()

	policy := newSSRFPolicy()
	policy.allowHost = func(string) bool { return true }
	policy.lookupIP = staticSSRFResolver(map[string][]string{
		"public.test": {"127.0.0.1"},
	})

	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	conn, err := policy.dialContext(context.Background(), "tcp", "public.test:"+port)
	if err != nil {
		t.Fatalf("dialContext: %v", err)
	}
	_ = conn.Close()

	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("validated dial did not reach the listener")
	}
}

func TestSSRFClientBlocksRedirectToPrivateTarget(t *testing.T) {
	var secretHits atomic.Int32
	mux := http.NewServeMux()
	var serverURL string
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		parsed, _ := url.Parse(serverURL)
		http.Redirect(w, r, "http://private.redirect.test:"+parsed.Port()+"/secret", http.StatusFound)
	})
	mux.HandleFunc("/secret", func(w http.ResponseWriter, r *http.Request) {
		secretHits.Add(1)
		_, _ = w.Write([]byte("secret"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	serverURL = server.URL

	policy := newSSRFPolicy()
	policy.allowHost = func(host string) bool { return host == "127.0.0.1" }
	policy.lookupIP = staticSSRFResolver(map[string][]string{
		"private.redirect.test": {"127.0.0.1"},
	})

	client := newSSRFHTTPClient(policy, "", 5*time.Second)
	resp, err := client.Get(server.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("client followed a redirect to a private target")
	}
	if secretHits.Load() != 0 {
		t.Fatalf("private redirect target was fetched %d time(s)", secretHits.Load())
	}
}

func TestSSRFSpiderBlocksPrivateRedirect(t *testing.T) {
	var secretHits atomic.Int32
	mux := http.NewServeMux()
	var serverURL string
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		parsed, _ := url.Parse(serverURL)
		http.Redirect(w, r, "http://private.redirect.test:"+parsed.Port()+"/secret", http.StatusFound)
	})
	mux.HandleFunc("/secret", func(w http.ResponseWriter, r *http.Request) {
		secretHits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body><main><article><p>secret</p></article></main></body></html>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	serverURL = server.URL

	policy := newSSRFPolicy()
	policy.allowHost = func(host string) bool { return host == "127.0.0.1" }
	policy.lookupIP = staticSSRFResolver(map[string][]string{
		"private.redirect.test": {"127.0.0.1"},
	})

	_, err := runSpider(WizardSettings{
		Mode:        1,
		StartURL:    server.URL,
		Concurrency: 2,
		ssrfPolicy:  policy,
	}, nil)
	if err != nil {
		t.Fatalf("runSpider: %v", err)
	}
	if secretHits.Load() != 0 {
		t.Fatalf("spider followed a redirect to a private target %d time(s)", secretHits.Load())
	}
}

func TestSSRFListRejectsPrivateURL(t *testing.T) {
	_, err := runList([]string{"http://10.0.0.1/"}, WizardSettings{
		Mode:        2,
		Concurrency: 1,
		ssrfPolicy:  newSSRFPolicy(),
	}, nil)
	if err == nil {
		t.Fatal("runList accepted a private URL")
	}
	if strings.Contains(err.Error(), "10.0.0.1") {
		t.Fatalf("error leaked the target: %q", err)
	}
}
