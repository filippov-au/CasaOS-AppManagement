package service

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IceWhaleTech/CasaOS-AppManagement/pkg/docker"
	"github.com/compose-spec/compose-go/types"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestRegistryChecksDoNotDependOnStoreOrTouchRecovery(t *testing.T) {
	m, runtime, app := updateFixture(t)
	m.target = func(*ComposeApp) (*ComposeApp, error) {
		t.Fatal("registry check consulted store")
		return nil, errors.New("unmanaged")
	}
	m.registryCheck = func(context.Context, *ComposeApp) []docker.ImageUpdate {
		return []docker.ImageUpdate{{Service: "web", Image: "test:2", Status: "available", LatestVersion: "3.0.0"}}
	}
	before := &updateRecord{Status: AppUpdateStatus{ID: app.Name, CheckStatus: "unmanaged", Operation: "idle"}, Previous: runtime.snapshot}
	if err := m.save(app.Name, before); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckRegistry(context.Background(), map[string]*ComposeApp{app.Name: app}); err != nil {
		t.Fatal(err)
	}
	after, err := m.read(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status.RegistryCheckedAt == nil || len(after.Status.RegistryImages) != 1 || after.Status.RegistryImages[0].Status != "available" {
		t.Fatalf("%+v", after.Status)
	}
	if after.Status.CheckStatus != "unmanaged" || string(after.Previous.YAML) != string(before.Previous.YAML) || len(runtime.calls) != 0 {
		t.Fatal("registry check changed store state or recovery")
	}
	m.registryCheck = func(context.Context, *ComposeApp) []docker.ImageUpdate {
		return []docker.ImageUpdate{{Service: "web", Image: "test:2", Status: "failed", Error: "Registry unavailable"}}
	}
	if err := m.CheckRegistry(context.Background(), map[string]*ComposeApp{app.Name: app}); err != nil {
		t.Fatal(err)
	}
	after, _ = m.read(app.Name)
	if after.Status.RegistryImages[0].Status != "failed" || after.Status.RegistryImages[0].LatestVersion != "" {
		t.Fatal("stale registry result survived failed check")
	}
}

func TestRegistryChecksRespectAppAndCheckLocks(t *testing.T) {
	m, _, app := updateFixture(t)
	m.registryCheck = func(context.Context, *ComposeApp) []docker.ImageUpdate { t.Fatal("checked busy app"); return nil }
	unlock, err := LockAppOperation(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := m.CheckRegistry(context.Background(), map[string]*ComposeApp{app.Name: app}); err != nil {
		t.Fatal(err)
	}
	m.checkMu.Lock()
	defer m.checkMu.Unlock()
	if err := m.CheckRegistry(context.Background(), nil); !errors.Is(err, ErrAppOperationBusy) {
		t.Fatal(err)
	}
}

// fakeImageRegistry serves the manifest, tag and config endpoints a registry
// check reads, so the multi-service fan-out runs without network access.
type fakeImageRegistry struct {
	host      string
	tags      map[string][]string
	manifests map[string]map[string][]byte
	blobs     map[string][]byte
}

func newFakeImageRegistry(t *testing.T) *fakeImageRegistry {
	t.Helper()
	f := &fakeImageRegistry{tags: map[string][]string{}, manifests: map[string]map[string][]byte{}, blobs: map[string][]byte{}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch path := r.URL.Path; {
		case path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(path, "/tags/list"):
			repo := strings.TrimSuffix(strings.TrimPrefix(path, "/v2/"), "/tags/list")
			_ = stdjson.NewEncoder(w).Encode(map[string]interface{}{"name": repo, "tags": f.tags[repo]})
		case strings.Contains(path, "/manifests/"):
			repo, ref, _ := strings.Cut(strings.TrimPrefix(path, "/v2/"), "/manifests/")
			data, ok := f.manifests[repo][ref]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", v1.MediaTypeImageManifest)
			w.Header().Set("Docker-Content-Digest", digest.FromBytes(data).String())
			_, _ = w.Write(data)
		case strings.Contains(path, "/blobs/"):
			_, ref, _ := strings.Cut(path, "/blobs/")
			data, ok := f.blobs[ref]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	previous := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("REPO_USER", "")
	t.Setenv("REPO_PASS", "")
	t.Cleanup(func() { http.DefaultTransport = previous; server.Close() })
	f.host = strings.TrimPrefix(server.URL, "https://")
	return f
}

// add publishes a single-platform image and returns its immutable config digest.
func (f *fakeImageRegistry) add(repo, tag, version string) string {
	config, _ := json.Marshal(map[string]interface{}{
		"architecture": "amd64", "os": "linux",
		"config": map[string]interface{}{"Labels": map[string]string{"org.opencontainers.image.version": version}},
	})
	id := digest.FromBytes(config)
	f.blobs[id.String()] = config
	manifest, _ := json.Marshal(map[string]interface{}{
		"schemaVersion": 2, "mediaType": v1.MediaTypeImageManifest,
		"config": map[string]interface{}{"mediaType": v1.MediaTypeImageConfig, "digest": id.String(), "size": len(config)},
		"layers": []interface{}{},
	})
	if f.manifests[repo] == nil {
		f.manifests[repo] = map[string][]byte{}
	}
	f.manifests[repo][tag] = manifest
	f.tags[repo] = append(f.tags[repo], tag)
	return id.String()
}

type fakeInstalledImage struct {
	reference string
	version   string
}

// fakeDockerAPI answers the two Docker calls a check makes: listing an app's
// containers, and inspecting the image each container runs.
func fakeDockerAPI(t *testing.T, app string, containers map[string]string, byID map[string]fakeInstalledImage) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_ping":
			w.Header().Set("API-Version", "1.44")
			_, _ = w.Write([]byte("OK"))
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			list := make([]map[string]interface{}, 0, len(containers))
			for service, id := range containers {
				list = append(list, map[string]interface{}{
					"Id": service + "-container", "ImageID": id,
					"Labels": map[string]string{"com.docker.compose.project": app, "com.docker.compose.service": service},
				})
			}
			_ = stdjson.NewEncoder(w).Encode(list)
		case strings.Contains(r.URL.Path, "/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			_, id, _ := strings.Cut(r.URL.Path, "/images/")
			id = strings.TrimSuffix(id, "/json")
			image, ok := byID[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = stdjson.NewEncoder(w).Encode(map[string]interface{}{
				"Id": id, "Os": "linux", "Architecture": "amd64", "RepoTags": []string{image.reference},
				"Config": map[string]interface{}{"Labels": map[string]string{"org.opencontainers.image.version": image.version}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(server.URL, "http://"))
	t.Cleanup(server.Close)
}

// Results are positional and carry their own service, so a shared loop variable
// or an unsynchronized write would move one service's version onto another.
func TestResolveAppUpdateImagesChecksEveryService(t *testing.T) {
	registry := newFakeImageRegistry(t)
	repository := registry.host + "/team/demo"
	oldID := registry.add("team/demo", "1.0.0", "1.0.0")
	newID := registry.add("team/demo", "2.0.0", "2.0.0")
	fakeDockerAPI(t, "parallel-app",
		map[string]string{"web": oldID, "worker": newID, "db": newID},
		map[string]fakeInstalledImage{
			oldID: {reference: repository + ":1.0.0", version: "1.0.0"},
			newID: {reference: repository + ":2.0.0", version: "2.0.0"},
		})

	app := &ComposeApp{Name: "parallel-app", Services: types.Services{
		{Name: "web", Image: repository + ":1.0.0"},
		{Name: "worker", Image: repository + ":2.0.0"},
		{Name: "db", Image: repository + ":2.0.0"},
	}}
	target, err := cloneCompose(app)
	if err != nil {
		t.Fatal(err)
	}
	results, err := resolveAppUpdateImages(context.Background(), app, target)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ service, status, image, version string }{
		{"web", "available", repository + ":2.0.0", "2.0.0"},
		{"worker", "up_to_date", repository + ":2.0.0", "2.0.0"},
		{"db", "up_to_date", repository + ":2.0.0", "2.0.0"},
	}
	if len(results) != len(want) {
		t.Fatalf("got %d results for %d services: %+v", len(results), len(want), results)
	}
	for i, expected := range want {
		got := results[i]
		if got.Service != expected.service || got.Status != expected.status || got.LatestImage != expected.image || got.LatestVersion != expected.version || got.Error != "" {
			t.Fatalf("result %d: %+v, want %+v", i, got, expected)
		}
		if target.Services[i].Image != expected.image {
			t.Fatalf("%s: target image %s, want %s", expected.service, target.Services[i].Image, expected.image)
		}
	}
}
