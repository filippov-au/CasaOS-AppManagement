package service

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IceWhaleTech/CasaOS-AppManagement/common"
	"github.com/IceWhaleTech/CasaOS-Common/utils/logger"
)

func TestAssistantMediaRedactsCredentialFields(t *testing.T) {
	text := assistantMediaRedact([]byte(`{"fields":[{"name":"password","value":"hidden-pass"},{"name":"apiKey","value":"hidden-key"}],"options":[{"Name":"Server1.Password","Value":"news-secret"}],"token":"plex-secret"}`), nil)
	for _, value := range []string{"hidden-pass", "hidden-key", "news-secret", "plex-secret"} {
		if strings.Contains(text, value) {
			t.Fatal("credential leaked", text)
		}
	}
}
func TestAssistantMediaDockerConnection(t *testing.T) {
	if os.Getenv("CASAOS_ASSISTANT_MEDIA_DOCKER_TEST") != "1" {
		t.Skip("set CASAOS_ASSISTANT_MEDIA_DOCKER_TEST=1")
	}
	logger.LogInitConsoleOnly()
	root := assistantTestRoot(t)
	t.Setenv("DOCKER_API_VERSION", "1.44")
	endpoint, e := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("DOCKER_HOST", strings.TrimSpace(string(endpoint)))
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	name := fmt.Sprintf("casaos-ai-media-%d", time.Now().UnixNano())
	dir := filepath.Join(root, name)
	if e = os.MkdirAll(filepath.Join(dir, "data", "downloads", "tv"), 0777); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(dir, common.ComposeYAMLFileName)
	port := func() int {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}
	sonarrPort, nzbPort := port(), port()
	fixtureScript, e := filepath.Abs("testdata/assistant_media_server.py")
	if e != nil {
		t.Fatal(e)
	}
	content := fmt.Sprintf(`name: %s
services:
  fixture:
    image: python:3.13-alpine
    command: ["python", "/fixture/server.py"]
    volumes:
      - %s:/fixture/server.py:ro
  sonarr:
    image: lscr.io/linuxserver/sonarr:latest
    environment:
      PUID: '1000'
      PGID: '1000'
      TZ: UTC
    volumes:
      - %s/sonarr:/config
      - %s/data:/data
    ports:
      - '127.0.0.1:%d:8989'
  nzbget:
    image: linuxserver/nzbget:26.1.20260606
    environment:
      PUID: '1000'
      PGID: '1000'
      TZ: UTC
    volumes:
      - %s/nzbget:/config
      - %s/data:/data
    ports:
      - '127.0.0.1:%d:6789'
x-casaos:
  main: sonarr
`, name, fixtureScript, dir, dir, sonarrPort, dir, dir, nzbPort)
	if e = os.WriteFile(path, []byte(content), 0600); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		b, e := exec.Command("docker", "compose", "-f", path, "-p", name, "down", "--volumes").CombinedOutput()
		if e != nil {
			t.Errorf("cleanup: %v %s", e, b)
		}
	})
	b, e := exec.CommandContext(ctx, "docker", "compose", "-f", path, "up", "-d").CombinedOutput()
	if e != nil {
		t.Fatalf("fixture start: %v %s", e, b)
	}
	r := AssistantRuntime{}
	for _, service := range []string{"sonarr", "nzbget"} {
		ready := false
		var last error
		for i := 0; i < 90; i++ {
			p, e := r.Prepare(ctx, "media_read", []byte(fmt.Sprintf(`{"app":%q,"service":%q,"resource":"status"}`, name, service)))
			if e == nil {
				_, e = p.Execute(ctx)
			}
			if e == nil {
				ready = true
				break
			}
			last = e
			time.Sleep(time.Second)
		}
		if !ready {
			t.Fatalf("%s did not become ready: %v", service, last)
		}
	}
	runSetup := func(name string, raw []byte) string {
		t.Helper()
		plan, e := r.Prepare(ctx, name, raw)
		if e != nil {
			t.Fatalf("%s prepare: %v", name, e)
		}
		out, e := plan.Execute(ctx)
		if e != nil {
			t.Fatalf("%s execute: %v %s", name, e, out)
		}
		return out
	}
	for _, folder := range []string{"/data/downloads/tv", "/data/downloads/completed", "/data/downloads/intermediate"} {
		runSetup("create_app_directory", []byte(fmt.Sprintf(`{"app":%q,"service":"nzbget","path":%q}`, name, folder)))
	}
	runSetup("create_app_directory", []byte(fmt.Sprintf(`{"app":%q,"service":"sonarr","path":"/data/media/tv"}`, name)))
	runSetup("add_sonarr_root_folder", []byte(fmt.Sprintf(`{"app":%q,"service":"sonarr","path":"/data/media/tv"}`, name)))
	runSetup("add_sonarr_root_folder", []byte(fmt.Sprintf(`{"app":%q,"service":"sonarr","path":"/data/media/tv"}`, name)))
	options := []byte(fmt.Sprintf(`{"app":%q,"service":"nzbget","options":{"Category1.Name":"tv","Category1.DestDir":"/data/downloads/tv","MainDir":"/data/downloads","DestDir":"/data/downloads/completed","InterDir":"/data/downloads/intermediate"}}`, name))
	runSetup("configure_nzbget", options)
	newsOptions := []byte(fmt.Sprintf(`{"app":%q,"service":"nzbget","options":{"Server1.Active":"yes","Server1.Name":"Fixture NNTP","Server1.Host":"fixture","Server1.Port":"119","Server1.Username":"fixture-user","Server1.Password":"fixture-password","Server1.Encryption":"no","Server1.Connections":"1"}}`, name))
	runSetup("configure_nzbget", newsOptions)
	indexer := []byte(fmt.Sprintf(`{"app":%q,"service":"sonarr","name":"Fixture indexer","url":"http://fixture:8080","api_key":"fixture-key"}`, name))
	runSetup("configure_sonarr_indexer", indexer)
	runSetup("configure_sonarr_indexer", indexer)
	// Reapplying configuration is harmless and credentials must remain intact.
	if out := runSetup("configure_nzbget", options); !strings.Contains(out, "without reloading") {
		t.Fatal("unchanged settings reloaded NZBGet", out)
	}
	nzb, err := assistantMediaResolve(ctx, name, "nzbget")
	if err != nil {
		t.Fatal(err)
	}

	args := []byte(fmt.Sprintf(`{"sonarr_app":%q,"sonarr_service":"sonarr","nzbget_app":%q,"nzbget_service":"nzbget","category":"tv"}`, name, name))
	var output string
	for i := 0; i < 20; i++ {
		plan, e := r.Prepare(ctx, "connect_sonarr_nzbget", args)
		if e == nil {
			output, e = plan.Execute(ctx)
		}
		if e == nil {
			break
		}
		err = e
		time.Sleep(time.Second)
	}
	if !strings.Contains(output, "saved the download client") {
		t.Fatalf("link failed: %v %s", err, output)
	}
	p, err := r.Prepare(ctx, "media_read", []byte(fmt.Sprintf(`{"app":%q,"service":"sonarr","resource":"download_clients"}`, name)))
	if err != nil {
		t.Fatal(err)
	}
	output, err = p.Execute(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "CasaOS NZBGet") {
		t.Fatal("download client was not persisted", output)
	}
	if len(nzb.password) >= 4 && strings.Contains(output, nzb.password) {
		t.Fatal("NZBGet credential leaked")
	}
	// Repeating the setup updates the same named client, and tests connectivity again.
	p, err = r.Prepare(ctx, "connect_sonarr_nzbget", args)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Execute(ctx); err != nil {
		t.Fatal(err)
	}
}
