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
	"github.com/IceWhaleTech/CasaOS-AppManagement/pkg/config"
	"github.com/IceWhaleTech/CasaOS-Common/utils/logger"
)

func assistantTestRoot(t *testing.T) string {
	t.Helper()
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	old := config.AppInfo.AppsPath
	config.AppInfo.AppsPath = root
	t.Cleanup(func() { config.AppInfo.AppsPath = old })
	return root
}
func TestAssistantRejectsUnsafeStack(t *testing.T) {
	logger.LogInitConsoleOnly()
	assistantTestRoot(t)
	for _, raw := range []string{
		`{"app":"../escape","services":[]}`,
		`{"app":"fixture","services":[{"name":"web","image":"nginx:alpine","privileged":true}]}`,
		`{"app":"fixture","services":[{"name":"web","image":"nginx:alpine","volumes":[{"source":"/var/run/docker.sock","target":"/docker.sock"}]}]}`,
		`{"app":"fixture","services":[{"name":"web","image":"nginx:alpine","volumes":[{"source":"/DATA/../etc","target":"/config"}]}]}`,
		`{"app":"fixture","services":[{"name":"web","image":"nginx","volumes":[]}]}`,
	} {
		if _, e := (AssistantRuntime{}).Prepare(context.Background(), "install_stack", []byte(raw)); e == nil {
			t.Fatal("unsafe plan accepted", raw)
		}
	}
}
func TestAssistantProbeRejectsUnownedPortAndStripsScripts(t *testing.T) {
	if _, e := assistantProbe(context.Background(), nil, 80, nil); e == nil {
		t.Fatal("accepted arbitrary port")
	}
	text := assistantPageText([]byte(`<html><title>Hello</title><script>steal()</script><style>hide</style><body>Ready</body></html>`))
	if strings.Contains(text, "steal") || strings.Contains(text, "hide") || !strings.Contains(text, "Ready") {
		t.Fatal(text)
	}
}
func TestAssistantConfigureRejectsStalePlan(t *testing.T) {
	logger.LogInitConsoleOnly()
	root := assistantTestRoot(t)
	dir := filepath.Join(root, "fixture")
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(dir, common.ComposeYAMLFileName)
	content := []byte("name: fixture\nservices:\n  web:\n    image: nginx:alpine\n")
	if e := os.WriteFile(path, content, 0600); e != nil {
		t.Fatal(e)
	}
	p, e := (AssistantRuntime{}).Prepare(context.Background(), "configure_service", []byte(`{"app":"fixture","service":"web","environment":{"TZ":"UTC"}}`))
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, append(content, []byte("# changed\n")...), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = p.Execute(context.Background()); e == nil || !strings.Contains(e.Error(), "changed since") {
		t.Fatal("stale plan executed", e)
	}
}

// Creates and removes only a uniquely named disposable project. Existing apps are untouched.
func TestAssistantDockerLifecycle(t *testing.T) {
	if os.Getenv("CASAOS_ASSISTANT_DOCKER_TEST") != "1" {
		t.Skip("set CASAOS_ASSISTANT_DOCKER_TEST=1")
	}
	logger.LogInitConsoleOnly()
	root := assistantTestRoot(t)
	t.Setenv("DOCKER_API_VERSION", "1.44")
	endpoint, e := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("DOCKER_HOST", strings.TrimSpace(string(endpoint)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	name := fmt.Sprintf("casaos-ai-test-%d", time.Now().UnixNano())
	path := filepath.Join(root, name, common.ComposeYAMLFileName)
	t.Cleanup(func() {
		b, e := exec.Command("docker", "compose", "-f", path, "-p", name, "down", "--volumes").CombinedOutput()
		if e != nil {
			t.Errorf("cleanup: %s %v", b, e)
		}
	})
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	r := AssistantRuntime{}
	run := func(tool, args string) string {
		t.Helper()
		p, e := r.Prepare(ctx, tool, []byte(args))
		if e != nil {
			t.Fatalf("%s prepare: %v", tool, e)
		}
		out, e := p.Execute(ctx)
		if e != nil {
			t.Fatalf("%s execute: %v\n%s", tool, e, out)
		}
		return out
	}
	run("install_stack", fmt.Sprintf(`{"app":%q,"services":[{"name":"web","image":"nginx:alpine","volumes":[],"environment":{"EXAMPLE_PASSWORD":"fixture-password"},"ports":[{"host":%d,"container":80}]}]}`, name, port))
	args := fmt.Sprintf(`{"app":%q}`, name)
	inspected := run("inspect_app", args)
	if !strings.Contains(inspected, "running") || strings.Contains(inspected, "fixture-password") {
		t.Fatal(inspected)
	}
	check := fmt.Sprintf(`{"app":%q,"port":%d}`, name, port)
	var page string
	for i := 0; i < 30; i++ {
		p, e := r.Prepare(ctx, "check_app", []byte(check))
		if e != nil {
			t.Fatal(e)
		}
		page, e = p.Execute(ctx)
		if e == nil && strings.Contains(page, "Welcome to nginx") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(page, "Welcome to nginx") {
		t.Fatal("web probe failed", page)
	}
	logs := run("app_logs", args)
	if !strings.Contains(logs, "web:") {
		t.Fatal("missing logs", logs)
	}
	run("configure_service", fmt.Sprintf(`{"app":%q,"service":"web","environment":{"TZ":"Australia/Melbourne","LITERAL":"value$with$dollars"}}`, name))
	inspected = run("inspect_app", args)
	if !strings.Contains(inspected, "Australia/Melbourne") || !strings.Contains(inspected, "value$with$dollars") {
		t.Fatal("config not applied", inspected)
	}
	if _, e = os.Stat(path + ".assistant.bak"); e != nil {
		t.Fatal("missing backup", e)
	}
	run("restart_app", args)
	page = run("check_app", check)
	if !strings.Contains(page, "Welcome to nginx") {
		t.Fatal("not healthy after restart", page)
	}
	// A saved app must be recoverable when no containers survived installation.
	b, e := exec.CommandContext(ctx, "docker", "compose", "-f", path, "down").CombinedOutput()
	if e != nil {
		t.Fatalf("remove disposable containers: %v %s", e, b)
	}
	run("retry_app", args)
	run("configure_service", fmt.Sprintf(`{"app":%q,"service":"web","image":"nginx:alpine"}`, name))
	inspected = run("inspect_app", args)
	if !strings.Contains(inspected, "running") || !strings.Contains(inspected, "value$with$dollars") {
		t.Fatal("retry or image configuration lost saved settings", inspected)
	}
}
