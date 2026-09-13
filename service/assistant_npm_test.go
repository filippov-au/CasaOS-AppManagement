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
	"fmt"
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
	"github.com/docker/docker/api/types/container"
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

type npmImageFixture struct {
	container      dockerTypes.ContainerJSON
	image          dockerTypes.ImageInspect
	containerCalls int
	imageCalls     int
}

func (f *npmImageFixture) ContainerInspect(_ context.Context, id string) (dockerTypes.ContainerJSON, error) {
	f.containerCalls++
	return f.container, nil
}
func (f *npmImageFixture) ImageInspectWithRaw(_ context.Context, id string) (dockerTypes.ImageInspect, []byte, error) {
	f.imageCalls++
	if f.container.ContainerJSONBase == nil || id != f.container.Image {
		return dockerTypes.ImageInspect{}, nil, fmt.Errorf("inspected the wrong image")
	}
	return f.image, nil, nil
}

func TestAssistantNPMRecognizesNormalizedImageReferences(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, ref := range []string{"jc21/nginx-proxy-manager", "jc21/nginx-proxy-manager:2.15.1", "docker.io/jc21/nginx-proxy-manager", "index.docker.io/jc21/nginx-proxy-manager:latest", "docker.io/jc21/nginx-proxy-manager@" + digest, "jc21/nginx-proxy-manager:2.15.1@" + digest} {
		if !npmImage(ref) {
			t.Errorf("missed official NPM reference %s", ref)
		}
	}
	for _, ref := range []string{"nginx:alpine", "someone/nginx-proxy-manager:latest", "registry.example/jc21/nginx-proxy-manager:latest", "jc21/nginx-proxy-manager-extra:latest", "jc21/nginx-proxy-manager:", digest} {
		if npmImage(ref) {
			t.Errorf("accepted unrelated or invalid reference %s", ref)
		}
	}
}
func TestAssistantNPMRecognizesImagesPinnedByCasaOSUpdates(t *testing.T) {
	id := "sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		name, display, configured string
		tags, digests             []string
		want                      string
	}{
		{name: "named image", display: "jc21/nginx-proxy-manager:2.15.1", want: "jc21/nginx-proxy-manager:2.15.1"},
		{name: "image ID after CasaOS update", display: id, configured: id, tags: []string{"jc21/nginx-proxy-manager:2.15.1"}, want: "jc21/nginx-proxy-manager:2.15.1"},
		{name: "short image ID", display: strings.Repeat("a", 12), configured: id, tags: []string{"jc21/nginx-proxy-manager:2.15.1"}, want: "jc21/nginx-proxy-manager:2.15.1"},
		{name: "moved tag", display: id, configured: "jc21/nginx-proxy-manager:latest", want: "jc21/nginx-proxy-manager:latest"},
		{name: "digest only", display: id, configured: id, digests: []string{"docker.io/jc21/nginx-proxy-manager@" + id}, want: "docker.io/jc21/nginx-proxy-manager@" + id},
		{name: "unrelated named image", display: "nginx:alpine"},
		{name: "unrelated pinned image", display: id, configured: id, tags: []string{"nginx:alpine"}},
		{name: "unidentifiable pinned image", display: id, configured: id},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &npmImageFixture{container: dockerTypes.ContainerJSON{ContainerJSONBase: &dockerTypes.ContainerJSONBase{Image: id}, Config: &container.Config{Image: tc.configured}}, image: dockerTypes.ImageInspect{ID: id, RepoTags: tc.tags, RepoDigests: tc.digests}}
			got, e := npmInstalledImage(context.Background(), fixture, dockerTypes.Container{ID: "fixture", Image: tc.display})
			if e != nil || got != tc.want {
				t.Fatalf("got %q (%v), want %q", got, e, tc.want)
			}
			if strings.Contains(tc.name, "named image") && fixture.containerCalls != 0 {
				t.Fatal("inspected a container whose named image was already known")
			}
		})
	}
}
