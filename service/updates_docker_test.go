package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IceWhaleTech/CasaOS-Common/utils/logger"
)

// Opt-in integration test; all containers and bind mounts belong to a disposable project.
func TestDockerUpdateAndRollback(t *testing.T) {
	if os.Getenv("CASAOS_UPDATE_DOCKER_TEST") != "1" {
		t.Skip("set CASAOS_UPDATE_DOCKER_TEST=1 to run against Docker")
	}
	logger.LogInitConsoleOnly()
	t.Setenv("DOCKER_API_VERSION", "1.44") // Docker 29 no longer serves the old client's default API.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.Mkdir(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dataDir, "marker")
	if err := os.WriteFile(marker, []byte("data survives update and rollback"), 0644); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("casaos-update-test-%d", time.Now().UnixNano())
	configFile := filepath.Join(dir, "compose.yaml")
	content := fmt.Sprintf(`name: %s
services:
  web:
    image: busybox:1.36
    command: ["sleep", "86400"]
    environment:
      CUSTOM: 'literal$$value'
    volumes:
      - %s:/data
      - /anonymous
    healthcheck:
      test: ["CMD", "test", "-f", "/data/marker"]
      interval: 1s
      timeout: 1s
      retries: 5
  db:
    image: busybox:1.36
    command: ["sleep", "86400"]
    volumes:
      - %s:/data
x-casaos:
  main: web
  store_app_id: fixture
`, name, dataDir, dataDir)
	if err := os.WriteFile(configFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	t.Cleanup(func() {
		b, err := exec.Command("docker", "compose", "-f", configFile, "-p", name, "down", "--volumes").CombinedOutput()
		if err != nil {
			t.Errorf("fixture cleanup: %v %s", err, b)
		}
	})
	run("compose", "-f", configFile, "up", "-d", "--wait")
	app, err := LoadComposeAppFromConfigFile(name, configFile)
	if err != nil {
		t.Fatal(err)
	}
	runtime := dockerUpdateRuntime{}
	run("compose", "-f", configFile, "exec", "-T", "web", "sh", "-c", "echo original > /anonymous/marker")
	original, err := runtime.Capture(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Release(context.Background(), original) })
	target, err := cloneCompose(app)
	if err != nil {
		t.Fatal(err)
	}
	for i := range target.Services {
		target.Services[i].Image = "busybox:1.37"
		target.Services[i].PullPolicy = "never"
	}
	if err := runtime.Pull(ctx, target); err != nil {
		t.Fatal(err)
	}
	yaml, err := resolvedComposeYAML(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Apply(ctx, app, yaml); err != nil {
		t.Fatal(err)
	}
	newImage := run("image", "inspect", "busybox:1.37", "--format", "{{.Id}}")
	for _, service := range []string{"web", "db"} {
		id := run("compose", "-f", configFile, "ps", "-q", service)
		if actual := run("inspect", id, "--format", "{{.Image}}"); actual != newImage {
			t.Fatalf("%s did not update: %s", service, actual)
		}
	}
	// Simulate post-update app writes. A version-only rollback must keep them.
	if err := os.WriteFile(marker, []byte("new data stays"), 0644); err != nil {
		t.Fatal(err)
	}
	run("compose", "-f", configFile, "exec", "-T", "web", "sh", "-c", "echo updated > /anonymous/marker")
	if err := runtime.Apply(ctx, app, original.YAML); err != nil {
		t.Fatal(err)
	}
	oldImage := run("image", "inspect", "busybox:1.36", "--format", "{{.Id}}")
	for _, service := range []string{"web", "db"} {
		id := run("compose", "-f", configFile, "ps", "-q", service)
		if actual := run("inspect", id, "--format", "{{.Image}}"); actual != oldImage {
			t.Fatalf("%s did not revert: %s", service, actual)
		}
	}
	if value := run("compose", "-f", configFile, "exec", "-T", "web", "printenv", "CUSTOM"); value != "literal$value" {
		t.Fatalf("environment changed: %q", value)
	}
	if value := run("compose", "-f", configFile, "exec", "-T", "web", "cat", "/data/marker"); value != "new data stays" {
		t.Fatalf("data changed: %q", value)
	}
	if value := run("compose", "-f", configFile, "exec", "-T", "web", "cat", "/anonymous/marker"); value != "updated" {
		t.Fatalf("anonymous volume changed: %q", value)
	}
	// External anonymous volumes are intentionally not removed by Compose. Clean up this fixture explicitly after down.
	anonymous := run("compose", "-f", configFile, "ps", "-q", "web")
	volume := run("inspect", anonymous, "--format", `{{range .Mounts}}{{if eq .Destination "/anonymous"}}{{.Name}}{{end}}{{end}}`)
	run("compose", "-f", configFile, "down", "--volumes")
	run("volume", "rm", volume)

}
