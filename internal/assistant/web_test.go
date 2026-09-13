package assistant

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPublicWebRejectsLocalAndCredentialURLs(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "ftp://example.com/file", "http://user:pass@example.com/", "http://localhost", "http://localhost.", "http://printer.local", "http://server.internal", "http://127.0.0.1", "http://10.0.0.1", "http://169.254.169.254/latest", "http://100.64.0.1", "http://[::1]", "http://[::ffff:127.0.0.1]", "http://[fe80::1%25eth0]", "https://example.com:8443/", "http://2130706433", "http://192.168.1.1.nip.io:8080/"} {
		if _, err := PublicURL(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	for _, raw := range []string{"https://example.com/book.epub?download=1", "http://example.org:80/", "https://[2606:4700:4700::1111]/"} {
		if _, err := PublicURL(raw); err != nil {
			t.Errorf("rejected %q: %v", raw, err)
		}
	}
	for _, ip := range []string{"0.0.0.0", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "240.1.2.3", "ff02::1", "fc00::1", "2001:db8::1", "2002:7f00:1::", "64:ff9b::7f00:1", "::ffff:10.0.0.1"} {
		if publicIP(netip.MustParseAddr(ip)) {
			t.Errorf("accepted reserved address %s", ip)
		}
	}
}

func TestPublicWebTransportAndRedirectBoundary(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:8080")
	c := NewPublicHTTPClient(time.Second)
	defer c.CloseIdleConnections()
	transport := c.Transport.(*http.Transport)
	if transport.Proxy != nil || c.Jar != nil {
		t.Fatal("web client uses proxy or cookies")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// The dialer also validates DNS results; URL validation alone isn't enough.
	if conn, err := transport.DialContext(ctx, "tcp", net.JoinHostPort("localhost", "80")); err == nil {
		conn.Close()
		t.Fatal("dialer accepted a local DNS result")
	}
	first, _ := http.NewRequest("GET", "https://example.org/start", nil)
	for _, destination := range []string{"http://example.org/end", "https://127.0.0.1/secret", "https://[::1]/", "https://user:secret@example.org/"} {
		next, _ := http.NewRequest("GET", destination, nil)
		if err := c.CheckRedirect(next, []*http.Request{first}); err == nil {
			t.Errorf("accepted redirect to %s", destination)
		}
	}
	u, _ := url.Parse("https://cdn.example.org/content")
	next := &http.Request{URL: u}
	if err := c.CheckRedirect(next, []*http.Request{first}); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckRedirect(next, []*http.Request{first, first, first, first, first}); err == nil {
		t.Fatal("unbounded redirects")
	}
}

func TestWebBodyIsBounded(t *testing.T) {
	if _, err := ReadWebBody(strings.NewReader("12345"), 4); err == nil {
		t.Fatal("read oversized response")
	}
	if b, err := ReadWebBody(strings.NewReader("1234"), 4); err != nil || string(b) != "1234" {
		t.Fatal(string(b), err)
	}
	_, err := ReadWebBody(&brokenWebReader{}, 4)
	if err == nil {
		t.Fatal("ignored read error")
	}
}

type brokenWebReader struct{}

func (*brokenWebReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestToolProgressIsBoundedAndPrivate(t *testing.T) {
	s := &session{view: View{Status: "running"}, cfg: Settings{APIKey: "private-token"}}
	ctx := toolProgressContext(context.Background(), s, "download_app_content")
	for i := 0; i < 100; i++ {
		Progress(ctx, "Downloading private-token")
	}
	if len(s.view.Events) != 1 || strings.Contains(s.view.Events[0].Text, "private-token") || len(s.messages) != 0 {
		t.Fatal("progress grew history, leaked a secret or entered model messages", s.view.Events)
	}
	s.view.Status = "cancelled"
	Progress(ctx, "late update")
	if len(s.view.Events) != 1 || strings.Contains(s.view.Events[0].Text, "late") {
		t.Fatal("progress continued after stop")
	}
}
