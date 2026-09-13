package assistant

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNPMWildcardSelection(t *testing.T) {
	now := time.Now().UTC()
	expiry := now.Add(time.Hour).Format(time.RFC3339)
	certs := []NPMCertificate{{ID: 8, Domains: []string{"*.example.com"}, Expires: expiry}, {ID: 3, Domains: []string{"*.example.com", "*.home.example.com"}, Expires: expiry}, {ID: 1, Domains: []string{"*.expired.com"}, Expires: now.Add(-time.Hour).Format(time.RFC3339)}, {ID: 2, Domains: []string{"app.specific.com"}, Expires: expiry}, {ID: 4, Domains: []string{"*.*.example.com", "*.com"}, Expires: expiry}, {ID: 5, Domains: []string{"*.broken.com"}, Expires: "unknown"}}
	suffixes := NPMSuffixes(certs, now)
	if strings.Join(suffixes, ",") != "example.com,home.example.com" {
		t.Fatal(suffixes)
	}
	chosen, e := SelectNPMCertificate(certs, "example.com", now)
	if e != nil || chosen.ID != 3 {
		t.Fatal(chosen, e)
	}
	for _, suffix := range []string{"deep.home.example.com", "expired.com", "specific.com", "broken.com", "example.com.evil.org"} {
		if _, e := SelectNPMCertificate(certs, suffix, now); e == nil {
			t.Fatal("accepted", suffix)
		}
	}
	if _, e := SelectNPMCertificate(certs, "example.com", now.Add(2*time.Hour)); e == nil {
		t.Fatal("accepted expired")
	}
	certs = append(certs, NPMCertificate{ID: 12, Domains: []string{"*.example.com"}, Expires: now.Add(4 * time.Hour).Format(time.RFC3339)})
	chosen, _ = SelectNPMCertificate(certs, "example.com", now)
	if chosen.ID != 12 {
		t.Fatal("did not select latest expiry")
	}
}
func TestNPMStoreIsolationRestartAndDisconnect(t *testing.T) {
	store := &NPMStore{Root: t.TempDir()}
	c := NPMConnection{App: "npm", Service: "web", Username: "owner@example.com", Password: "private-password", Suffix: "example.com"}
	if e := store.Save("1", c); e != nil {
		t.Fatal(e)
	}
	c1, e := store.Connection("1")
	if e != nil || c1.Password != c.Password {
		t.Fatal(e)
	}
	b, _ := json.Marshal(c1)
	if strings.Contains(string(b), c.Password) {
		t.Fatal("password returned")
	}
	info, _ := os.Stat(filepath.Join(store.Root, "1.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	if c2, e := store.Connection("2"); e != nil || c2 != nil {
		t.Fatal("cross user read")
	}
	if _, e := store.Connection("../1"); e == nil {
		t.Fatal("invalid owner")
	}
	route := NPMRoute{App: "sonarr", Service: "web", Domain: "sonarr.example.com", Marker: ID()}
	if e := store.SaveRoute("1", *c1, route); e != nil {
		t.Fatal(e)
	}
	restarted := &NPMStore{Root: store.Root}
	saved, _ := restarted.Route("1", *c1, route.Domain)
	if saved.Marker != route.Marker {
		t.Fatal("lost recovery intent")
	}
	if e := restarted.Disconnect("1"); e != nil {
		t.Fatal(e)
	}
	if e := restarted.SaveRoute("1", *c1, route); e == nil {
		t.Fatal("disconnected approval accepted")
	}
	data, _ := os.ReadFile(filepath.Join(store.Root, "1.json"))
	if strings.Contains(string(data), c.Password) || strings.Contains(string(data), c.Username) {
		t.Fatal("disconnect retained credentials")
	}
	if e := restarted.Save("1", c); e != nil {
		t.Fatal(e)
	}
	if e := restarted.SaveRoute("1", *c1, route); e == nil {
		t.Fatal("stale revision accepted after reconnect")
	}
}
func TestNPMClientReauthAndSanitizesMetadataAndErrors(t *testing.T) {
	logins := 0
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tokens":
			logins++
			var v map[string]string
			_ = json.NewDecoder(r.Body).Decode(&v)
			if v["identity"] != "owner" || v["secret"] != "private-password" {
				t.Error("credentials missing")
			}
			_, _ = w.Write([]byte(`{"token":"private-token"}`))
		case "/api/nginx/certificates":
			b, _ := io.ReadAll(r.Body)
			if len(b) != 0 {
				t.Error("GET must have no JSON body")
			}
			reads++
			if reads == 1 {
				w.WriteHeader(401)
				return
			}
			if r.Header.Get("Authorization") != "Bearer private-token" {
				t.Error("no token")
			}
			_, _ = w.Write([]byte(`[{"id":1,"domain_names":["*.example.com"],"expires_on":"2030-01-01T00:00:00Z","meta":{"certificate_key":"private-key","dns_provider_credentials":"private-dns"}}]`))
		default:
			w.WriteHeader(500)
			_, _ = w.Write([]byte("private-password private-token private-key"))
		}
	}))
	defer server.Close()
	api := NewNPMClient(server.URL, NPMConnection{Username: "owner", Password: "private-password"})
	defer api.Close()
	certs, e := api.Certificates(context.Background())
	if e != nil || logins != 2 || len(certs) != 1 {
		t.Fatal(logins, certs, e)
	}
	b, _ := json.Marshal(certs)
	if strings.Contains(string(b), "private") {
		t.Fatal("metadata leak")
	}
	if e = api.Call(context.Background(), "GET", "/error", nil, nil); e == nil || strings.Contains(e.Error(), "private") {
		t.Fatal(e)
	}
}

type npmOwnerRuntime struct{ owners chan string }

func (*npmOwnerRuntime) Tools() []Tool       { return []Tool{{Function: ToolFunction{Name: "publish_app"}}} }
func (*npmOwnerRuntime) IsWrite(string) bool { return true }
func (r *npmOwnerRuntime) Prepare(ctx context.Context, _ string, _ json.RawMessage) (Prepared, error) {
	r.owners <- ContextOwner(ctx)
	return Prepared{Summary: "Publish", Execute: func(ctx context.Context) (string, error) {
		r.owners <- ContextOwner(ctx)
		return `{"url":"https://app.example.com/","verified":true,"card_updated":true}`, nil
	}}, nil
}
func TestNPMManagerOwnerPermissionsAndVerifiedLink(t *testing.T) {
	for _, mode := range []string{"read", "review", "auto"} {
		t.Run(mode, func(t *testing.T) {
			r := &npmOwnerRuntime{owners: make(chan string, 2)}
			m := NewManager(&testModel{tool: "publish_app"}, r)
			v, e := m.Start("1", testSettings(), "publish", mode)
			if e != nil {
				t.Fatal(e)
			}
			if mode == "review" {
				v = waitStatus(t, m, v.ID, "approval")
				if len(r.owners) != 1 {
					t.Fatal("executed before approval")
				}
				_, e = m.Approve("1", v.ID, v.Pending.ID, true)
				if e != nil {
					t.Fatal(e)
				}
			}
			v = waitStatus(t, m, v.ID, "done")
			if mode == "read" {
				if len(r.owners) != 0 {
					t.Fatal("read mode executed publication")
				}
				return
			}
			for i := 0; i < 2; i++ {
				if owner := <-r.owners; owner != "1" {
					t.Fatal("missing authenticated owner", owner)
				}
			}
			found := false
			for _, event := range v.Events {
				if event.URL == "https://app.example.com/" && event.CardUpdated {
					found = true
				}
			}
			if !found {
				t.Fatal("missing verified link")
			}
		})
	}
}
