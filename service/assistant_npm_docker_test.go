package service

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"crypto/tls"
	"github.com/IceWhaleTech/CasaOS-AppManagement/common"
	"github.com/IceWhaleTech/CasaOS-AppManagement/internal/assistant"
	"github.com/IceWhaleTech/CasaOS-Common/utils/logger"
	dockerTypes "github.com/docker/docker/api/types"
	"github.com/gorilla/websocket"
	"golang.org/x/net/dns/dnsmessage"
)

func npmTestDNS(t *testing.T) {
	t.Helper()
	conn, e := net.ListenPacket("udp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, addr, e := conn.ReadFrom(buf)
			if e != nil {
				return
			}
			var msg dnsmessage.Message
			if msg.Unpack(buf[:n]) != nil {
				continue
			}
			msg.Header.Response = true
			msg.Header.RecursionAvailable = true
			for _, q := range msg.Questions {
				if q.Type == dnsmessage.TypeA {
					msg.Answers = append(msg.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: q.Name, Type: q.Type, Class: dnsmessage.ClassINET, TTL: 30}, Body: &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}}})
				}
			}
			b, e := msg.Pack()
			if e == nil {
				_, _ = conn.WriteTo(b, addr)
			}
		}
	}()
	old := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", conn.LocalAddr().String())
	}}
	t.Cleanup(func() { net.DefaultResolver = old })
}
func npmUploadTestCert(t *testing.T, ctx context.Context, api *assistant.NPMClient, cert, key []byte) int {
	t.Helper()
	var created struct {
		ID int `json:"id"`
	}
	if e := api.Call(ctx, "POST", "/nginx/certificates", map[string]interface{}{"provider": "other", "nice_name": "CasaOS test wildcard"}, &created); e != nil {
		t.Fatal(e)
	}
	var auth struct {
		Token string `json:"token"`
	}
	if e := api.Call(ctx, "POST", "/tokens", map[string]string{"identity": api.Connection.Username, "secret": api.Connection.Password}, &auth); e != nil {
		t.Fatal(e)
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for _, v := range []struct {
		name string
		data []byte
	}{{"certificate", cert}, {"certificate_key", key}} {
		part, e := form.CreateFormFile(v.name, v.name+".pem")
		if e != nil {
			t.Fatal(e)
		}
		_, _ = part.Write(v.data)
	}
	_ = form.Close()
	req, e := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/api/nginx/certificates/%d/upload", api.Base, created.ID), &body)
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Authorization", "Bearer "+auth.Token)
	req.Header.Set("Content-Type", form.FormDataContentType())
	res, e := api.HTTP.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("fixture certificate upload returned %d", res.StatusCode)
	}
	return created.ID
}
func TestAssistantNPMDockerPublication(t *testing.T) {
	if os.Getenv("CASAOS_ASSISTANT_NPM_DOCKER_TEST") != "1" {
		t.Skip("set CASAOS_ASSISTANT_NPM_DOCKER_TEST=1")
	}
	for _, mode := range []string{"shared-network", "published-port"} {
		t.Run(mode, func(t *testing.T) { npmDockerPublication(t, mode == "published-port") })
	}
}

