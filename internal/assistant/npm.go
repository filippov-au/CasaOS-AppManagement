package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type ownerContextKey struct{}

func WithOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, ownerContextKey{}, owner)
}
func ContextOwner(ctx context.Context) string {
	owner, _ := ctx.Value(ownerContextKey{}).(string)
	return owner
}

type NPMConnection struct {
	App          string `json:"app"`
	Service      string `json:"service"`
	Username     string `json:"username"`
	Password     string `json:"-"`
	Suffix       string `json:"suffix"`
	AccessListID int    `json:"access_list_id"`
	Revision     string `json:"revision"`
}
type NPMRoute struct {
	App     string `json:"app"`
	Service string `json:"service"`
	Domain  string `json:"domain"`
	Marker  string `json:"marker"`
	HostID  int    `json:"host_id"`
	Port    int    `json:"port"`
}
type npmSaved struct {
	Connection *NPMConnection      `json:"connection"`
	Password   string              `json:"password,omitempty"`
	Routes     map[string]NPMRoute `json:"routes"`
}
type NPMStore struct {
	Root string
	mu   sync.Mutex
}

func (s *NPMStore) read(owner string) (npmSaved, error) {
	v := npmSaved{Routes: map[string]NPMRoute{}}
	if !ownerPattern.MatchString(owner) {
		return v, errors.New("authenticated user required")
	}
	b, err := os.ReadFile(filepath.Join(s.Root, owner+".json"))
	if os.IsNotExist(err) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	if err = json.Unmarshal(b, &v); err != nil {
		return v, errors.New("could not read NPM settings")
	}
	if v.Routes == nil {
		v.Routes = map[string]NPMRoute{}
	}
	if v.Connection != nil {
		v.Connection.Password = v.Password
	}
	return v, nil
}
func (s *NPMStore) write(owner string, v npmSaved) error {
	if err := os.MkdirAll(s.Root, 0700); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Root, ".npm-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), filepath.Join(s.Root, owner+".json"))
}
func (s *NPMStore) Connection(owner string) (*NPMConnection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, e := s.read(owner)
	return v.Connection, e
}
func (s *NPMStore) Save(owner string, c NPMConnection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, e := s.read(owner)
	if e != nil {
		return e
	}
	c.Revision = ID()
	v.Connection = &c
	v.Password = c.Password
	return s.write(owner, v)
}
func (s *NPMStore) Disconnect(owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, e := s.read(owner)
	if e != nil {
		return e
	}
	v.Connection = nil
	v.Password = ""
	return s.write(owner, v)
}
func NPMRouteKey(c NPMConnection, domain string) string {
	return c.App + "/" + c.Service + "/" + domain
}
func (s *NPMStore) Route(owner string, c NPMConnection, domain string) (NPMRoute, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, e := s.read(owner)
	return v.Routes[NPMRouteKey(c, domain)], e
}
func (s *NPMStore) SaveRoute(owner string, c NPMConnection, r NPMRoute) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, e := s.read(owner)
	if e != nil {
		return e
	}
	if v.Connection == nil || v.Connection.Revision != c.Revision {
		return errors.New("NPM connection changed; inspect and propose again")
	}
	v.Routes[NPMRouteKey(c, r.Domain)] = r
	return s.write(owner, v)
}

type NPMCertificate struct {
	ID      int      `json:"id"`
	Domains []string `json:"domain_names"`
	Expires string   `json:"expires_on"`
}
type NPMAccessList struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}
type NPMHost struct {
	ID            int                    `json:"id,omitempty"`
	Domains       []string               `json:"domain_names"`
	Scheme        string                 `json:"forward_scheme"`
	Host          string                 `json:"forward_host"`
	Port          int                    `json:"forward_port"`
	CertificateID int                    `json:"certificate_id"`
	AccessListID  int                    `json:"access_list_id"`
	SSLForced     bool                   `json:"ssl_forced"`
	Websocket     bool                   `json:"allow_websocket_upgrade"`
	Enabled       bool                   `json:"enabled"`
	Advanced      string                 `json:"advanced_config"`
	Locations     []json.RawMessage      `json:"locations"`
	Meta          map[string]interface{} `json:"meta"`
}

func (h NPMHost) Marker() string { s, _ := h.Meta["casaos_assistant"].(string); return s }
func (h NPMHost) SameRoute(other NPMHost) bool {
	return len(h.Domains) == 1 && len(other.Domains) == 1 && h.Domains[0] == other.Domains[0] && h.Scheme == other.Scheme && h.Host == other.Host && h.Port == other.Port && h.CertificateID == other.CertificateID && h.AccessListID == other.AccessListID && h.SSLForced && h.Websocket && h.Enabled && h.Advanced == "" && len(h.Locations) == 0
}

var DNSLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func ValidSuffix(s string) bool {
	if len(s) > 189 || !strings.Contains(s, ".") {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if !DNSLabel.MatchString(label) {
			return false
		}
	}
	return true
}
func (c NPMCertificate) Expiry() time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if t, e := time.Parse(layout, c.Expires); e == nil {
			return t
		}
	}
	return time.Time{}
}
func (c NPMCertificate) Covers(suffix string, now time.Time) bool {
	if c.ID <= 0 || !ValidSuffix(suffix) || !c.Expiry().After(now) {
		return false
	}
	for _, d := range c.Domains {
		if strings.ToLower(d) == "*."+suffix {
			return true
		}
	}
	return false
}
func NPMSuffixes(certs []NPMCertificate, now time.Time) []string {
	set := map[string]bool{}
	for _, c := range certs {
		for _, d := range c.Domains {
			d = strings.ToLower(d)
			if strings.HasPrefix(d, "*.") && c.Covers(d[2:], now) {
				set[d[2:]] = true
			}
		}
	}
	out := []string{}
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
func SelectNPMCertificate(certs []NPMCertificate, suffix string, now time.Time) (NPMCertificate, error) {
	var best NPMCertificate
	for _, c := range certs {
		if c.Covers(suffix, now) && (best.ID == 0 || c.Expiry().After(best.Expiry()) || (c.Expiry().Equal(best.Expiry()) && c.ID < best.ID)) {
			best = c
		}
	}
	if best.ID == 0 {
		return best, errors.New("a valid wildcard certificate for the selected suffix is required; add or renew it in NPM, then refresh the connection")
	}
	return best, nil
}

type NPMClient struct {
	Base       string
	Connection NPMConnection
	HTTP       *http.Client
	token      string
}

func NewNPMClient(base string, c NPMConnection) *NPMClient {
	return &NPMClient{Base: base, Connection: c, HTTP: &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *NPMClient) Close() { c.HTTP.CloseIdleConnections(); c.token = ""; c.Connection.Password = "" }
func (c *NPMClient) request(ctx context.Context, method, path string, body, out interface{}) (int, error) {
	var b []byte
	var err error
	if body != nil {
		b, err = json.Marshal(body)
		if err != nil {
			return 0, errors.New("invalid NPM request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+"/api"+path, bytes.NewReader(b))
	if err != nil {
		return 0, errors.New("invalid NPM endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return 0, errors.New("NPM API could not be reached")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return res.StatusCode, fmt.Errorf("NPM API returned HTTP %d", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 2*1024*1024+1))
	if err != nil || len(data) > 2*1024*1024 {
		return res.StatusCode, errors.New("NPM response exceeded limit or could not be read")
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		return res.StatusCode, errors.New("unsupported NPM API response")
	}
	return res.StatusCode, nil
}
func (c *NPMClient) login(ctx context.Context) error {
	c.token = ""
	var v struct {
		Token string `json:"token"`
	}
	_, err := c.request(ctx, "POST", "/tokens", map[string]string{"identity": c.Connection.Username, "secret": c.Connection.Password}, &v)
	if err != nil {
		return errors.New("NPM login failed; check the saved credentials and API access")
	}
	if v.Token == "" {
		return errors.New("NPM did not issue an API token; this login may require interactive authentication")
	}
	c.token = v.Token
	return nil
}
func (c *NPMClient) Call(ctx context.Context, method, path string, body, out interface{}) error {
	if c.token == "" {
		if e := c.login(ctx); e != nil {
			return e
		}
	}
	status, err := c.request(ctx, method, path, body, out)
	if status == 401 {
		if e := c.login(ctx); e != nil {
			return e
		}
		_, err = c.request(ctx, method, path, body, out)
	}
	return err
}
func (c *NPMClient) Certificates(ctx context.Context) ([]NPMCertificate, error) {
	v := []NPMCertificate{}
	e := c.Call(ctx, "GET", "/nginx/certificates", nil, &v)
	return v, e
}
func (c *NPMClient) AccessLists(ctx context.Context) ([]NPMAccessList, error) {
	v := []NPMAccessList{}
	e := c.Call(ctx, "GET", "/nginx/access-lists", nil, &v)
	return v, e
}
func (c *NPMClient) Hosts(ctx context.Context) ([]NPMHost, error) {
	v := []NPMHost{}
	e := c.Call(ctx, "GET", "/nginx/proxy-hosts", nil, &v)
	return v, e
}
