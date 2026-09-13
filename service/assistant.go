package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/IceWhaleTech/CasaOS-AppManagement/common"
	"github.com/IceWhaleTech/CasaOS-AppManagement/internal/assistant"
	"github.com/IceWhaleTech/CasaOS-AppManagement/pkg/config"
	composeTypes "github.com/compose-spec/compose-go/types"
	reference "github.com/docker/distribution/reference"
	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"golang.org/x/net/html"
)

type AssistantRuntime struct{}

var Assistant = assistant.NewManager(assistant.ChatModel{}, AssistantRuntime{})

// One persistent store mutex serializes read/replace of credentials across requests.
var assistantSettingsStore = &assistant.SettingsStore{}

func GetAssistantSettings() *assistant.SettingsStore { return assistantSettingsStore }
func InitAssistantSettings() {
	assistantSettingsStore.Root = filepath.Join(filepath.Dir(config.AppInfo.AppsPath), "assistant")
	initAssistantNPM()
	Assistant.ResolveSettings = assistantSettingsStore.ReadProvider
	_ = Assistant.EnableHistory(filepath.Join(assistantSettingsStore.Root, "history"), assistantSettingsStore.Read)
}

func schema(properties map[string]interface{}, required ...string) map[string]interface{} {
	if required == nil {
		required = []string{}
	}
	return map[string]interface{}{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func str() map[string]interface{} { return map[string]interface{}{"type": "string"} }
func tool(name, description string, params map[string]interface{}) assistant.Tool {
	return assistant.Tool{Type: "function", Function: assistant.ToolFunction{Name: name, Description: description, Parameters: params}}
}
func (AssistantRuntime) Tools() []assistant.Tool {
	app := schema(map[string]interface{}{"app": str()}, "app")
	port := schema(map[string]interface{}{"host": map[string]interface{}{"type": "integer"}, "container": map[string]interface{}{"type": "integer"}}, "host", "container")
	volume := schema(map[string]interface{}{"source": str(), "target": str(), "read_only": map[string]interface{}{"type": "boolean"}}, "source", "target")
	env := map[string]interface{}{"type": "object", "additionalProperties": map[string]interface{}{"type": "string"}}
	service := schema(map[string]interface{}{"name": str(), "image": str(), "environment": env, "ports": map[string]interface{}{"type": "array", "items": port}, "volumes": map[string]interface{}{"type": "array", "items": volume}}, "name", "image", "volumes")
	return append([]assistant.Tool{
		tool("list_apps", "List Docker containers with Compose app/service names, status and published ports.", schema(map[string]interface{}{})),
		tool("inspect_app", "Inspect a CasaOS Compose app's settings and container status. Secrets are redacted.", app),
		tool("app_logs", "Read the last 100 lines of each app container's logs, with bounded output and secret redaction.", app),
		tool("check_app", "Open a published HTTP port of this app and inspect its status, title and page text. Only this app's observed published TCP ports are allowed. No redirects or scripts are followed.", schema(map[string]interface{}{"app": str(), "port": map[string]interface{}{"type": "integer"}}, "app", "port")),
		tool("install_stack", "Install one or several cooperating services as a named CasaOS Compose app. Use persistent host mounts beneath /DATA, shared /data paths, and explicit image tags. Existing apps and data are never replaced. Then inspect logs and check each web interface.", schema(map[string]interface{}{"app": str(), "services": map[string]interface{}{"type": "array", "items": service}}, "app", "services")),
		tool("configure_service", "Merge environment values and optionally replace published TCP ports, change the image, or add/update bind mounts by container target. Set shared_network to a casaos-ai- prefixed name to connect separately installed apps through a persistent internal bridge; join each participating service to the same name. Preserves existing networks and adds unique app-service DNS aliases. Keeps unspecified settings and mounts, backs up Compose, and attempts recovery if applying fails. Mount changes do not move or delete existing data. Inspect current settings first.", schema(map[string]interface{}{"app": str(), "service": str(), "shared_network": str(), "image": str(), "environment": env, "ports": map[string]interface{}{"type": "array", "items": port}, "volumes": map[string]interface{}{"type": "array", "items": volume}}, "app", "service")),
		tool("retry_app", "Pull the saved app images and start or reconcile its saved Compose configuration, including a partially failed installation. Uses the app lock and rejects changed settings. Does not remove volumes or data. Inspect logs and web endpoints afterward.", app),
		tool("restart_app", "Restart all containers in a CasaOS app, preserving data and configuration. Check logs and endpoints afterward.", app),
	}, append(append(assistantMediaTools(), assistantSetupTools()...), assistantNPMTools()...)...)
}
func (AssistantRuntime) IsWrite(name string) bool {
	return name == "publish_app" || name == "retry_app" || name == "create_app_directory" || name == "configure_nzbget" || name == "add_sonarr_root_folder" || name == "configure_sonarr_indexer" || name == "add_plex_library" || name == "connect_sonarr_nzbget" || name == "install_stack" || name == "configure_service" || name == "restart_app"
}

type assistantPort struct {
	Host      int `json:"host"`
	Container int `json:"container"`
}
type assistantVolume struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}
type assistantService struct {
	Name        string            `json:"name"`
	Image       string            `json:"image"`
	Environment map[string]string `json:"environment"`
	Ports       []assistantPort   `json:"ports"`
	Volumes     []assistantVolume `json:"volumes"`
}
type assistantArgs struct {
	SharedNetwork string             `json:"shared_network,omitempty"`
	Image         string             `json:"image,omitempty"`
	Volumes       *[]assistantVolume `json:"volumes,omitempty"`
	App           string             `json:"app"`
	Service       string             `json:"service"`
	Port          int                `json:"port"`
	Environment   map[string]string  `json:"environment"`
	Ports         *[]assistantPort   `json:"ports"`
	Services      []assistantService `json:"services"`
}

var assistantName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
var assistantEnv = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var secretKey = regexp.MustCompile(`(?i)password|passwd|token|secret|api.?key|authorization|credential|claim`)

func assistantDecode(raw stdjson.RawMessage, out interface{}) error {
	d := stdjson.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return errors.New("invalid tool arguments: " + err.Error())
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("unexpected trailing arguments")
	}
	return nil
}
func assistantLoad(id string) (*ComposeApp, error) {
	if !assistantName.MatchString(id) {
		return nil, errors.New("invalid app name")
	}
	path := filepath.Join(config.AppInfo.AppsPath, id, common.ComposeYAMLFileName)
	if err := assistantNoSymlink(path); err != nil {
		return nil, err
	}
	a, err := LoadComposeAppFromConfigFile(id, path)
	if err != nil {
		return nil, err
	}
	if a.Name != id {
		return nil, errors.New("Compose app name does not match")
	}
	return a, nil
}
func assistantNoSymlink(path string) error {
	for path != "/" && path != "." {
		st, err := os.Lstat(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && st.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlinks are not allowed for assistant-managed paths")
		}
		path = filepath.Dir(path)
	}
	return nil
}
func assistantSecrets(a *ComposeApp) []string {
	var values []string
	for _, s := range a.Services {
		for k, v := range s.Environment {
			if secretKey.MatchString(k) && v != nil {
				values = append(values, *v)
			}
		}
	}
	return values
}
func assistantJSON(v interface{}) string {
	b, _ := stdjson.MarshalIndent(v, "", "  ")
	return string(b)
}
func assistantContainers(ctx context.Context, cli client.APIClient, id string) ([]dockerTypes.Container, error) {
	f := filters.NewArgs()
	if id != "" {
		f.Add("label", "com.docker.compose.project="+id)
	}
	return cli.ContainerList(ctx, dockerTypes.ContainerListOptions{All: true, Filters: f})
}
func assistantContainerViews(items []dockerTypes.Container) []map[string]interface{} {
	result := []map[string]interface{}{}
	for _, c := range items {
		result = append(result, map[string]interface{}{"id": c.ID[:12], "names": c.Names, "app": c.Labels["com.docker.compose.project"], "service": c.Labels["com.docker.compose.service"], "image": c.Image, "state": c.State, "status": c.Status, "ports": c.Ports})
	}
	return result
}
func (r AssistantRuntime) Prepare(ctx context.Context, name string, raw stdjson.RawMessage) (assistant.Prepared, error) {
	if name == "npm_inspect" || name == "publish_app" || name == "check_app_url" {
		return assistantNPMPlan(ctx, name, raw)
	}
	switch name {
	case "create_app_directory":
		return assistantDirectoryPlan(ctx, raw)
	case "configure_nzbget":
		return assistantNZBPlan(ctx, raw)
	case "add_sonarr_root_folder":
		return assistantSonarrRootPlan(ctx, raw)
	case "configure_sonarr_indexer":
		return assistantSonarrIndexerPlan(ctx, raw)
	case "add_plex_library":
		return assistantPlexLibraryPlan(ctx, raw)
	}

	if name == "media_read" {
		return assistantMediaReadPlan(ctx, raw)
	}
	if name == "connect_sonarr_nzbget" {
		return assistantConnectPlan(ctx, raw)
	}
	var a assistantArgs
	if err := assistantDecode(raw, &a); err != nil {
		return assistant.Prepared{}, err
	}
	if name != "list_apps" && !assistantName.MatchString(a.App) {
		return assistant.Prepared{}, errors.New("invalid app name")
	}
	if name == "install_stack" {
		return assistantInstallPlan(a)
	}
	var app *ComposeApp
	var err error
	if name != "list_apps" {
		app, err = assistantLoad(a.App)
		if err != nil {
			return assistant.Prepared{}, err
		}
	}
	if name == "retry_app" {
		return assistantRetryPlan(app)
	}
	if name == "configure_service" {
		return assistantConfigurePlan(app, a)
	}
	allowed := name == "list_apps" || name == "inspect_app" || name == "app_logs" || name == "check_app" || name == "restart_app"
	if !allowed {
		return assistant.Prepared{}, errors.New("unknown tool")
	}
	return assistant.Prepared{Summary: name + " " + a.App, Execute: func(ctx context.Context) (string, error) {
		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return "", err
		}
		defer cli.Close()
		items, err := assistantContainers(ctx, cli, a.App)
		if err != nil {
			return "", err
		}
		switch name {
		case "list_apps":
			return assistantJSON(assistantContainerViews(items)), nil
		case "inspect_app":
			// Reload at execution time; observations must reflect current state.
			current, err := assistantLoad(a.App)
			if err != nil {
				return "", err
			}
			views := []map[string]interface{}{}
			for _, s := range current.Services {
				env := map[string]string{}
				for k, v := range s.Environment {
					if v != nil {
						env[k] = *v
						if secretKey.MatchString(k) {
							env[k] = "[redacted]"
						}
					}
				}
				views = append(views, map[string]interface{}{"name": s.Name, "image": s.Image, "environment": env, "volumes": s.Volumes, "ports": s.Ports, "networks": s.Networks})
			}
			return assistant.Redact(assistantJSON(map[string]interface{}{"app": a.App, "services": views, "containers": assistantContainerViews(items)}), assistantSecrets(current)...), nil
		case "app_logs":
			var output strings.Builder
			for _, c := range items {
				if output.Len() >= 20000 {
					break
				}
				stream, err := cli.ContainerLogs(ctx, c.ID, dockerTypes.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: "100"})
				if err != nil {
					return "", err
				}
				info, err := cli.ContainerInspect(ctx, c.ID)
				if err != nil {
					stream.Close()
					return "", err
				}
				buf := &assistantLogWriter{limit: 20000 - output.Len()}
				if info.Config.Tty {
					_, err = io.Copy(buf, io.LimitReader(stream, 256*1024))
				} else {
					_, err = stdcopy.StdCopy(buf, buf, io.LimitReader(stream, 256*1024))
				}
				stream.Close()
				output.WriteString("\n" + c.Labels["com.docker.compose.service"] + ":\n" + buf.String())
				if err != nil {
					output.WriteString("\n[log stream truncated]")
				}
			}
			return assistant.Redact(output.String(), assistantSecrets(app)...), nil
		case "check_app":
			return assistantProbe(ctx, items, a.Port, assistantSecrets(app))
		case "restart_app":
			unlock, err := LockAppOperation(a.App)
			if err != nil {
				return "", err
			}
			defer unlock()
			if len(items) == 0 {
				return "", errors.New("no containers found")
			}
			timeout := 20
			for _, c := range items {
				if err := cli.ContainerRestart(ctx, c.ID, container.StopOptions{Timeout: &timeout}); err != nil {
					return "", err
				}
			}
			return "Restart completed. Inspect logs and check the web interfaces to verify readiness.", nil
		}
		return "", errors.New("unknown tool")
	}}, nil
}