func npmDockerPublication(t *testing.T, published bool) {
	logger.LogInitConsoleOnly()
	root := assistantTestRoot(t)
	oldStore := assistantNPMStore
	assistantNPMStore = &assistant.NPMStore{Root: filepath.Join(root, "private-npm")}
	t.Cleanup(func() { assistantNPMStore = oldStore })
	t.Setenv("DOCKER_API_VERSION", "1.44")
	endpoint, e := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("DOCKER_HOST", strings.TrimSpace(string(endpoint)))
	ctx, cancel := context.WithTimeout(assistant.WithOwner(context.Background(), "1"), 5*time.Minute)
	defer cancel()
	prefix := fmt.Sprintf("casaos-ai-npm-%d", time.Now().UnixNano())
	npmName, appName := prefix+"-proxy", prefix+"-app"
	network := npmNetworkName(npmName, "npm")
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", network).Run() })
	run := func(args ...string) {
		t.Helper()
		b, e := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if e != nil {
			t.Fatalf("docker %s: %v %s", args[0], e, b)
		}
	}
	files := map[string]string{}
	hostPort := 0
	if published {
		listener, err := net.Listen("tcp4", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		hostPort = listener.Addr().(*net.TCPAddr).Port
		listener.Close()
	}
	python := `from http.server import BaseHTTPRequestHandler, HTTPServer
import base64, hashlib
class Handler(BaseHTTPRequestHandler):
 def do_GET(self):
  if self.headers.get('Upgrade', '').lower() == 'websocket':
   self.send_response(101)
   self.send_header('Upgrade', 'websocket')
   self.send_header('Connection', 'Upgrade')
   self.send_header('Sec-WebSocket-Accept', base64.b64encode(hashlib.sha1((self.headers['Sec-WebSocket-Key']+'258EAFA5-E914-47DA-95CA-C5AB0DC85B11').encode()).digest()).decode())
   self.end_headers()
  else:
   self.send_response(200); self.end_headers(); self.wfile.write(b'CasaOS NPM fixture')
HTTPServer(('0.0.0.0', 8080), Handler).serve_forever()`
	command, _ := stdjson.Marshal([]string{"python", "-u", "-c", python})
	for _, name := range []string{npmName, appName} {
		dir := filepath.Join(root, name)
		if e = os.MkdirAll(dir, 0700); e != nil {
			t.Fatal(e)
		}
		file := filepath.Join(dir, common.ComposeYAMLFileName)
		files[name] = file
		content := "name: " + name + "\n"
		if name == npmName {
			content += `services:
  npm:
    image: jc21/nginx-proxy-manager:2.15.1
    environment:
      INITIAL_ADMIN_EMAIL: fixture@example.com
      INITIAL_ADMIN_PASSWORD: fixture-password-123
      DISABLE_IPV6: 'true'
      IP_RANGES_FETCH_ENABLED: 'false'
    ports:
      - '127.0.0.1::81'
      - '127.0.0.1::80'
      - '127.0.0.1::443'
    volumes:
      - npm-data:/data
      - npm-certs:/etc/letsencrypt
volumes:
  npm-data: {}
  npm-certs: {}
x-casaos:
  main: npm
`
		} else {
			content += "services:\n  web:\n    image: python:3.13-alpine\n    expose: ['8080']\n    command: " + string(command) + "\nx-casaos:\n  main: web\n  index: /library\n"
		}
		if published {
			if name == npmName {
				content = strings.Replace(content, "  npm:\n", "  npm:\n    network_mode: bridge\n", 1)
			} else {
				content = strings.Replace(content, "    expose:", fmt.Sprintf("    ports: ['%d:8080']\n    expose:", hostPort), 1)
			}
		}
		if e = os.WriteFile(file, []byte(content), 0600); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() {
			b, e := exec.Command("docker", "compose", "-f", file, "down", "--volumes").CombinedOutput()
			if e != nil {
				t.Errorf("fixture cleanup: %v %s", e, b)
			}
		})
		run("compose", "-f", file, "up", "-d")
	}
	// Exercise the actual CasaOS update path: saved Compose keeps the named
	// image, but the running container is created with an immutable image ID.
	npmApp, e := assistantLoad(npmName)
	if e != nil {
		t.Fatal(e)
	}
	npmYAML, e := os.ReadFile(files[npmName])
	if e != nil {
		t.Fatal(e)
	}
	if e = (dockerUpdateRuntime{}).Apply(ctx, npmApp, npmYAML); e != nil {
		t.Fatal(e)
	}
	instances, e := AssistantNPMDiscover(ctx)
	if e != nil {
		t.Fatal(e)
	}
	discovered := false
	for _, instance := range instances {
		if instance.App == npmName && instance.Service == "npm" && npmImage(instance.Image) {
			discovered = true
		}
	}
	if !discovered {
		t.Fatal("NPM disappeared from discovery after a CasaOS update")
	}
	c := assistant.NPMConnection{App: npmName, Service: "npm", Username: "fixture@example.com", Password: "fixture-password-123", Suffix: "apps.casaos.test", AccessListID: 0}
	var api *assistant.NPMClient
	for deadline := time.Now().Add(75 * time.Second); time.Now().Before(deadline); {
		api, _, e = npmResolve(ctx, c)
		if e == nil {
			_, e = api.Certificates(ctx)
			if e == nil {
				break
			}
			api.Close()
		}
		time.Sleep(time.Second)
	}
	if e != nil {
		t.Fatal("NPM fixture did not become ready", e)
	}
	defer api.Close()
	if e = AssistantNPMSave(ctx, "1", c); e == nil {
		t.Fatal("connected without wildcard")
	}
	cert, key, roots := npmTestCertificate(t, "*.apps.casaos.test")
	certID := npmUploadTestCert(t, ctx, api, cert, key)
	if e = AssistantNPMSave(ctx, "1", c); e != nil {
		t.Fatal(e)
	}
	saved, e := assistantNPMStore.Connection("1")
	if e != nil {
		t.Fatal(e)
	}
	c = *saved
	npmTestDNS(t)
	oldProbe := npmProbeApp
	npmProbeApp = func(ctx context.Context, npm dockerTypes.Container, app *ComposeApp, domain, suffix string) (npmProbeResult, error) {
		return npmProbeTLS(ctx, npm, app, domain, suffix, roots)
	}
	t.Cleanup(func() { npmProbeApp = oldProbe })
	raw := []byte(fmt.Sprintf(`{"app":%q,"service":"web","port":8080,"label":"fixture"}`, appName))
	publish := func() {
		t.Helper()
		p, e := (AssistantRuntime{}).Prepare(ctx, "publish_app", raw)
		if e != nil {
			t.Fatal(e)
		}
		out, e := p.Execute(ctx)
		if e != nil {
			t.Fatal(out, e)
		}
		var result npmProbeResult
		if stdjson.Unmarshal([]byte(out), &result) != nil || !result.Verified || !result.CardUpdated {
			t.Fatal(out)
		}
	}
	// Published-port publication must leave NPM Compose and both containers intact.
	beforeNPM, e := os.ReadFile(files[npmName])
	if e != nil {
		t.Fatal(e)
	}
	containerIDs := func() string {
		t.Helper()
		b, err := exec.CommandContext(ctx, "docker", "ps", "--filter", "label=com.docker.compose.project="+npmName, "--filter", "label=com.docker.compose.project="+appName, "--format", "{{.ID}}").Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	beforeIDs := containerIDs()
	var unrelated assistant.NPMHost
	if published {
		unrelated = assistant.NPMHost{Domains: []string{"unrelated.apps.casaos.test"}, Scheme: "http", Host: "192.0.2.1", Port: 8123, CertificateID: certID, SSLForced: true, Enabled: true, Locations: []stdjson.RawMessage{}, Meta: map[string]interface{}{}}
		if e = api.Call(ctx, "POST", "/nginx/proxy-hosts", unrelated, &unrelated); e != nil {
			t.Fatal(e)
		}
		if e = api.Call(ctx, "GET", "/nginx/proxy-hosts/"+strconv.Itoa(unrelated.ID), nil, &unrelated); e != nil {
			t.Fatal(e)
		}
	}
	publish()
	publish()
	if published {
		afterNPM, err := os.ReadFile(files[npmName])
		if err != nil || !bytes.Equal(beforeNPM, afterNPM) || beforeIDs != containerIDs() {
			t.Fatal("host-port publication changed NPM settings or recreated containers", err)
		}
		var readback assistant.NPMHost
		if e = api.Call(ctx, "GET", "/nginx/proxy-hosts/"+strconv.Itoa(unrelated.ID), nil, &readback); e != nil || npmSnapshotHost(&readback) != npmSnapshotHost(&unrelated) {
			t.Fatalf("unrelated proxy host changed: before=%s after=%s error=%v", npmSnapshotHost(&unrelated), npmSnapshotHost(&readback), e)
		}
	}
	api.Close()
	api, _, e = npmResolve(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	defer api.Close()
	hosts, e := api.Hosts(ctx)
	if e != nil {
		t.Fatal(e)
	}
	host, e := npmFindHost(hosts, "fixture.apps.casaos.test")
	wantCount := 1
	if published {
		wantCount++
	}
	if e != nil || len(hosts) != wantCount || host == nil || host.CertificateID != certID {
		t.Fatal("duplicate or invalid hosts", e)
	}
	if published {
		if net.ParseIP(host.Host) == nil || host.Port != hostPort {
			t.Fatal("incorrect host-port upstream", host)
		}
	} else if host.Host != appName+"-web" || host.Port != 8080 {
		t.Fatal("incorrect shared-network upstream", host)
	}
	active, npm, e := npmResolve(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	defer active.Close()
	addr, e := npmPublished(npm, 443)
	if e != nil {
		t.Fatal(e)
	}
	dialer := websocket.Dialer{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "fixture.apps.casaos.test"}, NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	ws, _, e := dialer.DialContext(ctx, "wss://fixture.apps.casaos.test/ws", nil)
	if e != nil {
		t.Fatal("WebSocket proxy", e)
	}
	ws.Close()
	// Recreate both containers; persistent Compose aliases must survive.
	for _, name := range []string{appName, npmName} {
		run("compose", "-f", files[name], "up", "-d", "--force-recreate")
	}
	var check string
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); {
		check, e = npmCheckSaved(ctx, "1", c, appName, "fixture.apps.casaos.test")
		if e == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if e != nil {
		t.Fatal("recreated route", check, e)
	}
	// The app's path remains intact and the card points to HTTPS.
	app, e := assistantLoad(appName)
	if e != nil {
		t.Fatal(e)
	}
	info, e := app.StoreInfo(false)
	if e != nil || *info.Hostname != "fixture.apps.casaos.test" || info.Index != "/library" {
		t.Fatal(info, e)
	}
	active.Close()
	active, _, e = npmResolve(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	defer active.Close()
	// Losing the POST response still recovers by the durable ownership marker.
	record, e := assistantNPMStore.Route("1", c, "fixture.apps.casaos.test")
	if e != nil {
		t.Fatal(e)
	}
	record.HostID = 0
	if e = assistantNPMStore.SaveRoute("1", c, record); e != nil {
		t.Fatal(e)
	}
	publish()
	// Enforce an existing Access List, and retain the authentication-required status.
	var access assistant.NPMAccessList
	if e = active.Call(ctx, "POST", "/nginx/access-lists", map[string]interface{}{"name": "Fixture authentication", "satisfy_any": false, "pass_auth": false, "items": []map[string]string{{"username": "fixture", "password": "test-access-password"}}, "clients": []interface{}{}}, &access); e != nil {
		t.Fatal(e)
	}
	c.AccessListID = access.ID
	if e = AssistantNPMSave(ctx, "1", c); e != nil {
		t.Fatal(e)
	}
	saved, e = assistantNPMStore.Connection("1")
	if e != nil {
		t.Fatal(e)
	}
	c = *saved
	publish()
	check, e = npmCheckSaved(ctx, "1", c, appName, "fixture.apps.casaos.test")
	if e != nil {
		t.Fatal(check, e)
	}
	var restricted npmProbeResult
	if stdjson.Unmarshal([]byte(check), &restricted) != nil || restricted.HTTPStatus != 401 || !restricted.Verified {
		t.Fatal("access policy not enforced", check)
	}
	// A changed certificate invalidates a prepared proposal before mutations.
	plan, e := (AssistantRuntime{}).Prepare(ctx, "publish_app", raw)
	if e != nil {
		t.Fatal(e)
	}
	if e = active.Call(ctx, "DELETE", "/nginx/certificates/"+strconv.Itoa(certID), nil, nil); e != nil {
		t.Fatal(e)
	}
	if _, e = plan.Execute(ctx); e == nil {
		t.Fatal("published after wildcard disappeared")
	}
	// Disconnect invalidates an independently prepared plan even after reconnect.
	if e = AssistantNPMDisconnect("1"); e != nil {
		t.Fatal(e)
	}
	if _, e = plan.Execute(ctx); e == nil {
		t.Fatal("disconnected plan executed")
	}
}
