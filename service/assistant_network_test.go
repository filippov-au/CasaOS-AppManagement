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

	"github.com/IceWhaleTech/CasaOS-AppManagement/common"
	"github.com/IceWhaleTech/CasaOS-Common/utils/logger"
)

func TestAssistantSharedNetworkPreservesImplicitDefaultAndRejectsHostMode(t *testing.T) {
	root := assistantTestRoot(t)
	for _, mode := range []string{"", "host"} {
		dir := filepath.Join(root, "fixture")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		extra := ""
		if mode != "" {
			extra = "    network_mode: " + mode + "\n"
		}
		file := filepath.Join(dir, common.ComposeYAMLFileName)
		if err := os.WriteFile(file, []byte("name: fixture\nservices:\n  web:\n    image: nginx:alpine\n"+extra), 0600); err != nil {
			t.Fatal(err)
		}
		app, err := assistantLoad("fixture")
		if err != nil {
			t.Fatal(err)
		}
		err = assistantSharedNetwork(app, 0, "casaos-ai-media")
		if mode == "host" {
			if err == nil {
				t.Fatal("changed host networking")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := app.Services[0].Networks["default"]; !ok {
			t.Fatal("lost default network")
		}
		if err = assistantSharedNetwork(app, 0, "casaos-ai-media"); err != nil {
			t.Fatal(err)
		}
		if len(app.Services[0].Networks["casaos-ai-media"].Aliases) != 1 {
			t.Fatal("duplicate alias")
		}
		if err = assistantSharedNetwork(app, 0, "bridge"); err == nil {
			t.Fatal("accepted system network")
		}
	}
}

func TestAssistantDockerSharedNetworkAcrossApps(t *testing.T) {
	if os.Getenv("CASAOS_ASSISTANT_NETWORK_DOCKER_TEST") != "1" {
		t.Skip("set CASAOS_ASSISTANT_NETWORK_DOCKER_TEST=1")
	}
	logger.LogInitConsoleOnly()
	root := assistantTestRoot(t)
	t.Setenv("DOCKER_API_VERSION", "1.44")
	endpoint, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_HOST", strings.TrimSpace(string(endpoint)))
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	prefix := fmt.Sprintf("casaos-ai-net-%d", time.Now().UnixNano())
	network := prefix + "-shared"
	apps := []string{prefix + "-a", prefix + "-b"}
	// Cleanup runs in reverse order: containers first, shared network last.
	t.Cleanup(func() {
		b, err := exec.Command("docker", "network", "rm", network).CombinedOutput()
		if err != nil {
			t.Errorf("remove test network: %v %s", err, b)
		}
	})
	for _, name := range apps {
		dir := filepath.Join(root, name)
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, common.ComposeYAMLFileName)
		content := fmt.Sprintf("name: %s\nservices:\n  web:\n    image: nginx:alpine\n    environment:\n      KEEP: 'value$$with$$dollars'\n    volumes:\n      - cache:/cache\nvolumes:\n  cache: {}\nx-casaos:\n  main: web\n", name)
		if err = os.WriteFile(file, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			b, err := exec.Command("docker", "compose", "-f", file, "down", "--volumes").CombinedOutput()
			if err != nil {
				t.Errorf("cleanup: %v %s", err, b)
			}
		})
		if b, err := exec.CommandContext(ctx, "docker", "compose", "-f", file, "up", "-d").CombinedOutput(); err != nil {
			t.Fatalf("start: %v %s", err, b)
		}
		if b, err := exec.CommandContext(ctx, "docker", "compose", "-f", file, "exec", "-T", "web", "touch", "/cache/preserved").CombinedOutput(); err != nil {
			t.Fatalf("write fixture sentinel: %v %s", err, b)
		}
		for i := 0; i < 2; i++ {
			plan, err := (AssistantRuntime{}).Prepare(ctx, "configure_service", []byte(fmt.Sprintf(`{"app":%q,"service":"web","shared_network":%q}`, name, network)))
			if err != nil {
				t.Fatal(err)
			}
			if out, err := plan.Execute(ctx); err != nil {
				t.Fatalf("join network: %v %s", err, out)
			}
		}
		app, err := assistantLoad(name)
		if err != nil {
			t.Fatal(err)
		}
		if len(app.Services[0].Networks) != 2 || len(app.Services[0].Volumes) != 1 || *app.Services[0].Environment["KEEP"] != "value$with$dollars" {
			t.Fatal("settings lost during network change")
		}
		// Compose recreation must retain both connectivity and persistent data.
		if b, err := exec.CommandContext(ctx, "docker", "compose", "-f", file, "up", "-d", "--force-recreate").CombinedOutput(); err != nil {
			t.Fatalf("recreate: %v %s", err, b)
		}
		if b, err := exec.CommandContext(ctx, "docker", "compose", "-f", file, "exec", "-T", "web", "test", "-f", "/cache/preserved").CombinedOutput(); err != nil {
			t.Fatalf("lost fixture data: %v %s", err, b)
		}
	}
	source := filepath.Join(root, apps[0], common.ComposeYAMLFileName)
	var output []byte
	for i := 0; i < 20; i++ {
		output, err = exec.CommandContext(ctx, "docker", "compose", "-f", source, "exec", "-T", "web", "wget", "-qO-", "http://"+apps[1]+"-web").CombinedOutput()
		if err == nil && strings.Contains(string(output), "Welcome to nginx") {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil || !strings.Contains(string(output), "Welcome to nginx") {
		t.Fatalf("cross-app DNS/HTTP failed: %v %s", err, output)
	}
}