type assistantLogWriter struct {
	bytes.Buffer
	limit int
}

func (w *assistantLogWriter) Write(p []byte) (int, error) {
	n := len(p)
	remaining := w.limit - w.Len()
	if remaining > len(p) {
		remaining = len(p)
	}
	if remaining > 0 {
		_, _ = w.Buffer.Write(p[:remaining])
	}
	return n, nil
}

func assistantProbe(ctx context.Context, items []dockerTypes.Container, port int, secrets []string) (string, error) {
	if port < 1 || port > 65535 {
		return "", errors.New("invalid published port")
	}
	found := false
	host := "127.0.0.1"
	for _, c := range items {
		for _, p := range c.Ports {
			if p.Type == "tcp" && int(p.PublicPort) == port {
				found = true
				if p.IP != "" && p.IP != "0.0.0.0" && p.IP != "::" {
					host = p.IP
				}
			}
		}
	}
	if !found {
		return "", errors.New("port is not published by this app")
	}
	u := "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	// No proxy, redirects, arbitrary URLs, or scripts: traffic stays on the observed app endpoint.
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 65536))
	if err != nil {
		return "", err
	}
	text := assistantPageText(body)
	return assistant.Redact(assistantJSON(map[string]interface{}{"url": u, "status": res.StatusCode, "content_type": res.Header.Get("Content-Type"), "page_text": text, "note": "HTTP observation only; JavaScript and login flows were not executed."}), secrets...), nil
}
func assistantPageText(body []byte) string {
	z := html.NewTokenizer(bytes.NewReader(body))
	var out strings.Builder
	skip := false
	for {
		t := z.Next()
		if t == html.ErrorToken {
			break
		}
		tok := z.Token()
		if t == html.StartTagToken && (tok.Data == "script" || tok.Data == "style") {
			skip = true
		}
		if t == html.EndTagToken && (tok.Data == "script" || tok.Data == "style") {
			skip = false
		}
		if t == html.TextToken && !skip {
			out.WriteString(strings.Join(strings.Fields(tok.Data), " ") + " ")
		}
		if out.Len() > 6000 {
			break
		}
	}
	return strings.TrimSpace(out.String())
}

