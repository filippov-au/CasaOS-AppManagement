package service

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/IceWhaleTech/CasaOS-AppManagement/internal/assistant"
	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

func assistantSetupTools() []assistant.Tool {
	return []assistant.Tool{
		tool("create_app_directory", "Create a missing directory within a service's existing writable /data, /downloads, or /media bind mount, using the service PUID/PGID. Existing directories and permissions are preserved. Use for download categories and media roots before configuring apps.", schema(map[string]interface{}{"app": str(), "service": str(), "path": str()}, "app", "service", "path")),
		tool("configure_nzbget", "Merge download paths, categories and news server settings into NZBGet's complete configuration. Allowed options: MainDir/DestDir/InterDir/NzbDir; CategoryN.Name/DestDir/Unpack; ServerN.Active/Name/Host/Port/Username/Password/Encryption/Connections/Retention/Level/Group/Optional. Back up settings, test changed active news servers, save, reload and verify. Credential values may be $secret:name references supplied privately by the user. Keeps unrelated settings and credentials.", schema(map[string]interface{}{"app": str(), "service": str(), "options": map[string]interface{}{"type": "object", "additionalProperties": str()}}, "app", "service", "options")),
		tool("add_sonarr_root_folder", "Add an existing writable media directory to Sonarr. Reuses an existing matching root folder, verifies the saved resource, and never deletes folders or media. Create the directory first if needed.", schema(map[string]interface{}{"app": str(), "service": str(), "path": str()}, "app", "service", "path")),
		tool("configure_sonarr_indexer", "Create or update a Newznab indexer by name using Sonarr's schema. Tests the indexer's API before saving. Provide the user's URL and API key ($secret:name for private values); existing unrelated indexers are preserved.", schema(map[string]interface{}{"app": str(), "service": str(), "name": str(), "url": str(), "api_key": str()}, "app", "service", "name", "url", "api_key")),
		tool("add_plex_library", "Add a TV or movie library for an existing media directory. Reuses an existing library with the same name and path. Authenticates with the server's existing Plex token; Plex account sign-in/claiming may be required first. Verifies the new library through the server API.", schema(map[string]interface{}{"app": str(), "service": str(), "name": str(), "path": str(), "type": map[string]interface{}{"type": "string", "enum": []string{"show", "movie"}}}, "app", "service", "name", "path", "type")),
	}
}

type assistantPathArgs struct {
	App     string `json:"app"`
	Service string `json:"service"`
	Path    string `json:"path"`
}

