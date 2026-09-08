package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	dockerTypes "github.com/docker/docker/api/types"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestNewerStableTags(t *testing.T) {
	tags := []string{"1.2.3", "1.10.0", "1.9.0", "2.0.0", "9.0.0-beta", "3.0.0-alpine", "latest", "v4.0.0", "1.2", "1.2.4"}
	if got := newerStableTags("1.2.3", tags); !reflect.DeepEqual(got, []string{"2.0.0", "1.10.0", "1.9.0", "1.2.4"}) {
		t.Fatal(got)
	}
	for _, tag := range []string{"latest", "stable", "1", "1.2", "1.2.3-alpine", "1.2.3-rc1"} {
		if got := newerStableTags(tag, tags); len(got) != 0 {
			t.Fatalf("changed channel %s: %v", tag, got)
		}
	}
	if got := newerStableTags("v1.0.0", tags); !reflect.DeepEqual(got, []string{"v4.0.0"}) {
		t.Fatal(got)
	}
}

type registryFixture struct {
	server    *httptest.Server
	tags      []string
	manifests map[string][]byte
	blobs     map[string][]byte
	failTags  bool
	bearer    bool
	basic     bool
}

func newRegistryFixture(t *testing.T) *registryFixture {
	t.Helper()
	f := &registryFixture{manifests: map[string][]byte{}, blobs: map[string][]byte{}}
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			t.Errorf("registry check attempted mutation: %s", r.Method)
		}
		if f.basic {
			user, password, ok := r.BasicAuth()
			if !ok || user != "registry-user" || password != "registry-password" {
				w.Header().Set("WWW-Authenticate", `Basic realm="Registry"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		if r.URL.Path == "/Token" {
			if r.URL.Query().Get("scope") != "repository:team/demo:pull" {
				t.Errorf("wrong token scope: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"CaseSensitiveToken"}`))
			return
		}
		if f.bearer && r.Header.Get("Authorization") != "Bearer CaseSensitiveToken" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+f.server.URL+`/Token",service="TestRegistry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v2/team/demo/tags/list" {
			if f.failTags {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"name": "team/demo", "tags": f.tags})
			return
		}
		if key := strings.TrimPrefix(r.URL.Path, "/v2/team/demo/manifests/"); key != r.URL.Path {
			if data, ok := f.manifests[key]; ok {
				kind := v1.MediaTypeImageManifest
				if strings.Contains(string(data), `"manifests"`) {
					kind = v1.MediaTypeImageIndex
				}
				w.Header().Set("Content-Type", kind)
				w.Header().Set("Docker-Content-Digest", digest.FromBytes(data).String())
				_, _ = w.Write(data)
				return
			}
		}
		if key := strings.TrimPrefix(r.URL.Path, "/v2/team/demo/blobs/"); key != r.URL.Path {
			if data, ok := f.blobs[key]; ok {
				_, _ = w.Write(data)
				return
			}
		}
		http.NotFound(w, r)
	}))
	previous := http.DefaultTransport
	http.DefaultTransport = f.server.Client().Transport
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("REPO_USER", "")
	t.Setenv("REPO_PASS", "")
	t.Cleanup(func() { http.DefaultTransport = previous; f.server.Close() })
	return f
}

func (f *registryFixture) addImage(tag, arch, version string) string {
	config, _ := json.Marshal(map[string]interface{}{"architecture": arch, "os": "linux", "config": map[string]interface{}{"Env": []string{"VERSION=" + version}}})
	id := digest.FromBytes(config)
	f.blobs[id.String()] = config
	manifest, _ := json.Marshal(map[string]interface{}{"schemaVersion": 2, "mediaType": v1.MediaTypeImageManifest, "config": map[string]interface{}{"mediaType": v1.MediaTypeImageConfig, "digest": id, "size": len(config)}, "layers": []interface{}{}})
	f.manifests[tag] = manifest
	f.manifests[digest.FromBytes(manifest).String()] = manifest
	f.tags = append(f.tags, tag)
	return id.String()
}

func (f *registryFixture) check(tag, id string) ImageUpdate {
	return CheckImageUpdate(context.Background(), strings.TrimPrefix(f.server.URL, "https://")+"/team/demo:"+tag, dockerTypes.ImageInspect{ID: id, Os: "linux", Architecture: "amd64"})
}

func TestRegistryCheckFindsStableVersionOnInstalledPlatform(t *testing.T) {
	f := newRegistryFixture(t)
	id := f.addImage("1.0.0", "amd64", "1")
	newID := f.addImage("2.0.0", "amd64", "2")
	f.addImage("3.0.0", "arm64", "3")
	f.addImage("9.0.0-rc1", "amd64", "9")
	result := f.check("1.0.0", id)
	if result.Status != "available" || result.LatestVersion != "2.0.0" || result.LatestImageID != newID || result.Error != "" {
		t.Fatalf("%+v", result)
	}
}

func TestRegistryCheckFloatingTagsAndIndexChanges(t *testing.T) {
	f := newRegistryFixture(t)
	id := f.addImage("amd64", "amd64", "1")
	f.addImage("arm64", "arm64", "2")
	index, _ := json.Marshal(map[string]interface{}{"schemaVersion": 2, "mediaType": v1.MediaTypeImageIndex, "manifests": []v1.Descriptor{
		{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromBytes(f.manifests["arm64"]), Size: int64(len(f.manifests["arm64"])), Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}},
		{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromBytes(f.manifests["amd64"]), Size: int64(len(f.manifests["amd64"])), Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}},
	}})
	f.manifests["latest"] = index
	f.failTags = true // Floating tags must not enumerate or change channels.
	result := f.check("latest", id)
	if result.Status != "up_to_date" || result.LatestImageID != id {
		t.Fatalf("%+v", result)
	}
	newID := f.addImage("latest", "amd64", "2")
	result = f.check("latest", id)
	if result.Status != "available" || result.LatestVersion != "latest" || result.LatestImageID != newID {
		t.Fatalf("%+v", result)
	}
}

func TestRegistryCheckSupportsAnonymousAndBearerRegistries(t *testing.T) {
	f := newRegistryFixture(t)
	id := f.addImage("latest", "amd64", "1")
	for _, bearer := range []bool{false, true} {
		f.bearer = bearer
		if result := f.check("latest", id); result.Status != "up_to_date" {
			t.Fatalf("bearer=%v: %+v", bearer, result)
		}
	}
	f.bearer, f.basic = false, true
	t.Setenv("REPO_USER", "registry-user")
	t.Setenv("REPO_PASS", "registry-password")
	if result := f.check("latest", id); result.Status != "up_to_date" {
		t.Fatalf("basic auth: %+v", result)
	}
}

func TestRegistryCheckDoesNotHideFailuresOrFollowPinnedDigests(t *testing.T) {
	f := newRegistryFixture(t)
	id := f.addImage("1.0.0", "amd64", "1")
	f.failTags = true
	result := f.check("1.0.0", id)
	if result.Status != "failed" || result.Error == "" || result.LatestVersion != "" {
		t.Fatalf("%+v", result)
	}
	result = CheckImageUpdate(context.Background(), "example/demo@sha256:"+strings.Repeat("a", 64), dockerTypes.ImageInspect{ID: id})
	if result.Status != "pinned" || result.LatestImage != "" {
		t.Fatalf("%+v", result)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	result = CheckImageUpdate(ctx, strings.TrimPrefix(f.server.URL, "https://")+"/team/demo:latest", dockerTypes.ImageInspect{ID: id, Os: "linux", Architecture: "amd64"})
	if result.Status != "failed" || time.Since(start) > time.Second {
		t.Fatalf("cancel ignored: %+v", result)
	}
}

func TestResolvePinnedImagePreservesDigestAndChecksPlatform(t *testing.T) {
	f := newRegistryFixture(t)
	id := f.addImage("1.0.0", "amd64", "1")
	f.addImage("2.0.0", "amd64", "2")
	f.failTags = true
	pin := strings.TrimPrefix(f.server.URL, "https://") + "/team/demo@" + digest.FromBytes(f.manifests["1.0.0"]).String()
	for _, installedID := range []string{id, "sha256:" + strings.Repeat("a", 64)} {
		result := ResolveImageUpdate(context.Background(), pin, dockerTypes.ImageInspect{ID: installedID, Os: "linux", Architecture: "amd64"})
		status := "up_to_date"
		if installedID != id {
			status = "available"
		}
		if result.Status != status || result.LatestImage != pin || result.LatestImageID != id || result.Error != "" {
			t.Fatalf("pin changed or not verified: %+v", result)
		}
	}
	result := ResolveImageUpdate(context.Background(), pin, dockerTypes.ImageInspect{ID: id, Os: "linux", Architecture: "arm64"})
	if result.Status != "failed" || result.Error == "" {
		t.Fatalf("wrong platform offered: %+v", result)
	}
}