func assistantPorts(ports []assistantPort) ([]composeTypes.ServicePortConfig, error) {
	out := []composeTypes.ServicePortConfig{}
	seen := map[int]bool{}
	for _, p := range ports {
		if p.Host < 1 || p.Host > 65535 || p.Container < 1 || p.Container > 65535 || seen[p.Host] {
			return nil, errors.New("ports must be unique and in 1–65535")
		}
		seen[p.Host] = true
		out = append(out, composeTypes.ServicePortConfig{Target: uint32(p.Container), Published: strconv.Itoa(p.Host), Protocol: "tcp", Mode: "ingress"})
	}
	return out, nil
}
func assistantEnvironment(env map[string]string) (composeTypes.MappingWithEquals, error) {
	out := composeTypes.MappingWithEquals{}
	for k, v := range env {
		if !assistantEnv.MatchString(k) || len(v) > 8192 || strings.ContainsRune(v, 0) {
			return nil, errors.New("invalid environment setting")
		}
		value := v
		out[k] = &value
	}
	return out, nil
}
func assistantInstallPlan(a assistantArgs) (assistant.Prepared, error) {
	if len(a.Services) < 1 || len(a.Services) > 12 {
		return assistant.Prepared{}, errors.New("a stack needs 1–12 services")
	}
	path := filepath.Join(config.AppInfo.AppsPath, a.App, common.ComposeYAMLFileName)
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		return assistant.Prepared{}, errors.New("app directory already exists; inspect the existing app")
	}
	project := &ComposeApp{Name: a.App, Services: composeTypes.Services{}}
	names := map[string]bool{}
	hostPorts := map[int]bool{}
	for _, s := range a.Services {
		if !assistantName.MatchString(s.Name) || names[s.Name] {
			return assistant.Prepared{}, errors.New("invalid or duplicate service name")
		}
		names[s.Name] = true
		if !assistantValidImage(s.Image) {
			return assistant.Prepared{}, errors.New("images need an explicit tag or digest")
		}
		env, err := assistantEnvironment(s.Environment)
		if err != nil {
			return assistant.Prepared{}, err
		}
		ports, err := assistantPorts(s.Ports)
		if err != nil {
			return assistant.Prepared{}, err
		}
		for _, p := range s.Ports {
			if hostPorts[p.Host] {
				return assistant.Prepared{}, errors.New("stack has conflicting published ports")
			}
			hostPorts[p.Host] = true
		}
		volumes := []composeTypes.ServiceVolumeConfig{}
		for _, v := range s.Volumes {
			source := filepath.Clean(v.Source)
			target := filepath.Clean(v.Target)
			if !strings.HasPrefix(source, "/DATA/") || !filepath.IsAbs(target) || target == "/" || strings.Contains(source, ":") || strings.ContainsAny(source+target, "$\x00") {
				return assistant.Prepared{}, errors.New("host mounts must be below /DATA and have an absolute container target")
			}
			if err := assistantNoSymlink(source); err != nil {
				return assistant.Prepared{}, err
			}
			volumes = append(volumes, composeTypes.ServiceVolumeConfig{Type: "bind", Source: source, Target: target, ReadOnly: v.ReadOnly})
		}
		project.Services = append(project.Services, composeTypes.ServiceConfig{Name: s.Name, Image: s.Image, Environment: env, Ports: ports, Volumes: volumes, Restart: "unless-stopped"})
	}
	project.Extensions = map[string]interface{}{"x-casaos": map[string]interface{}{"main": a.Services[0].Name, "title": map[string]string{"en_us": a.App}}}
	data, err := resolvedComposeYAML(project)
	if err != nil {
		return assistant.Prepared{}, err
	}
	if _, err = NewComposeAppFromYAML(data, false, false); err != nil {
		return assistant.Prepared{}, err
	}
	return assistant.Prepared{Summary: "Install stack " + a.App + ". Images will be downloaded and containers started.\n" + assistant.Redact(string(data), assistantSecrets(project)...), Execute: func(ctx context.Context) (string, error) {
		unlock, err := LockAppOperation(a.App)
		if err != nil {
			return "", err
		}
		defer unlock()
		if err := assistantNoSymlink(filepath.Dir(path)); err != nil {
			return "", err
		}
		for _, s := range project.Services {
			for _, v := range s.Volumes {
				if err := assistantNoSymlink(v.Source); err != nil {
					return "", err
				}
			}
		}
		if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
			return "", err
		}
		// Retain a failed install's Compose definition for diagnosis and recovery; never delete its data.
		if err := atomicPrivateWrite(path, data); err != nil {
			return "", err
		}
		installed, err := LoadComposeAppFromConfigFile(a.App, path)
		if err != nil {
			return "", err
		}
		if err = installed.PullAndInstall(ctx); err != nil {
			return "Compose saved; installation incomplete. Inspect app logs and settings before retrying.", err
		}
		return "Stack installed. App-specific setup is still required; inspect containers, logs and web endpoints now.", nil
	}}, nil
}
func assistantConfigurePlan(app *ComposeApp, a assistantArgs) (assistant.Prepared, error) {
	if len(a.Environment) == 0 && a.Ports == nil && a.Image == "" && a.Volumes == nil && a.SharedNetwork == "" {
		return assistant.Prepared{}, errors.New("provide environment, image, ports, mounts, or a shared network to change")
	}
	if len(app.ComposeFiles) != 1 {
		return assistant.Prepared{}, errors.New("only single-file CasaOS Compose apps can be configured")
	}
	original, err := os.ReadFile(app.ComposeFiles[0])
	if err != nil {
		return assistant.Prepared{}, err
	}
	fingerprint := sha256.Sum256(original)
	target, err := cloneCompose(app)
	if err != nil {
		return assistant.Prepared{}, err
	}
	index := -1
	for i, s := range target.Services {
		if s.Name == a.Service {
			index = i
		}
	}
	if index < 0 {
		return assistant.Prepared{}, errors.New("service not found")
	}
	if a.SharedNetwork != "" {
		if err := assistantSharedNetwork(target, index, a.SharedNetwork); err != nil {
			return assistant.Prepared{}, err
		}
	}
	if a.Image != "" {
		if !assistantValidImage(a.Image) {
			return assistant.Prepared{}, errors.New("image needs a valid explicit tag or digest")
		}
		target.Services[index].Image = a.Image
	}
	if a.Volumes != nil {
		seen := map[string]bool{}
		for _, v := range *a.Volumes {
			source, targetPath := filepath.Clean(v.Source), path.Clean(v.Target)
			if !strings.HasPrefix(source, "/DATA/") || !path.IsAbs(targetPath) || targetPath == "/" || strings.ContainsAny(source+targetPath, "$\x00") || strings.Contains(source, ":") || seen[targetPath] {
				return assistant.Prepared{}, errors.New("mounts need unique absolute targets and host sources below /DATA")
			}
			if err = assistantNoSymlink(source); err != nil {
				return assistant.Prepared{}, err
			}
			seen[targetPath] = true
			mount := composeTypes.ServiceVolumeConfig{Type: "bind", Source: source, Target: targetPath, ReadOnly: v.ReadOnly}
			replaced := false
			for i, old := range target.Services[index].Volumes {
				if old.Target == targetPath {
					target.Services[index].Volumes[i] = mount
					replaced = true
					break
				}
			}
			if !replaced {
				target.Services[index].Volumes = append(target.Services[index].Volumes, mount)
			}
		}
	}
	env, err := assistantEnvironment(a.Environment)
	if err != nil {
		return assistant.Prepared{}, err
	}
	if target.Services[index].Environment == nil {
		target.Services[index].Environment = composeTypes.MappingWithEquals{}
	}
	for k, v := range env {
		target.Services[index].Environment[k] = v
	}
	if a.Ports != nil {
		ports, err := assistantPorts(*a.Ports)
		if err != nil {
			return assistant.Prepared{}, err
		}
		target.Services[index].Ports = ports
	}
	data, err := resolvedComposeYAML(target)
	if err != nil {
		return assistant.Prepared{}, err
	}
	secrets := append(assistantSecrets(app), assistantSecrets(target)...)
	summary := "Configure " + a.App + " / " + a.Service + ". Containers may restart. Existing networks and unspecified mounts are preserved; mount changes do not move or delete existing data. A shared network allows its joined services to communicate on private ports.\n" + assistant.Redact(assistantJSON(a), secrets...)
	return assistant.Prepared{Summary: summary, Execute: func(ctx context.Context) (string, error) {
		unlock, err := LockAppOperation(a.App)
		if err != nil {
			return "", err
		}
		defer unlock()
		if err = assistantNoSymlink(app.ComposeFiles[0]); err != nil {
			return "", err
		}
		current, err := os.ReadFile(app.ComposeFiles[0])
		if err != nil {
			return "", err
		}
		if sha256.Sum256(current) != fingerprint {
			return "", errors.New("app settings changed since the proposal; inspect and propose again")
		}
		if a.Volumes != nil {
			for _, v := range *a.Volumes {
				if err = assistantNoSymlink(filepath.Clean(v.Source)); err != nil {
					return "", err
				}
			}
		}
		backup := app.ComposeFiles[0] + ".assistant.bak"
		if err = atomicPrivateWrite(backup, current); err != nil {
			return "", err
		}
		svc, cli, err := apiService()
		if err != nil {
			return "", err
		}
		defer cli.Close()
		if a.SharedNetwork != "" {
			unlockNetwork, err := LockAppOperation("assistant-network:" + a.SharedNetwork)
			if err != nil {
				return "", err
			}
			defer unlockNetwork()
			if err := assistantEnsureNetwork(ctx, cli, a.SharedNetwork); err != nil {
				return "", err
			}
		}
		if err = atomicPrivateWrite(app.ComposeFiles[0], data); err != nil {
			return "", err
		}
		updated, applyErr := LoadComposeAppFromConfigFile(a.App, app.ComposeFiles[0])
		if applyErr == nil && a.Image != "" {
			applyErr = updated.Pull(ctx)
		}
		if applyErr == nil {
			applyErr = updated.UpWithCheckRequire(ctx, svc)
		}
		if applyErr != nil {
			restoreErr := atomicPrivateWrite(app.ComposeFiles[0], current)
			recoveryCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if restoreErr == nil {
				restoreErr = app.Up(recoveryCtx, svc)
			}
			if restoreErr != nil {
				return "", fmt.Errorf("apply failed: %v; recovery also failed: %v; backup retained at %s", applyErr, restoreErr, backup)
			}
			return "Previous Compose settings restored.", applyErr
		}
		if err = Updates.SettingsChanged(updated); err != nil {
			return "Settings applied, but update metadata refresh failed.", err
		}
		return "Settings applied. Previous Compose saved as .assistant.bak. Inspect logs and check web endpoints to verify.", nil
	}}, nil
}

