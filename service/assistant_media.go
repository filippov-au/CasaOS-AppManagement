package service

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	stdjson "encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/IceWhaleTech/CasaOS-AppManagement/internal/assistant"
	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

func assistantMediaTools() []assistant.Tool {
	target := map[string]interface{}{"app": str(), "service": str(), "resource": map[string]interface{}{"type": "string", "enum": []string{"status", "setup", "settings", "download_clients", "indexers", "root_folders", "libraries"}}}
	return []assistant.Tool{
		tool("media_read", "Read Sonarr, NZBGet or Plex status/configuration through the app's native API. Authentication is read privately from that service's /config; API credentials are never returned. Use resources supported by the app: Sonarr status/settings/download_clients/indexers/root_folders, NZBGet status/settings, Plex status/setup/libraries. Plex setup reports claim state and the user steps for account sign-in.", schema(target, "app", "service", "resource")),
		tool("connect_sonarr_nzbget", "Test and connect Sonarr to NZBGet using their existing credentials without exposing them. Requires both services on a shared Docker network and identical shared /data mounts. Creates or updates a download client named CasaOS NZBGet. The category must already exist in NZBGet. Checks the connection before saving. News servers, indexers and media root folders must be configured separately.", schema(map[string]interface{}{"sonarr_app": str(), "sonarr_service": str(), "nzbget_app": str(), "nzbget_service": str(), "category": str()}, "sonarr_app", "sonarr_service", "nzbget_app", "nzbget_service", "category")),
	}
}

type assistantMediaTarget struct {
	app        *ComposeApp
	container  dockerTypes.Container
	kind       string
	base       string
	username   string
	password   string
	token      string
	configHash [32]byte
}

