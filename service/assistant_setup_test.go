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

func TestAssistantNZBMergePreservesCredentialsAndRejectsExecutionOptions(t *testing.T) {
	old := []assistantNZBOption{{Name: "ControlPassword", Value: "private"}, {Name: "Server1.Password", Value: "news-secret"}, {Name: "Category1.Name", Value: "old"}}
	merged, err := assistantNZBMerge(old, map[string]string{"Category1.Name": "tv", "Category1.DestDir": "/data/downloads/tv"})
	if err != nil {
		t.Fatal(err)
	}
	if merged[0].Value != "private" || merged[1].Value != "news-secret" || old[2].Value != "old" {
		t.Fatal("lost existing settings or mutated source")
	}
	for _, changes := range []map[string]string{{"ControlPassword": "change"}, {"Extensions": "bad"}, {"ScriptDir": "/tmp"}, {"Category1.Name": "tv\nControlPassword=oops"}} {
		if _, err = assistantNZBMerge(old, changes); err == nil {
			t.Fatal("unsafe settings accepted", changes)
		}
	}
}

func TestAssistantPlexClaimStateMustBeExplicit(t *testing.T) {
	for _, tt := range []struct {
		input   string
		claimed bool
		valid   bool
	}{
		{`<MediaContainer claimed="0"/>`, false, true},
		{`<MediaContainer claimed="1"/>`, true, true},
		{`{"MediaContainer":{"claimed":true}}`, true, true},
		{`{"MediaContainer":{"claimed":false}}`, false, true},
		{`{"MediaContainer":{"claimed":1}}`, true, true},
		{`{"MediaContainer":{}}`, false, false},
		{`<MediaContainer/>`, false, false},
		{`{"MediaContainer":{"claimed":"unknown"}}`, false, false},
	} {
		claimed, err := assistantPlexClaimed([]byte(tt.input))
		if (err == nil) != tt.valid || claimed != tt.claimed {
			t.Fatalf("claim state %s: %v %v", tt.input, claimed, err)
		}
	}
}
func TestAssistantPlexSectionsParsesJSONAndXML(t *testing.T) {
	for _, data := range []string{`{"MediaContainer":{"Directory":[{"key":"1","title":"TV","type":"show","Location":[{"path":"/data/media/tv"}]}]}}`, `<MediaContainer><Directory key="1" title="TV" type="show"><Location path="/data/media/tv"/></Directory></MediaContainer>`} {
		sections, err := assistantPlexSections([]byte(data))
		if err != nil || len(sections) != 1 || sections[0].Locations[0].Path != "/data/media/tv" {
			t.Fatal(sections, err)
		}
	}
}
func TestAssistantPlexDockerLibrary(t *testing.T) {
	if os.Getenv("CASAOS_ASSISTANT_PLEX_DOCKER_TEST") != "1" {
		t.Skip("set CASAOS_ASSISTANT_PLEX_DOCKER_TEST=1")
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
	name := fmt.Sprintf("casaos-ai-plex-%d", time.Now().UnixNano())
	dir := filepath.Join(root, name)
	prefs := filepath.Join(dir, "config", "Library", "Application Support", "Plex Media Server", "Preferences.xml")
	if e = os.MkdirAll(filepath.Dir(prefs), 0777); e != nil {
		t.Fatal(e)
	}
	// Fixture access is local-only (published port bound to loopback), avoiding a real Plex account in tests.
	if e = os.WriteFile(prefs, []byte(`<Preferences allowedNetworks="0.0.0.0/0" acceptedEULA="1" FriendlyName="CasaOS disposable test"/>`), 0600); e != nil {
		t.Fatal(e)
	}
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	file := filepath.Join(dir, common.ComposeYAMLFileName)
	content := fmt.Sprintf(`name: %s
services:
  plex:
    image: lscr.io/linuxserver/plex:latest
    environment:
      PUID: '1000'
      PGID: '1000'
      TZ: UTC
      VERSION: docker
    volumes:
      - %s/config:/config
      - %s/data:/data
    ports:
      - '127.0.0.1:%d:32400'
x-casaos:
  main: plex
`, name, dir, dir, port)
	if e = os.WriteFile(file, []byte(content), 0600); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		b, e := exec.Command("docker", "compose", "-f", file, "-p", name, "down", "--volumes").CombinedOutput()
		if e != nil {
			t.Errorf("cleanup: %v %s", e, b)
		}
	})
	b, e := exec.CommandContext(ctx, "docker", "compose", "-f", file, "up", "-d").CombinedOutput()
	if e != nil {
		t.Fatalf("start: %v %s", e, b)
	}
	r := AssistantRuntime{}
	ready := false
	for i := 0; i < 90; i++ {
		p, e := r.Prepare(ctx, "media_read", []byte(fmt.Sprintf(`{"app":%q,"service":"plex","resource":"libraries"}`, name)))
		if e == nil {
			_, e = p.Execute(ctx)
		}
		if e == nil {
			ready = true
			break
		}
		time.Sleep(time.Second)
	}
	if !ready {
		t.Fatal("Plex did not become ready")
	}
	run := func(tool string, args string) string {
		t.Helper()
		p, e := r.Prepare(ctx, tool, []byte(args))
		if e != nil {
			t.Fatal(tool, e)
		}
		out, e := p.Execute(ctx)
		if e != nil {
			t.Fatal(tool, e, out)
		}
		return out
	}
	run("create_app_directory", fmt.Sprintf(`{"app":%q,"service":"plex","path":"/data/media/tv"}`, name))
	setup := run("media_read", fmt.Sprintf(`{"app":%q,"service":"plex","resource":"setup"}`, name))
	if !strings.Contains(setup, `"claimed": false`) || !strings.Contains(setup, "https://plex.tv/claim") || !strings.Contains(setup, "$secret:plex_claim") {
		t.Fatal("unclaimed Plex did not provide private claim guidance", setup)
	}
	args := fmt.Sprintf(`{"app":%q,"service":"plex","name":"TV Test","path":"/data/media/tv","type":"show"}`, name)
	run("add_plex_library", args)
	if out := run("add_plex_library", args); !strings.Contains(out, "already configured") {
		t.Fatal("created duplicate library", out)
	}
	out := run("media_read", fmt.Sprintf(`{"app":%q,"service":"plex","resource":"libraries"}`, name))
	if !strings.Contains(out, "TV Test") {
		t.Fatal("missing library", out)
	}
}
