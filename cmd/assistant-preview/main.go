// assistant-preview runs the real assistant API locally for provider verification.
// Its Docker runtime exposes observation tools only, regardless of session mode.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/IceWhaleTech/CasaOS-AppManagement/internal/assistant"
	"github.com/IceWhaleTech/CasaOS-AppManagement/pkg/config"
	"github.com/IceWhaleTech/CasaOS-AppManagement/route"
	"github.com/IceWhaleTech/CasaOS-AppManagement/service"
	"github.com/IceWhaleTech/CasaOS-Common/external"
	"github.com/IceWhaleTech/CasaOS-Common/utils/jwt"
	"github.com/IceWhaleTech/CasaOS-Common/utils/logger"
	"golang.org/x/sys/unix"
)

type observationRuntime struct{ service.AssistantRuntime }

func (r observationRuntime) Tools() []assistant.Tool {
	result := []assistant.Tool{}
	for _, tool := range r.AssistantRuntime.Tools() {
		if !r.IsWrite(tool.Function.Name) {
			result = append(result, tool)
		}
	}
	return result
}
func (r observationRuntime) Prepare(ctx context.Context, name string, raw json.RawMessage) (assistant.Prepared, error) {
	if r.IsWrite(name) {
		return assistant.Prepared{}, errors.New("the local verification preview only permits inspection")
	}
	return r.AssistantRuntime.Prepare(ctx, name, raw)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	cache, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	state := flag.String("state", filepath.Join(cache, "CasaOS", "assistant-preview"), "Private settings and conversation directory")
	listen := flag.String("listen", "127.0.0.1:5190", "Loopback API address")
	origin := flag.String("ui-origin", "http://127.0.0.1:5189", "Local preview UI origin")
	apps := flag.String("apps-root", "", "Existing CasaOS Compose directory to inspect (optional)")
	flag.Parse()
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("preview API must listen on a loopback IP")
	}
	ui, err := url.Parse(*origin)
	if err != nil || ui.Scheme != "http" || net.ParseIP(ui.Hostname()) == nil || !net.ParseIP(ui.Hostname()).IsLoopback() || ui.User != nil || ui.RawQuery != "" || ui.Fragment != "" {
		return errors.New("preview UI must use a loopback HTTP origin")
	}
	if err = os.MkdirAll(*state, 0700); err != nil {
		return err
	}
	if err = os.Chmod(*state, 0700); err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(*state)
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(root, ".preview.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("another preview is using this state directory")
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	config.CommonInfo.RuntimePath = root
	config.AppInfo.AppsPath = filepath.Join(root, "apps")
	if *apps != "" {
		config.AppInfo.AppsPath, err = filepath.EvalSymlinks(*apps)
		if err != nil {
			return err
		}
	}
	if err = os.MkdirAll(config.AppInfo.AppsPath, 0700); err != nil {
		return err
	}
	if os.Getenv("DOCKER_HOST") == "" {
		endpoint, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
		if err != nil {
			return errors.New("could not read the Docker context; start Docker or set DOCKER_HOST")
		}
		if err = os.Setenv("DOCKER_HOST", strings.TrimSpace(string(endpoint))); err != nil {
			return err
		}
	}
	logger.LogInitConsoleOnly()
	privateKey, publicKey, err := jwt.GenerateKeyPair()
	if err != nil {
		return err
	}
	publicJSON, err := jwt.GenerateJwksJSON(publicKey)
	if err != nil {
		return err
	}
	keyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	keyServer := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(publicJSON)
	})}
	go func() { _ = keyServer.Serve(keyListener) }()
	defer keyServer.Close()
	if err = os.WriteFile(filepath.Join(root, external.UserServiceAddressFilename), []byte("http://"+keyListener.Addr().String()), 0600); err != nil {
		return err
	}
	service.Assistant = assistant.NewManager(assistant.ChatModel{}, observationRuntime{})
	// Keep preview keys/history inside its private state, even with real app configs.
	appPath := config.AppInfo.AppsPath
	config.AppInfo.AppsPath = filepath.Join(root, "apps")
	handler := route.InitV2Router()
	config.AppInfo.AppsPath = appPath
	access, err := jwt.GenerateToken("local-preview", privateKey, 1, "casaos", 12*time.Hour)
	if err != nil {
		return err
	}
	startup := map[string]string{"url": ui.String(), "api": "http://" + *listen, "state": root, "access_token": access}
	data, _ := json.MarshalIndent(startup, "", "  ")
	if err = os.WriteFile(filepath.Join(root, "startup.json"), data, 0600); err != nil {
		return err
	}
	server := &http.Server{Addr: *listen, ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, route.V2APIPath+"/assistant/") {
			http.NotFound(w, r)
			return
		}
		if from := r.Header.Get("Origin"); from != "" && from != *origin {
			http.Error(w, "unrecognized preview origin", http.StatusForbidden)
			return
		}
		handler.ServeHTTP(w, r)
	})}
	defer server.Close()
	fmt.Println("Local inspection preview is running. Open", ui.String())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	err = server.Serve(listener)
	for _, topic := range service.Assistant.List("1") {
		if topic.Status == "running" || topic.Status == "approval" {
			_, _ = service.Assistant.Cancel("1", topic.ID)
		}
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
