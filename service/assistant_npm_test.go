package service

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	stdjson "encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IceWhaleTech/CasaOS-AppManagement/common"
	"github.com/IceWhaleTech/CasaOS-AppManagement/internal/assistant"
	"github.com/IceWhaleTech/CasaOS-Common/utils/logger"
	dockerTypes "github.com/docker/docker/api/types"
)

func npmTestCertificate(t *testing.T, domain string) ([]byte, []byte, *x509.CertPool) {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "CasaOS test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, e := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	root, e := x509.ParseCertificate(caDER)
	if e != nil {
		t.Fatal(e)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: domain}, DNSNames: []string{domain}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(12 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, e := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), roots
}
func npmTestApp(t *testing.T, root, name string) *ComposeApp {
	t.Helper()
	dir := filepath.Join(root, name)
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	b := []byte("name: " + name + "\nservices:\n  web:\n    image: nginx:alpine\nx-casaos:\n  main: web\n  index: /library?view=tv\n")
	if e := os.WriteFile(filepath.Join(dir, common.ComposeYAMLFileName), b, 0600); e != nil {
		t.Fatal(e)
	}
	app, e := assistantLoad(name)
	if e != nil {
		t.Fatal(e)
	}
	return app
}
func TestAssistantNPMRejectsInvalidHostsAndUnownedRoutes(t *testing.T) {
	for _, label := range []string{"app.example.com", "a.b", "*", "-app", strings.Repeat("a", 64), "UPPER", "a/b"} {
		if _, e := npmLabel("app", label); e == nil {
			t.Fatal("accepted", label)
		}
	}
	if label, e := npmLabel("my_app", ""); e != nil || label != "my-app" {
		t.Fatal(label, e)
	}
	r := assistant.NPMRoute{App: "app", Service: "web", Domain: "app.example.com", Marker: "ours", HostID: 1}
	h := &assistant.NPMHost{ID: 1, Domains: []string{r.Domain}, Meta: map[string]interface{}{"casaos_assistant": "ours"}}
	if e := npmValidateHost(h, r, "app", "web"); e != nil {
		t.Fatal(e)
	}
	h.Meta["casaos_assistant"] = "other"
	if e := npmValidateHost(h, r, "app", "web"); e == nil {
		t.Fatal("took over unrelated route")
	}
	h.Meta["casaos_assistant"] = "ours"
	h.Advanced = "return 200;"
	if e := npmValidateHost(h, r, "app", "web"); e == nil {
		t.Fatal("overwrote custom config")
	}
	h.Advanced = ""
	h.Domains = []string{"*.example.com"}
	if e := npmValidateHost(h, r, "app", "web"); e == nil {
		t.Fatal("took over wildcard route")
	}
	if !(AssistantRuntime{}).IsWrite("publish_app") || (AssistantRuntime{}).IsWrite("npm_inspect") {
		t.Fatal("incorrect permissions")
	}
}
func TestAssistantNPMProbeRequiresTrustedWildcardAndPreservesCardPath(t *testing.T) {
	logger.LogInitConsoleOnly()
	root := assistantTestRoot(t)
	app := npmTestApp(t, root, "app")
	for _, domain := range []string{"*.localhost", "app.localhost"} {
		t.Run(domain, func(t *testing.T) {
			cert, key, roots := npmTestCertificate(t, domain)
			pair, e := tls.X509KeyPair(cert, key)
			if e != nil {
				t.Fatal(e)
			}
			status := 200
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Host != "app.localhost" || r.URL.String() != "/library?view=tv" {
					t.Error("incorrect Host or app path", r.Host, r.URL.String())
				}
				w.WriteHeader(status)
			}))
			server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
			server.StartTLS()
			defer server.Close()
			host, p, _ := net.SplitHostPort(server.Listener.Addr().String())
			port, _ := strconv.Atoi(p)
			npm := dockerTypes.Container{Ports: []dockerTypes.Port{{IP: host, PrivatePort: 443, PublicPort: uint16(port), Type: "tcp"}}}
			// localhost DNS support differs by OS; TLS and status are still independently checked.
			result, e := npmProbeTLS(context.Background(), npm, app, "app.localhost", "localhost", roots)
			if domain == "app.localhost" {
				if e == nil || result.TLS != "missing wildcard" {
					t.Fatal(result, e)
				}
				return
			}
			if e != nil || result.HTTPStatus != 200 || !strings.Contains(result.TLS, "trusted wildcard") {
				t.Fatal(result, e)
			}
			status = 502
			result, e = npmProbeTLS(context.Background(), npm, app, "app.localhost", "localhost", roots)
			if e != nil || result.Verified || result.Status != "application or upstream error" {
				t.Fatal(result, e)
			}
			status = 401
			result, e = npmProbeTLS(context.Background(), npm, app, "app.localhost", "localhost", roots)
			if e != nil || result.Status != "authentication or access policy required" {
				t.Fatal(result, e)
			}
			if _, e = npmProbeTLS(context.Background(), npm, app, "app.localhost", "localhost", nil); e == nil {
				t.Fatal("accepted untrusted certificate")
			}
		})
	}
	if e := npmSaveCard(app, "app.example.com"); e != nil {
		t.Fatal(e)
	}
	updated, e := assistantLoad("app")
	if e != nil {
		t.Fatal(e)
	}
	info, e := updated.StoreInfo(false)
	if e != nil || *info.Hostname != "app.example.com" || string(*info.Scheme) != "https" || info.Index != "/library?view=tv" || info.PortMap != "443" {
		t.Fatal(info, e)
	}
	backup, e := os.ReadFile(app.ComposeFiles[0] + ".assistant.bak")
	if e != nil || strings.Contains(string(backup), "hostname") {
		t.Fatal("lost original card backup", e)
	}
}
func TestAssistantNPMInspectExcludesLoginFromModel(t *testing.T) {
	// Serialization of returned inventories must never include the private password.
	inv := AssistantNPMInventory{Connection: &assistant.NPMConnection{Password: "do-not-expose"}}
	b, e := stdjson.Marshal(inv)
	if e != nil || strings.Contains(string(b), "do-not-expose") {
		t.Fatal(string(b), e)
	}
}