func assistantValidImage(image string) bool {
	if strings.ContainsAny(image, " \t\r\n$") {
		return false
	}
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return false
	}
	_, tagged := named.(reference.Tagged)
	_, digested := named.(reference.Digested)
	return tagged || digested
}
func assistantRetryPlan(app *ComposeApp) (assistant.Prepared, error) {
	if len(app.ComposeFiles) != 1 {
		return assistant.Prepared{}, errors.New("retry requires a single CasaOS Compose file")
	}
	original, err := os.ReadFile(app.ComposeFiles[0])
	if err != nil {
		return assistant.Prepared{}, err
	}
	fingerprint := sha256.Sum256(original)
	return assistant.Prepared{Summary: "Retry starting " + app.Name + " from its saved Compose settings. Images may be downloaded and containers recreated. Existing volumes and data are preserved.", Execute: func(ctx context.Context) (string, error) {
		unlock, err := LockAppOperation(app.Name)
		if err != nil {
			return "", err
		}
		defer unlock()
		if err = assistantNoSymlink(app.ComposeFiles[0]); err != nil {
			return "", err
		}
		current, err := os.ReadFile(app.ComposeFiles[0])
		if err != nil {
			return "", err
		}
		if sha256.Sum256(current) != fingerprint {
			return "", errors.New("app settings changed since the retry was proposed")
		}
		fresh, err := assistantLoad(app.Name)
		if err != nil {
			return "", err
		}
		svc, cli, err := apiService()
		if err != nil {
			return "", err
		}
		defer cli.Close()
		if err = fresh.Pull(ctx); err != nil {
			return "Saved configuration retained for another attempt.", err
		}
		if err = fresh.UpWithCheckRequire(ctx, svc); err != nil {
			return "Startup is still failing. Inspect app logs and settings.", err
		}
		return "App started from its saved settings. Inspect logs and check web endpoints to verify readiness.", nil
	}}, nil
}
