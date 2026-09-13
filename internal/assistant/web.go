package assistant

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// PublicURL is shared by page reads, search results, downloads and redirects.
// Credentials and local services must never be accessed through the web tools.
func PublicURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 8192 || strings.ContainsAny(raw, "\x00\r\n\\") || u == nil || u.Opaque != "" || u.User != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("use a public HTTP(S) URL without credentials")
	}
	if u.Port() != "" && !((u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80")) {
		return nil, errors.New("web tools only allow standard HTTP(S) ports")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	ip, ipErr := netip.ParseAddr(host)
	if host == "localhost" || (ipErr != nil && !strings.Contains(host, ".")) || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.Contains(host, "%") {
		return nil, errors.New("local network destinations are unavailable to web tools")
	}
	if ipErr == nil && !publicIP(ip) {
		return nil, errors.New("non-public network destination")
	}
	u.Fragment = ""
	return u, nil
}

var privateWebRanges = func() []netip.Prefix {
	var ranges []netip.Prefix
	for _, cidr := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/3", "2001::/23", "2001:db8::/32", "2002::/16"} {
		ranges = append(ranges, netip.MustParsePrefix(cidr))
	}
	return ranges
}()

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || (ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip)) {
		return false
	}
	for _, p := range privateWebRanges {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

type webConn struct{ net.Conn }

func (c webConn) Read(p []byte) (int, error) {
	_ = c.SetReadDeadline(time.Now().Add(45 * time.Second))
	return c.Conn.Read(p)
}

// Resolve once per connection and dial the validated IP, preventing DNS rebinding.
// Neither the environment proxy nor a cookie jar nor internal credentials are used.
func NewPublicHTTPClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{Proxy: nil, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 20 * time.Second, MaxResponseHeaderBytes: 64 << 10, DisableCompression: true,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, errors.New("invalid web destination")
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil || len(ips) == 0 {
				return nil, errors.New("could not resolve public web destination")
			}
			for _, ip := range ips {
				if !publicIP(ip) {
					return nil, errors.New("web destination resolves to a non-public address")
				}
			}
			for _, ip := range ips {
				conn, e := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
				if e == nil {
					return webConn{conn}, nil
				}
			}
			return nil, errors.New("could not connect to public web destination")
		},
	}
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many web redirects")
		}
		if _, err := PublicURL(req.URL.String()); err != nil {
			return err
		}
		if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errors.New("HTTPS downgrade redirect refused")
		}
		return nil
	}}
}

func PublicGET(ctx context.Context, client *http.Client, raw string) (*http.Response, error) {
	u, err := PublicURL(raw)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("invalid web request")
	}
	req.Header.Set("User-Agent", "CasaOS-Assistant/1.0")
	req.Header.Set("Accept-Encoding", "identity")
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// url.Error may contain signed URLs; don't expose it to model observations.
		return nil, errors.New("web request failed: check connectivity, public address and redirects")
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("website returned HTTP %d; authentication or site restrictions may require user action", res.StatusCode)
	}
	return res, nil
}

func ReadWebBody(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("web response exceeds the read limit; use a smaller page or API response")
	}
	return data, nil
}