// Map a container media path back to a concrete bind mount before any filesystem
// action. Excludes config, system paths, parent traversals and symlink escapes.
func assistantMediaPath(app *ComposeApp, service, target string, writable bool) (string, error) {
	if target != path.Clean(target) || !path.IsAbs(target) || strings.ContainsAny(target, "\x00\\$") {
		return "", errors.New("use a clean absolute media path")
	}
	media := false
	for _, root := range []string{"/data", "/downloads", "/media"} {
		if target == root || strings.HasPrefix(target, root+"/") {
			media = true
		}
	}
	if !media {
		return "", errors.New("media paths must be within /data, /downloads, or /media")
	}
	best := ""
	source := ""
	for _, s := range app.Services {
		if s.Name != service {
			continue
		}
		for _, v := range s.Volumes {
			if v.Type != "bind" || (writable && v.ReadOnly) {
				continue
			}
			if target == v.Target || strings.HasPrefix(target, strings.TrimRight(v.Target, "/")+"/") {
				if len(v.Target) > len(best) {
					best = v.Target
					source = v.Source
				}
			}
		}
	}
	if best == "" || source == "" || !filepath.IsAbs(source) {
		return "", errors.New("path is not inside a supported persistent bind mount")
	}
	for _, root := range []string{"/", "/etc", "/proc", "/sys", "/dev", "/run", "/var/run", "/root"} {
		if source == root || (root != "/" && strings.HasPrefix(source, root+"/")) {
			return "", errors.New("system directories cannot be used for assistant media operations")
		}
	}
	host := filepath.Join(source, strings.TrimPrefix(strings.TrimPrefix(target, best), "/"))
	if err := assistantNoSymlink(host); err != nil {
		return "", err
	}
	return host, nil
}
func assistantDirectoryPlan(ctx context.Context, raw stdjson.RawMessage) (assistant.Prepared, error) {
	var a assistantPathArgs
	if err := assistantDecode(raw, &a); err != nil {
		return assistant.Prepared{}, err
	}
	app, err := assistantLoad(a.App)
	if err != nil {
		return assistant.Prepared{}, err
	}
	host, err := assistantMediaPath(app, a.Service, a.Path, true)
	if err != nil {
		return assistant.Prepared{}, err
	}
	return assistant.Prepared{Summary: "Create media directory " + a.Path + " for " + a.App + " / " + a.Service + "\nHost location: " + host + "\nExisting directories and data are preserved.", Execute: func(ctx context.Context) (string, error) {
		unlock, err := LockAppOperation(a.App)
		if err != nil {
			return "", err
		}
		defer unlock()
		fresh, err := assistantLoad(a.App)
		if err != nil {
			return "", err
		}
		now, err := assistantMediaPath(fresh, a.Service, a.Path, true)
		if err != nil {
			return "", err
		}
		if now != host {
			return "", errors.New("mounts changed since the proposal")
		}
		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return "", err
		}
		defer cli.Close()
		items, err := assistantContainers(ctx, cli, a.App)
		if err != nil {
			return "", err
		}
		id := ""
		for _, c := range items {
			if c.Labels["com.docker.compose.service"] == a.Service {
				if id != "" {
					return "", errors.New("select an unscaled service")
				}
				id = c.ID
			}
		}
		if id == "" {
			return "", errors.New("start the app before creating its directory")
		}
		uid, gid := 1000, 1000
		for _, s := range fresh.Services {
			if s.Name == a.Service {
				for k, v := range s.Environment {
					if v == nil {
						continue
					}
					n, e := strconv.Atoi(*v)
					if e == nil && n >= 0 {
						if k == "PUID" {
							uid = n
						}
						if k == "PGID" {
							gid = n
						}
					}
				}
			}
		}
		missing := []string{}
		current := a.Path
		for {
			stat, e := cli.ContainerStatPath(ctx, id, current)
			if e == nil {
				if !stat.Mode.IsDir() {
					return "", errors.New("path contains a non-directory")
				}
				break
			}
			if !client.IsErrNotFound(e) {
				return "", e
			}
			if _, e = assistantMediaPath(fresh, a.Service, current, true); e != nil {
				return "", e
			}
			missing = append(missing, current)
			current = path.Dir(current)
		}
		for i := len(missing) - 1; i >= 0; i-- {
			if _, err = assistantMediaPath(fresh, a.Service, missing[i], true); err != nil {
				return "", err
			}
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			if err = tw.WriteHeader(&tar.Header{Name: path.Base(missing[i]) + "/", Typeflag: tar.TypeDir, Mode: 0775, Uid: uid, Gid: gid}); err != nil {
				return "", err
			}
			if err = tw.Close(); err != nil {
				return "", err
			}
			if err = cli.CopyToContainer(ctx, id, path.Dir(missing[i]), &buf, dockerTypes.CopyToContainerOptions{CopyUIDGID: true}); err != nil {
				return "", err
			}
		}
		return "Media directory is available: " + a.Path, nil
	}}, nil
}

func (m *assistantMediaTarget) rpc(ctx context.Context, method string, params []interface{}, out interface{}) error {
	data, err := m.request(ctx, http.MethodPost, "/jsonrpc", map[string]interface{}{"method": method, "params": params, "id": 1})
	if err != nil {
		return err
	}
	var response struct {
		Result stdjson.RawMessage `json:"result"`
		Error  stdjson.RawMessage `json:"error"`
	}
	if err = stdjson.Unmarshal(data, &response); err != nil {
		return errors.New("invalid NZBGet RPC response")
	}
	if len(response.Error) > 0 && string(response.Error) != "null" {
		return fmt.Errorf("NZBGet rejected %s: %s", method, assistantMediaRedact(response.Error, m.secrets()))
	}
	if len(response.Result) == 0 {
		return errors.New("NZBGet returned no result")
	}
	return stdjson.Unmarshal(response.Result, out)
}

