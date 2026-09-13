package route

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IceWhaleTech/CasaOS-AppManagement/common"
	"github.com/IceWhaleTech/CasaOS-AppManagement/internal/assistant"
	"github.com/IceWhaleTech/CasaOS-AppManagement/pkg/config"
	"github.com/IceWhaleTech/CasaOS-AppManagement/service"
	"github.com/IceWhaleTech/CasaOS-Common/external"
	"github.com/IceWhaleTech/CasaOS-Common/utils/jwt"
	"github.com/IceWhaleTech/CasaOS-Common/utils/logger"
)

type assistantRoundTrip func(*http.Request) (*http.Response, error)

func (f assistantRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercises the real JWT middleware, routes, provider wire format, manager and
// Docker runtime. Only the provider and user-service JWKS are local fixtures.
func TestAssistantAuthenticatedDockerWorkflow(t *testing.T) {
	if os.Getenv("CASAOS_ASSISTANT_HTTP_DOCKER_TEST") != "1" {
		t.Skip("set CASAOS_ASSISTANT_HTTP_DOCKER_TEST=1")
	}
	logger.LogInitConsoleOnly()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldApps, oldRuntime, oldAssistant := config.AppInfo.AppsPath, config.CommonInfo.RuntimePath, service.Assistant
	config.AppInfo.AppsPath, config.CommonInfo.RuntimePath = filepath.Join(root, "apps"), root
	t.Cleanup(func() {
		config.AppInfo.AppsPath, config.CommonInfo.RuntimePath, service.Assistant = oldApps, oldRuntime, oldAssistant
		service.InitAssistantSettings()
	})
	t.Setenv("DOCKER_API_VERSION", "1.44")
	endpoint, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_HOST", strings.TrimSpace(string(endpoint)))
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	name := fmt.Sprintf("casaos-ai-http-%d", time.Now().UnixNano())
	appDir := filepath.Join(config.AppInfo.AppsPath, name)
	if err = os.MkdirAll(appDir, 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	composePath := filepath.Join(appDir, common.ComposeYAMLFileName)
	compose := fmt.Sprintf("name: %s\nservices:\n  web:\n    image: nginx:alpine\n    ports:\n      - '127.0.0.1:%d:80'\nx-casaos:\n  main: web\n", name, port)
	if err = os.WriteFile(composePath, []byte(compose), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		b, err := exec.Command("docker", "compose", "-f", composePath, "down", "--volumes").CombinedOutput()
		if err != nil {
			t.Errorf("cleanup: %v %s", err, b)
		}
	})
	if b, err := exec.CommandContext(ctx, "docker", "compose", "-f", composePath, "up", "-d").CombinedOutput(); err != nil {
		t.Fatalf("start: %v %s", err, b)
	}
	privateKey, publicKey, err := jwt.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	jwks, err := jwt.GenerateJwksJSON(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwks)
	}))
	defer keys.Close()
	if err = os.WriteFile(filepath.Join(root, external.UserServiceAddressFilename), []byte(keys.URL), 0600); err != nil {
		t.Fatal(err)
	}
	token, err := jwt.GetAccessToken("fixture-owner", privateKey, 1)
	if err != nil {
		t.Fatal(err)
	}
	otherToken, err := jwt.GetAccessToken("other-owner", privateKey, 2)
	if err != nil {
		t.Fatal(err)
	}
	var modelMu sync.Mutex
	modelCalls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelMu.Lock()
		defer modelMu.Unlock()
		if r.Header.Get("Authorization") != "Bearer fixture-provider-key" {
			http.Error(w, "wrong provider authorization", 401)
			return
		}
		var request struct {
			Model    string              `json:"model"`
			Messages []assistant.Message `json:"messages"`
			Tools    []assistant.Tool    `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) == 0 || len(request.Tools) == 0 {
			http.Error(w, "invalid provider request", 400)
			return
		}
		modelCalls++
		msg := assistant.Message{Role: "assistant"}
		tool, args := "", ""
		switch modelCalls {
		case 1, 5:
			tool, args = "inspect_app", fmt.Sprintf(`{"app":%q}`, name)
		case 2:
			if !strings.Contains(request.Messages[len(request.Messages)-1].Content, "running") {
				http.Error(w, "missing inspection evidence", 400)
				return
			}
			tool, args = "configure_service", fmt.Sprintf(`{"app":%q,"service":"web","environment":{"TZ":"Australia/Melbourne"}}`, name)
		case 3:
			if !strings.Contains(request.Messages[len(request.Messages)-1].Content, "Settings applied") {
				http.Error(w, "missing apply evidence", 400)
				return
			}
			tool, args = "check_app", fmt.Sprintf(`{"app":%q,"port":%d}`, name, port)
		case 4:
			if !strings.Contains(request.Messages[len(request.Messages)-1].Content, "Welcome to nginx") {
				http.Error(w, "missing HTTP evidence", 400)
				return
			}
			msg.Content = "Timezone configured and the app web page was verified."
		default:
			msg.Content = "Continued from saved conversation; current app state was inspected."
		}
		if tool != "" {
			msg.Calls = []assistant.Call{{ID: fmt.Sprintf("call-%d", modelCalls), Type: "function", Function: assistant.Function{Name: tool, Arguments: args}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{map[string]interface{}{"message": msg}}})
	}))
	defer provider.Close()
	providerURL, _ := url.Parse(provider.URL)
	client := &http.Client{Transport: assistantRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.deepseek.com" {
			return nil, fmt.Errorf("unexpected provider host")
		}
		copy := r.Clone(r.Context())
		u := *r.URL
		u.Scheme, u.Host = providerURL.Scheme, providerURL.Host
		copy.URL = &u
		return http.DefaultTransport.RoundTrip(copy)
	})}
	newManager := func() *assistant.Manager {
		return assistant.NewManager(assistant.ChatModel{Client: client}, service.AssistantRuntime{})
	}
	service.Assistant = newManager()
	handler := InitV2Router()
	request := func(method, path, auth string, body interface{}, want int) []byte {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(method, V2APIPath+"/assistant"+path, bytes.NewReader(b))
		req.Header.Set("Authorization", auth)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("user_id", "1") // Forged owner must not override JWT identity.
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s: want %d got %d %s", method, path, want, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	decodeView := func(b []byte) assistant.View {
		t.Helper()
		var out struct {
			Data assistant.View `json:"data"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		return out.Data
	}
	wait := func(id, status string) assistant.View {
		t.Helper()
		deadline := time.Now().Add(time.Minute)
		for time.Now().Before(deadline) {
			v := decodeView(request("GET", "/sessions/"+id, token, nil, 200))
			if v.Status == status {
				return v
			}
			if v.Status == "error" {
				t.Fatalf("assistant failed: %+v", v)
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("assistant did not reach " + status)
		return assistant.View{}
	}
	settings := request("PUT", "/settings", token, map[string]interface{}{"provider": "deepseek", "model": assistant.Presets[0].Models[0], "api_key": "fixture-provider-key"}, 200)
	if strings.Contains(string(settings), "fixture-provider-key") {
		t.Fatal("provider key returned")
	}
	v := decodeView(request("POST", "/sessions", token, map[string]string{"message": "Inspect the app, set the timezone and verify its web page.", "mode": "review"}, 200))
	v = wait(v.ID, "approval")
	request("GET", "/sessions/"+v.ID, otherToken, nil, 404)
	request("POST", "/sessions/"+v.ID+"/decision", token, map[string]interface{}{"approval_id": strings.Repeat("0", 32), "allow": true}, 409)
	request("POST", "/sessions/"+v.ID+"/decision", token, map[string]interface{}{"approval_id": v.Pending.ID, "allow": true}, 200)
	v = wait(v.ID, "done")
	b, err := os.ReadFile(composePath)
	if err != nil || !strings.Contains(string(b), "Australia/Melbourne") {
		t.Fatal("approved configuration not persisted", err)
	}
	// Recreate both manager and router as on a service restart, retaining disk state.
	service.Assistant = newManager()
	handler = InitV2Router()
	if got := decodeView(request("GET", "/sessions/"+v.ID, token, nil, 200)); got.Status != "done" {
		t.Fatal("history not restored", got)
	}
	request("POST", "/sessions/"+v.ID+"/messages", token, map[string]string{"message": "Inspect the app again."}, 200)
	final := wait(v.ID, "done")
	if !strings.Contains(final.Events[len(final.Events)-1].Text, "Continued from saved conversation") {
		t.Fatal("restored chat did not continue", final)
	}
	modelMu.Lock()
	calls := modelCalls
	modelMu.Unlock()
	if calls != 6 {
		t.Fatal("unexpected model requests", calls)
	}
	request("DELETE", "/sessions/"+v.ID, token, nil, 200)
}