func (m *assistantMediaTarget) secrets() []string { return []string{m.password, m.token} }
func assistantReadContainerFile(ctx context.Context, cli client.APIClient, id, path string) ([]byte, error) {
	stream, _, err := cli.CopyFromContainer(ctx, id, path)
	if err != nil {
		return nil, errors.New("app configuration is not ready or is not in the supported /config location")
	}
	defer stream.Close()
	reader := tar.NewReader(io.LimitReader(stream, 512*1024))
	header, err := reader.Next()
	if err != nil {
		return nil, err
	}
	if header.Typeflag != tar.TypeReg || header.Size > 256*1024 {
		return nil, errors.New("app configuration is not a bounded regular file")
	}
	return io.ReadAll(reader)
}
func assistantMediaResolve(ctx context.Context, appID, service string) (*assistantMediaTarget, error) {
	app, err := assistantLoad(appID)
	if err != nil {
		return nil, err
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	containers, err := assistantContainers(ctx, cli, appID)
	if err != nil {
		return nil, err
	}
	var selected *dockerTypes.Container
	for i, c := range containers {
		if c.Labels["com.docker.compose.service"] == service {
			if selected != nil {
				return nil, errors.New("select an unscaled media service")
			}
			selected = &containers[i]
		}
	}
	if selected == nil {
		return nil, errors.New("media service container not found")
	}
	m := &assistantMediaTarget{app: app, container: *selected}
	image := strings.ToLower(selected.Image)
	path := ""
	port := 0
	urlBase := ""
	switch {
	case strings.Contains(image, "/sonarr:") || strings.Contains(image, "/sonarr@"):
		m.kind = "sonarr"
		path = "/config/config.xml"
		port = 8989
	case strings.Contains(image, "/nzbget:") || strings.Contains(image, "/nzbget@"):
		m.kind = "nzbget"
		path = "/config/nzbget.conf"
		port = 6789
	case strings.Contains(image, "/plex:") || strings.Contains(image, "/plex@"):
		m.kind = "plex"
		path = "/config/Library/Application Support/Plex Media Server/Preferences.xml"
		port = 32400
	default:
		return nil, errors.New("supported native API adapters are Sonarr, NZBGet and Plex with /config storage")
	}
	data, err := assistantReadContainerFile(ctx, cli, selected.ID, path)
	if err != nil {
		return nil, err
	}
	m.configHash = sha256.Sum256(data)
	switch m.kind {
	case "sonarr":
		var cfg struct {
			APIKey  string `xml:"ApiKey"`
			Port    int    `xml:"Port"`
			URLBase string `xml:"UrlBase"`
		}
		if err = xml.Unmarshal(data, &cfg); err != nil {
			return nil, errors.New("invalid Sonarr configuration")
		}
		m.token = cfg.APIKey
		if cfg.Port > 0 {
			port = cfg.Port
		}
		urlBase = cfg.URLBase
		if m.token == "" {
			return nil, errors.New("Sonarr API key is not ready")
		}
	case "nzbget":
		cfg := assistantNZBConfig(data)
		m.username = cfg["ControlUsername"]
		m.password = cfg["ControlPassword"]
		if p, e := strconv.Atoi(cfg["ControlPort"]); e == nil && p > 0 {
			port = p
		}
	case "plex":
		var cfg struct {
			Token string `xml:"PlexOnlineToken,attr"`
		}
		if err = xml.Unmarshal(data, &cfg); err != nil {
			return nil, errors.New("invalid Plex preferences")
		}
		m.token = cfg.Token
	}
	if strings.ContainsAny(urlBase, "?#\\") || strings.HasPrefix(urlBase, "//") {
		return nil, errors.New("unsupported app URL base")
	}
	host := "127.0.0.1"
	published := 0
	for _, p := range selected.Ports {
		if p.Type == "tcp" && int(p.PrivatePort) == port && p.PublicPort > 0 {
			published = int(p.PublicPort)
			if p.IP != "" && p.IP != "0.0.0.0" && p.IP != "::" {
				host = p.IP
			}
			break
		}
	}
	if published == 0 {
		return nil, errors.New("publish the media service's HTTP port before using its API")
	}
	m.base = "http://" + net.JoinHostPort(host, strconv.Itoa(published)) + strings.TrimRight(urlBase, "/")
	return m, nil
}
func assistantNZBConfig(data []byte) map[string]string {
	cfg := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if pair := strings.SplitN(line, "=", 2); len(pair) == 2 {
			cfg[strings.TrimSpace(pair[0])] = strings.TrimSpace(pair[1])
		}
	}
	return cfg
}
func (m *assistantMediaTarget) request(ctx context.Context, method, path string, body interface{}) ([]byte, error) {
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = stdjson.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, m.base+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	switch m.kind {
	case "sonarr":
		req.Header.Set("X-Api-Key", m.token)
	case "nzbget":
		req.SetBasicAuth(m.username, m.password)
	case "plex":
		req.Header.Set("X-Plex-Token", m.token)
	}
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	res, err := client.Do(req)
	if err != nil {
		return nil, errors.New("media API could not be reached")
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 256*1024+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 256*1024 {
		return nil, errors.New("media API response exceeded limit")
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, &assistantMediaHTTPError{Kind: m.kind, Status: res.StatusCode, Message: assistantMediaRedact(data, m.secrets())}
	}
	return data, nil
}
func assistantMediaRedact(data []byte, secrets []string) string {
	var v interface{}
	if stdjson.Unmarshal(data, &v) == nil {
		var visit func(interface{})
		visit = func(v interface{}) {
			switch x := v.(type) {
			case map[string]interface{}:
				// Arr field arrays and NZBGet configuration use name/value records.
				for _, key := range []string{"name", "Name"} {
					if name, ok := x[key].(string); ok && secretKey.MatchString(name) {
						for _, val := range []string{"value", "Value"} {
							if _, ok := x[val]; ok {
								x[val] = "[redacted]"
							}
						}
					}
				}
				for k, value := range x {
					if secretKey.MatchString(k) {
						x[k] = "[redacted]"
					} else {
						visit(value)
					}
				}
			case []interface{}:
				for _, value := range x {
					visit(value)
				}
			}
		}
		visit(v)
		data, _ = stdjson.MarshalIndent(v, "", "  ")
	}
	return assistant.Redact(string(data), secrets...)
}
func assistantMediaReadPlan(ctx context.Context, raw stdjson.RawMessage) (assistant.Prepared, error) {
	var a struct {
		App      string `json:"app"`
		Service  string `json:"service"`
		Resource string `json:"resource"`
	}
	if err := assistantDecode(raw, &a); err != nil {
		return assistant.Prepared{}, err
	}
	return assistant.Prepared{Summary: "Read " + a.Resource + " from " + a.App + " / " + a.Service, Execute: func(ctx context.Context) (string, error) {
		m, err := assistantMediaResolve(ctx, a.App, a.Service)
		if err != nil {
			return "", err
		}
		var path string
		var body interface{}
		method := http.MethodGet
		switch m.kind {
		case "sonarr":
			path = map[string]string{"status": "/api/v3/system/status", "settings": "/api/v3/config/mediamanagement", "download_clients": "/api/v3/downloadclient", "indexers": "/api/v3/indexer", "root_folders": "/api/v3/rootfolder"}[a.Resource]
		case "nzbget":
			if a.Resource == "status" || a.Resource == "settings" {
				rpc := "status"
				if a.Resource == "settings" {
					rpc = "config"
				}
				method = http.MethodPost
				path = "/jsonrpc"
				body = map[string]interface{}{"method": rpc, "params": []interface{}{}, "id": 1}
			}
		case "plex":
			if a.Resource == "setup" {
				return assistantPlexSetup(ctx, m)
			}
			path = map[string]string{"status": "/identity", "libraries": "/library/sections"}[a.Resource]
		}
		if path == "" {
			return "", errors.New("resource is not supported by this app")
		}
		data, err := m.request(ctx, method, path, body)
		if err != nil {
			return "", err
		}
		return assistantMediaRedact(data, m.secrets()), nil
	}}, nil
}

type assistantConnectArgs struct {
	SonarrApp     string `json:"sonarr_app"`
	SonarrService string `json:"sonarr_service"`
	NZBGetApp     string `json:"nzbget_app"`
	NZBGetService string `json:"nzbget_service"`
	Category      string `json:"category"`
}

func assistantConnectPlan(ctx context.Context, raw stdjson.RawMessage) (assistant.Prepared, error) {
	var a assistantConnectArgs
	if err := assistantDecode(raw, &a); err != nil {
		return assistant.Prepared{}, err
	}
	if !assistantName.MatchString(a.Category) {
		return assistant.Prepared{}, errors.New("choose a valid NZBGet category name")
	}
	sonarr, err := assistantMediaResolve(ctx, a.SonarrApp, a.SonarrService)
	if err != nil {
		return assistant.Prepared{}, err
	}
	nzbget, err := assistantMediaResolve(ctx, a.NZBGetApp, a.NZBGetService)
	if err != nil {
		return assistant.Prepared{}, err
	}
	if sonarr.kind != "sonarr" || nzbget.kind != "nzbget" {
		return assistant.Prepared{}, errors.New("service types do not match")
	}
	shared := false
	for network := range sonarr.container.NetworkSettings.Networks {
		if _, ok := nzbget.container.NetworkSettings.Networks[network]; ok {
			shared = true
		}
	}
	if !shared {
		return assistant.Prepared{}, errors.New("Sonarr and NZBGet need a shared Docker network")
	}
	volumeSource := func(m *assistantMediaTarget, service string) string {
		for _, s := range m.app.Services {
			if s.Name == service {
				for _, v := range s.Volumes {
					if v.Target == "/data" && v.Type == "bind" && !v.ReadOnly {
						return v.Source
					}
				}
			}
		}
		return ""
	}
	source := volumeSource(sonarr, a.SonarrService)
	if source == "" || source != volumeSource(nzbget, a.NZBGetService) {
		return assistant.Prepared{}, errors.New("configure the same writable /data bind mount for Sonarr and NZBGet first")
	}
	// Load existing state now, so approval is for a concrete, immutable request.
	data, err := sonarr.request(ctx, http.MethodGet, "/api/v3/downloadclient", nil)
	if err != nil {
		return assistant.Prepared{}, err
	}
	oldHash := sha256.Sum256(data)
	var clients []map[string]interface{}
	if err = stdjson.Unmarshal(data, &clients); err != nil {
		return assistant.Prepared{}, err
	}
	var payload map[string]interface{}
	for _, c := range clients {
		if c["name"] == "CasaOS NZBGet" {
			payload = c
			break
		}
	}
	if payload == nil {
		data, err = sonarr.request(ctx, http.MethodGet, "/api/v3/downloadclient/schema", nil)
		if err != nil {
			return assistant.Prepared{}, err
		}
		var schemas []map[string]interface{}
		if err = stdjson.Unmarshal(data, &schemas); err != nil {
			return assistant.Prepared{}, err
		}
		for _, s := range schemas {
			if s["implementation"] == "Nzbget" {
				payload = s
				break
			}
		}
	}
	if payload == nil {
		return assistant.Prepared{}, errors.New("Sonarr did not provide the NZBGet client schema")
	}
	// Ensure category exists, avoiding Sonarr imports into an unexpected download folder.
	data, err = nzbget.request(ctx, http.MethodPost, "/jsonrpc", map[string]interface{}{"method": "config", "params": []interface{}{}, "id": 1})
	if err != nil {
		return assistant.Prepared{}, err
	}
	var cfg struct {
		Result []struct {
			Name  string
			Value string
		} `json:"result"`
	}
	if err = stdjson.Unmarshal(data, &cfg); err != nil {
		return assistant.Prepared{}, err
	}
	categoryExists := false
	controlPort := 6789
	for _, entry := range cfg.Result {
		if strings.HasPrefix(entry.Name, "Category") && strings.HasSuffix(entry.Name, ".Name") && entry.Value == a.Category {
			categoryExists = true
		}
		if entry.Name == "ControlPort" {
			controlPort, _ = strconv.Atoi(entry.Value)
		}
	}
	if !categoryExists {
		return assistant.Prepared{}, errors.New("create category " + a.Category + " in NZBGet settings before linking")
	}
	if controlPort < 1 || controlPort > 65535 {
		return assistant.Prepared{}, errors.New("invalid NZBGet control port")
	}
	host := a.NZBGetService
	// A stable Docker DNS name survives container recreation; never save a transient IP.
	if len(nzbget.container.Names) > 0 {
		host = strings.TrimPrefix(nzbget.container.Names[0], "/")
	}

	payload["name"] = "CasaOS NZBGet"
	payload["enable"] = true
	fields, ok := payload["fields"].([]interface{})
	if !ok {
		return assistant.Prepared{}, errors.New("invalid Sonarr download client schema")
	}
	values := map[string]interface{}{"host": host, "port": controlPort, "useSsl": false, "username": nzbget.username, "password": nzbget.password, "tvCategory": a.Category}
	for _, entry := range fields {
		field, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := field["name"].(string)
		if value, ok := values[name]; ok {
			field["value"] = value
		}
	}
	return assistant.Prepared{Summary: "Connect " + a.SonarrApp + " / " + a.SonarrService + " to " + a.NZBGetApp + " / " + a.NZBGetService + ".\nDownload client: CasaOS NZBGet\nCategory: " + a.Category + "\nShared data: " + source + " → /data\nExisting credentials are used privately. Test connectivity before saving.", Execute: func(ctx context.Context) (string, error) {
		unlock, err := LockAppOperation(a.SonarrApp)
		if err != nil {
			return "", err
		}
		defer unlock()
		freshSonarr, err := assistantMediaResolve(ctx, a.SonarrApp, a.SonarrService)
		if err != nil {
			return "", err
		}
		freshNZB, err := assistantMediaResolve(ctx, a.NZBGetApp, a.NZBGetService)
		if err != nil {
			return "", err
		}
		if freshSonarr.container.ID != sonarr.container.ID || freshNZB.container.ID != nzbget.container.ID || freshSonarr.configHash != sonarr.configHash || freshNZB.configHash != nzbget.configHash {
			return "", errors.New("media services changed; inspect and propose the connection again")
		}
		current, err := sonarr.request(ctx, http.MethodGet, "/api/v3/downloadclient", nil)
		if err != nil {
			return "", err
		}
		if sha256.Sum256(current) != oldHash {
			return "", errors.New("Sonarr download clients changed since the proposal")
		}
		if _, err = sonarr.request(ctx, http.MethodPost, "/api/v3/downloadclient/test", payload); err != nil {
			return "", err
		}
		path := "/api/v3/downloadclient"
		method := http.MethodPost
		if id, ok := payload["id"].(float64); ok && id > 0 {
			path += "/" + strconv.Itoa(int(id))
			method = http.MethodPut
		}
		data, err := sonarr.request(ctx, method, path, payload)
		if err != nil {
			return "", err
		}
		return "Sonarr tested the NZBGet connection and saved the download client. Verify indexers, news server credentials, media root folders and Plex libraries separately.\n" + assistantMediaRedact(data, append(sonarr.secrets(), nzbget.secrets()...)), nil
	}}, nil
}

type assistantMediaHTTPError struct {
	Kind    string
	Status  int
	Message string
}

func (e *assistantMediaHTTPError) Error() string {
	return fmt.Sprintf("%s API HTTP %d: %s", e.Kind, e.Status, e.Message)
}