type assistantNZBOption struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

var assistantNZBOptionName = regexp.MustCompile(`^(MainDir|DestDir|InterDir|NzbDir|Category[1-9][0-9]*\.(Name|DestDir|Unpack)|Server[1-9][0-9]*\.(Active|Name|Host|Port|Username|Password|Encryption|Connections|Retention|Level|Group|Optional))$`)

func assistantNZBMerge(current []assistantNZBOption, changes map[string]string) ([]assistantNZBOption, error) {
	if len(changes) == 0 || len(changes) > 100 {
		return nil, errors.New("provide 1–100 NZBGet options")
	}
	for k, v := range changes {
		if !assistantNZBOptionName.MatchString(k) || len(v) > 8192 || strings.ContainsAny(v, "\r\n\x00") {
			return nil, errors.New("invalid or unsupported NZBGet option: " + k)
		}
	}
	result := append([]assistantNZBOption{}, current...)
	keys := map[string]bool{}
	for i, opt := range result {
		keys[opt.Name] = true
		if value, ok := changes[opt.Name]; ok {
			result[i].Value = value
		}
	}
	names := []string{}
	for name := range changes {
		if !keys[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		result = append(result, assistantNZBOption{Name: name, Value: changes[name]})
	}
	return result, nil
}
func assistantNativeBackup(app *ComposeApp, service, kind string, data []byte) (string, error) {
	if !assistantName.MatchString(service) || len(app.ComposeFiles) != 1 {
		return "", errors.New("cannot locate native settings backup directory")
	}
	dir := filepath.Join(filepath.Dir(app.ComposeFiles[0]), ".assistant-backups")
	file := filepath.Join(dir, service+"-"+kind+"-"+assistant.ID()+".json")
	if err := assistantNoSymlink(file); err != nil {
		return "", err
	}
	return file, atomicPrivateWrite(file, data)
}
func assistantSameMedia(ctx context.Context, old *assistantMediaTarget, service string) (*assistantMediaTarget, error) {
	fresh, err := assistantMediaResolve(ctx, old.app.Name, service)
	if err != nil {
		return nil, err
	}
	if fresh.kind != old.kind || fresh.container.ID != old.container.ID || fresh.configHash != old.configHash || fresh.base != old.base {
		return nil, errors.New("app changed since the proposal; inspect and propose again")
	}
	return fresh, nil
}
func assistantNZBPlan(ctx context.Context, raw stdjson.RawMessage) (assistant.Prepared, error) {
	var a struct {
		App     string            `json:"app"`
		Service string            `json:"service"`
		Options map[string]string `json:"options"`
	}
	if err := assistantDecode(raw, &a); err != nil {
		return assistant.Prepared{}, err
	}
	// Validate before any credential or network access.
	if _, err := assistantNZBMerge(nil, a.Options); err != nil {
		return assistant.Prepared{}, err
	}
	m, err := assistantMediaResolve(ctx, a.App, a.Service)
	if err != nil {
		return assistant.Prepared{}, err
	}
	if m.kind != "nzbget" {
		return assistant.Prepared{}, errors.New("select an NZBGet service")
	}
	var current []assistantNZBOption
	if err = m.rpc(ctx, "loadconfig", []interface{}{}, &current); err != nil {
		return assistant.Prepared{}, err
	}
	next, err := assistantNZBMerge(current, a.Options)
	if err != nil {
		return assistant.Prepared{}, err
	}
	verifyOptions := map[string]string{}
	for key, value := range a.Options {
		verifyOptions[key] = value
	}
	if err = assistantNZBPinQueue(ctx, m, current, next, a.Options, verifyOptions); err != nil {
		return assistant.Prepared{}, err
	}
	values := map[string]string{}
	secrets := m.secrets()
	for _, entry := range next {
		values[entry.Name] = entry.Value
		if secretKey.MatchString(entry.Name) {
			secrets = append(secrets, entry.Value)
		}
	}
	for k, v := range a.Options {
		if k == "MainDir" || k == "DestDir" || k == "InterDir" || k == "NzbDir" || strings.HasSuffix(k, ".DestDir") {
			if v == "" && strings.HasPrefix(k, "Category") {
				continue
			}
			resolved := strings.ReplaceAll(v, "${MainDir}", values["MainDir"])
			if _, err = assistantMediaPath(m.app, a.Service, resolved, true); err != nil {
				return assistant.Prepared{}, fmt.Errorf("%s: %w", k, err)
			}
		}
	}
	if err = assistantNZBDirectoryChange(ctx, m, current, a.Options); err != nil {
		return assistant.Prepared{}, err
	}
	oldData, _ := stdjson.Marshal(current)
	fingerprint := sha256.Sum256(oldData)
	nextData, _ := stdjson.Marshal(next)
	unchanged := sha256.Sum256(nextData) == fingerprint
	return assistant.Prepared{Summary: "Configure NZBGet " + a.App + " / " + a.Service + ". Test changed active news servers, save a private backup, reload and verify. The existing queue/history path is preserved when MainDir changes.\n" + assistantMediaRedact([]byte(assistantJSON(a.Options)), secrets), Execute: func(ctx context.Context) (string, error) {
		unlock, err := LockAppOperation(a.App)
		if err != nil {
			return "", err
		}
		defer unlock()
		if _, err = assistantSameMedia(ctx, m, a.Service); err != nil {
			return "", err
		}
		var fresh []assistantNZBOption
		if err = m.rpc(ctx, "loadconfig", []interface{}{}, &fresh); err != nil {
			return "", err
		}
		b, _ := stdjson.Marshal(fresh)
		if sha256.Sum256(b) != fingerprint {
			return "", errors.New("NZBGet settings changed since the proposal")
		}
		if unchanged {
			if err = assistantNZBVerify(ctx, m, current, verifyOptions); err != nil {
				return "", err
			}
			return "NZBGet already has the requested settings; active configuration verified without reloading.", nil
		}
		// A changed base directory must not silently relocate queue/history files.
		// Downloads in progress may still refer to their old intermediate paths.
		if err = assistantNZBDirectoryChange(ctx, m, current, a.Options); err != nil {
			return "", err
		}
		changedServers := map[string]bool{}
		for k := range a.Options {
			if strings.HasPrefix(k, "Server") {
				changedServers[strings.SplitN(k, ".", 2)[0]] = true
			}
		}
		for server := range changedServers {
			prefix := server + "."
			if strings.EqualFold(values[prefix+"Active"], "no") {
				continue
			}
			host := values[prefix+"Host"]
			port, e := strconv.Atoi(values[prefix+"Port"])
			if host == "" || e != nil || port < 1 || port > 65535 {
				return "", errors.New("news server needs a host and valid port")
			}
			encrypted := strings.EqualFold(values[prefix+"Encryption"], "yes")
			var result string
			err = m.rpc(ctx, "testserver", []interface{}{host, port, values[prefix+"Username"], values[prefix+"Password"], encrypted, "", 10, 2}, &result)
			if err != nil {
				return "", errors.New(assistant.Redact(err.Error(), secrets...))
			}
			if result != "" {
				return "", fmt.Errorf("news server connection test failed: %s", assistant.Redact(result, secrets...))
			}
		}
		backup, err := assistantNativeBackup(m.app, a.Service, "nzbget", oldData)
		if err != nil {
			return "", err
		}
		applyErr := assistantNZBSaveReload(ctx, m, next)
		if applyErr == nil {
			applyErr = assistantNZBVerify(ctx, m, next, verifyOptions)
		}
		if applyErr != nil {
			recovery, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			restoreErr := assistantNZBSaveReload(recovery, m, current)
			if restoreErr == nil {
				originalOptions := map[string]string{}
				for _, old := range current {
					if _, changed := verifyOptions[old.Name]; changed {
						originalOptions[old.Name] = old.Value
					}
				}
				restoreErr = assistantNZBVerify(recovery, m, current, originalOptions)
			}
			if restoreErr != nil {
				return "", fmt.Errorf("NZBGet apply failed; recovery also failed. Backup: %s. Inspect status and logs before retrying", backup)
			}
			return "Previous NZBGet settings restored. Backup: " + backup, errors.New(assistant.Redact(applyErr.Error(), secrets...))
		}
		return "NZBGet settings saved, reloaded and verified. Backup: " + backup + ". Inspect logs and check the download client connection next.", nil
	}}, nil
}
func assistantNZBSaveReload(ctx context.Context, m *assistantMediaTarget, settings []assistantNZBOption) error {
	var ok bool
	if err := m.rpc(ctx, "saveconfig", []interface{}{settings}, &ok); err != nil {
		return err
	}
	if !ok {
		return errors.New("NZBGet did not save the configuration")
	}
	if err := m.rpc(ctx, "reload", []interface{}{}, &ok); err != nil {
		return err
	}
	if !ok {
		return errors.New("NZBGet did not accept reload")
	}
	return nil
}
func assistantNZBVerify(ctx context.Context, m *assistantMediaTarget, want []assistantNZBOption, changes map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	expected := map[string]string{}
	for _, s := range want {
		expected[s.Name] = s.Value
	}
	for {
		var got []assistantNZBOption
		if err := m.rpc(ctx, "loadconfig", []interface{}{}, &got); err == nil {
			found := map[string]string{}
			for _, s := range got {
				found[s.Name] = s.Value
			}
			match := true
			for k, v := range expected {
				if found[k] != v {
					match = false
					break
				}
			}
			var status map[string]interface{}
			if match && m.rpc(ctx, "status", []interface{}{}, &status) == nil {
				var active []assistantNZBOption
				if m.rpc(ctx, "config", []interface{}{}, &active) == nil && assistantNZBActiveMatches(expected, changes, active) {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("NZBGet did not report the saved configuration after reload")
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func assistantNZBActiveMatches(saved, changes map[string]string, active []assistantNZBOption) bool {
	running := map[string]string{}
	for _, option := range active {
		running[option.Name] = option.Value
	}
	for key, want := range changes {
		for i := 0; i < 8 && strings.Contains(want, "${"); i++ {
			for name, value := range saved {
				want = strings.ReplaceAll(want, "${"+name+"}", value)
			}
		}
		got, ok := running[key]
		if !ok {
			return false
		}
		if strings.HasSuffix(key, "Dir") {
			got, want = path.Clean(got), path.Clean(want)
		}
		if got != want {
			return false
		}
	}
	return true
}

func assistantNZBDirectoryChange(ctx context.Context, m *assistantMediaTarget, current []assistantNZBOption, changes map[string]string) error {
	old := map[string]string{}
	for _, option := range current {
		old[option.Name] = option.Value
	}
	pathsChanged := false
	for key, value := range changes {
		if strings.HasSuffix(key, "Dir") && value != old[key] {
			pathsChanged = true
		}
	}
	if !pathsChanged {
		return nil
	}
	var groups []stdjson.RawMessage
	if err := m.rpc(ctx, "listgroups", []interface{}{}, &groups); err != nil {
		return err
	}
	if len(groups) > 0 {
		return errors.New("NZBGet has queued or processing downloads. Finish them before changing download directories; the assistant will not move or discard queued data")
	}
	return nil
}

func assistantNZBPinQueue(ctx context.Context, m *assistantMediaTarget, current, next []assistantNZBOption, changes, verify map[string]string) error {
	main, changed := changes["MainDir"]
	if !changed {
		return nil
	}
	old := map[string]string{}
	for _, option := range current {
		old[option.Name] = option.Value
	}
	if main == old["MainDir"] || !strings.Contains(old["QueueDir"], "${MainDir}") {
		return nil
	}
	var active []assistantNZBOption
	if err := m.rpc(ctx, "config", []interface{}{}, &active); err != nil {
		return err
	}
	queue := ""
	for _, option := range active {
		if option.Name == "QueueDir" {
			queue = option.Value
		}
	}
	if !path.IsAbs(queue) || queue == "/" || strings.ContainsAny(queue, "$\x00\r\n") {
		return errors.New("cannot preserve NZBGet's current queue/history directory; keep MainDir unchanged")
	}
	for i := range next {
		if next[i].Name == "QueueDir" {
			next[i].Value = queue
			verify["QueueDir"] = queue
			return nil
		}
	}
	return errors.New("cannot locate the queue directory in NZBGet's complete configuration")
}

func assistantSonarrRootPlan(ctx context.Context, raw stdjson.RawMessage) (assistant.Prepared, error) {
	var a assistantPathArgs
	if err := assistantDecode(raw, &a); err != nil {
		return assistant.Prepared{}, err
	}
	m, err := assistantMediaResolve(ctx, a.App, a.Service)
	if err != nil {
		return assistant.Prepared{}, err
	}
	if m.kind != "sonarr" {
		return assistant.Prepared{}, errors.New("select a Sonarr service")
	}
	if _, err = assistantMediaPath(m.app, a.Service, a.Path, true); err != nil {
		return assistant.Prepared{}, err
	}
	return assistant.Prepared{Summary: "Add Sonarr media root " + a.Path + " to " + a.App + " / " + a.Service + ". Existing folders and media will be preserved.", Execute: func(ctx context.Context) (string, error) {
		unlock, err := LockAppOperation(a.App)
		if err != nil {
			return "", err
		}
		defer unlock()
		fresh, err := assistantSameMedia(ctx, m, a.Service)
		if err != nil {
			return "", err
		}
		if _, err = assistantMediaPath(fresh.app, a.Service, a.Path, true); err != nil {
			return "", err
		}
		check := func() (bool, error) {
			data, e := m.request(ctx, http.MethodGet, "/api/v3/rootfolder", nil)
			if e != nil {
				return false, e
			}
			var folders []struct {
				Path       string `json:"path"`
				Accessible bool   `json:"accessible"`
			}
			if e = stdjson.Unmarshal(data, &folders); e != nil {
				return false, e
			}
			for _, f := range folders {
				if f.Path == a.Path {
					return true, nil
				}
			}
			return false, nil
		}
		found, err := check()
		if err != nil {
			return "", err
		}
		if found {
			return "Sonarr already has media root " + a.Path, nil
		}
		if _, err = m.request(ctx, http.MethodPost, "/api/v3/rootfolder", map[string]string{"path": a.Path}); err != nil {
			return "", err
		}
		found, err = check()
		if err != nil {
			return "The root folder was submitted; verification failed.", err
		}
		if !found {
			return "", errors.New("Sonarr did not return the new root folder")
		}
		return "Sonarr media root added and verified: " + a.Path, nil
	}}, nil
}

func assistantSonarrIndexerPlan(ctx context.Context, raw stdjson.RawMessage) (assistant.Prepared, error) {
	var a struct {
		App     string `json:"app"`
		Service string `json:"service"`
		Name    string `json:"name"`
		URL     string `json:"url"`
		APIKey  string `json:"api_key"`
	}
	if err := assistantDecode(raw, &a); err != nil {
		return assistant.Prepared{}, err
	}
	u, err := url.Parse(a.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return assistant.Prepared{}, errors.New("provide an HTTP(S) indexer base URL without embedded credentials or query")
	}
	if strings.TrimSpace(a.Name) == "" || len(a.Name) > 100 || len(a.APIKey) == 0 || len(a.APIKey) > 4096 {
		return assistant.Prepared{}, errors.New("indexer name and API key are required")
	}
	m, err := assistantMediaResolve(ctx, a.App, a.Service)
	if err != nil {
		return assistant.Prepared{}, err
	}
	if m.kind != "sonarr" {
		return assistant.Prepared{}, errors.New("select a Sonarr service")
	}
	old, err := m.request(ctx, http.MethodGet, "/api/v3/indexer", nil)
	if err != nil {
		return assistant.Prepared{}, err
	}
	hash := sha256.Sum256(old)
	var items []map[string]interface{}
	if err = stdjson.Unmarshal(old, &items); err != nil {
		return assistant.Prepared{}, err
	}
	var payload map[string]interface{}
	for _, item := range items {
		if item["name"] == a.Name {
			if item["implementation"] != "Newznab" {
				return assistant.Prepared{}, errors.New("an indexer with this name uses another protocol")
			}
			payload = item
		}
	}
	if payload == nil {
		data, e := m.request(ctx, http.MethodGet, "/api/v3/indexer/schema", nil)
		if e != nil {
			return assistant.Prepared{}, e
		}
		if e = stdjson.Unmarshal(data, &items); e != nil {
			return assistant.Prepared{}, e
		}
		for _, item := range items {
			if item["implementation"] == "Newznab" {
				payload = item
				break
			}
		}
	}
	if payload == nil {
		return assistant.Prepared{}, errors.New("Sonarr did not provide a Newznab schema")
	}
	payload["name"] = a.Name
	payload["enableRss"] = true
	payload["enableAutomaticSearch"] = true
	payload["enableInteractiveSearch"] = true
	fields, ok := payload["fields"].([]interface{})
	if !ok {
		return assistant.Prepared{}, errors.New("invalid indexer schema")
	}
	for _, entry := range fields {
		field, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		switch field["name"] {
		case "baseUrl":
			field["value"] = strings.TrimRight(a.URL, "/")
		case "apiKey":
			field["value"] = a.APIKey
		}
	}
	return assistant.Prepared{Summary: "Test and configure Sonarr Newznab indexer " + a.Name + " at " + a.URL + " for " + a.App + " / " + a.Service + ". Enable RSS and search. A private backup is kept before changes.", Execute: func(ctx context.Context) (string, error) {
		unlock, err := LockAppOperation(a.App)
		if err != nil {
			return "", err
		}
		defer unlock()
		if _, err = assistantSameMedia(ctx, m, a.Service); err != nil {
			return "", err
		}
		current, err := m.request(ctx, http.MethodGet, "/api/v3/indexer", nil)
		if err != nil {
			return "", err
		}
		if sha256.Sum256(current) != hash {
			return "", errors.New("Sonarr indexers changed since the proposal")
		}
		if _, err = m.request(ctx, http.MethodPost, "/api/v3/indexer/test", payload); err != nil {
			return "", errors.New(assistant.Redact(err.Error(), a.APIKey))
		}
		if _, err = assistantNativeBackup(m.app, a.Service, "sonarr-indexers", old); err != nil {
			return "", err
		}
		method := http.MethodPost
		endpoint := "/api/v3/indexer"
		if id, ok := payload["id"].(float64); ok && id > 0 {
			method = http.MethodPut
			endpoint += "/" + strconv.Itoa(int(id))
		}
		if _, err = m.request(ctx, method, endpoint, payload); err != nil {
			return "", errors.New(assistant.Redact(err.Error(), a.APIKey))
		}
		saved, err := m.request(ctx, http.MethodGet, "/api/v3/indexer", nil)
		if err != nil {
			return "Indexer submitted, but read-back failed.", err
		}
		var items []map[string]interface{}
		if err = stdjson.Unmarshal(saved, &items); err != nil {
			return "", err
		}
		for _, item := range items {
			if item["name"] == a.Name {
				return "Sonarr indexer tested and saved: " + a.Name, nil
			}
		}
		return "", errors.New("Sonarr did not return the saved indexer")
	}}, nil
}
