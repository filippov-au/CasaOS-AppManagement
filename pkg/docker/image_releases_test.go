package docker

import (
	"context"
	"reflect"
	"strings"
	"testing"

	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
)

func TestReleaseCandidatesExcludeDevelopmentAndPreserveVariants(t *testing.T) {
	tags := []string{"1.25.3", "1.26.0", "v1.27.0", "1.27.0-rc1", "2.0.0-beta", "latest", "nightly", "sha-abcdef", "1.26.0-rootless", "1.28.0-rootless", "1.27.0-alpine", "16.2.0", "17.1.0"}
	for _, tc := range []struct {
		channel, installed string
		want               []string
	}{
		{"latest", "1.25.3", []string{"17.1.0", "16.2.0", "v1.27.0", "1.26.0", "1.25.3"}},
		{"latest-rootless", "1.25.3", []string{"1.28.0-rootless", "1.26.0-rootless"}},
		{"16", "16.1.0", []string{"16.2.0"}},
		{"1.25", "1.25.3", []string{"1.25.3"}},
		{"nightly", "1.25.3", nil},
	} {
		var got []string
		for _, release := range releaseCandidates(tc.channel, tc.installed, tags) {
			got = append(got, release.tag)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: %v", tc.channel, got)
		}
	}
	got := releaseCandidates("latest", "5.0.3.8127-ls190", []string{"5.0.3.8127-ls189", "5.0.3.8127-ls191", "5.1.0.9000-ls200", "5.2.0-develop"})
	if len(got) != 2 || got[0].tag != "5.1.0.9000-ls200" {
		t.Fatal(got)
	}
	got = releaseCandidates("latest", "1.25.3", []string{"1.26", "1.26.0"})
	if got[0].tag != "1.26.0" {
		t.Fatal("selected a moving minor alias", got)
	}
}

func TestReleaseUpdateReplacesLatestWithVerifiedNumberedTag(t *testing.T) {
	f := newRegistryFixture(t)
	oldID := f.addImage("1.25.3", "amd64", "old")
	newID := f.addImage("1.26.0", "amd64", "release")
	f.addImage("1.27.0", "arm64", "other-platform")
	f.addImage("9.0.0-rc1", "amd64", "prerelease")
	f.addImage("latest", "amd64", "unknown-build")
	f.addImage("nightly", "amd64", "development")
	repository := strings.TrimPrefix(f.server.URL, "https://") + "/team/demo:"
	installed := dockerTypes.ImageInspect{ID: oldID, Os: "linux", Architecture: "amd64", Config: &container.Config{Labels: map[string]string{"org.opencontainers.image.version": "1.25.3"}}}
	result := ResolveImageUpdate(context.Background(), repository+"latest", installed)
	if result.Status != "available" || result.LatestVersion != "1.26.0" || result.LatestImage != repository+"1.26.0" || result.LatestImageID != newID {
		t.Fatalf("%+v", result)
	}
	installed.ID, installed.Config.Labels["org.opencontainers.image.version"] = newID, "1.26.0"
	result = ResolveImageUpdate(context.Background(), repository+"1.26.0", installed)
	if result.Status != "up_to_date" {
		t.Fatalf("repeated update: %+v", result)
	}
}

func TestReleaseUpdateNeverFallsBackToUnversionedBuildOrDowngrade(t *testing.T) {
	f := newRegistryFixture(t)
	id := f.addImage("1.24.0", "amd64", "old")
	f.addImage("latest", "amd64", "unknown")
	f.addImage("1.99.0-dev", "amd64", "dev")
	installed := dockerTypes.ImageInspect{ID: id, Os: "linux", Architecture: "amd64", Config: &container.Config{Labels: map[string]string{"org.opencontainers.image.version": "1.25.3"}}}
	image := strings.TrimPrefix(f.server.URL, "https://") + "/team/demo:latest"
	result := ResolveImageUpdate(context.Background(), image, installed)
	if result.Status != "failed" || result.LatestImage != "" || result.Error == "" {
		t.Fatalf("unsafe fallback: %+v", result)
	}
	f.failTags = true
	result = ResolveImageUpdate(context.Background(), image, installed)
	if result.Status != "failed" || result.LatestImage != "" {
		t.Fatalf("tag-list failure hidden: %+v", result)
	}
}
