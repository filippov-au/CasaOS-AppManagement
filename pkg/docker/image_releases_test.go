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

func TestNZBGetReleaseNeverComparesDateAliasesToLinuxServerVersions(t *testing.T) {
	f := newRegistryFixture(t)
	labels := map[string]string{"build_version": "Linuxserver.io version:- v26.2-ls250 Build-date:- 2026-01-01"}
	oldID := f.addLabeledImage("v26.2-ls250", "amd64", "installed", labels)
	wantID := f.addImage("v26.3-ls262", "amd64", "release")
	f.addImage("2021.11.25", "amd64", "old-date-release")
	f.addImage("26.3.20260904", "amd64", "date-build-alias")
	f.addImage("testing-version-cd7e586", "amd64", "testing")
	image := strings.TrimPrefix(f.server.URL, "https://") + "/team/demo:latest"
	result := ResolveImageUpdate(context.Background(), image, dockerTypes.ImageInspect{ID: oldID, Os: "linux", Architecture: "amd64", Config: &container.Config{Labels: labels}})
	if result.LatestVersion != "v26.3-ls262" || result.LatestImageID != wantID || result.Status != "available" {
		t.Fatalf("wrong release family: %+v", result)
	}
	if got := releaseCandidates("latest", "26.2", []string{"2021.11.25", "26.3"}); len(got) != 1 || got[0].tag != "26.3" {
		t.Fatal(got)
	}
	if got := releaseCandidates("latest", "2025.11.25", []string{"2026.1.1", "26.3"}); len(got) != 1 || got[0].tag != "2026.1.1" {
		t.Fatal(got)
	}
}

func TestReleaseIdentifiesUnlabelledInstalledImageBeforeOfferingChanges(t *testing.T) {
	f := newRegistryFixture(t)
	id := f.addImage("2.15.1", "amd64", "installed")
	image := strings.TrimPrefix(f.server.URL, "https://") + "/team/demo:latest"
	installed := dockerTypes.ImageInspect{ID: id, Os: "linux", Architecture: "amd64"}
	result := ResolveImageUpdate(context.Background(), image, installed)
	if result.CurrentVersion != "2.15.1" || result.LatestVersion != "2.15.1" || result.CurrentImageID != result.LatestImageID {
		t.Fatalf("same image not recognised: %+v", result)
	}
	nextID := f.addImage("2.16.0", "amd64", "newer")
	result = ResolveImageUpdate(context.Background(), image, installed)
	if result.CurrentVersion != "2.15.1" || result.LatestVersion != "2.16.0" || result.LatestImageID != nextID {
		t.Fatalf("upgrade direction not resolved: %+v", result)
	}
	installed.ID = "sha256:" + strings.Repeat("f", 64)
	result = ResolveImageUpdate(context.Background(), image, installed)
	if result.Status != "failed" || result.LatestImage != "" || !strings.Contains(result.Error, "downgrade") {
		t.Fatalf("unknown version treated as older: %+v", result)
	}
}

func TestPlexReleaseUpdateRecognizesInstalledBuildBehindMovedAlias(t *testing.T) {
	f := newRegistryFixture(t)
	oldVersion := "1.43.2.10687-563d026ea-ls310"
	labels := map[string]string{"build_version": "Linuxserver.io version:- " + oldVersion + " Build-date:- 2026-01-01"}
	oldID := f.addLabeledImage(oldVersion, "amd64", "installed", labels)
	f.addImage("1.43.2", "amd64", "moved-alias")
	f.addImage("1.43.3", "amd64", "new-alias")
	f.addImage("1.43.2.10687-563d026ea-ls313", "amd64", "packaging-update")
	newVersion := "1.43.3.10896-cb3ebc72d-ls323"
	newID := f.addImage(newVersion, "amd64", "new-release")
	f.addImage("1.43.4.11000-abcdef123-ls324", "arm64", "other-platform")
	image := strings.TrimPrefix(f.server.URL, "https://") + "/team/demo:"
	installed := dockerTypes.ImageInspect{ID: oldID, Os: "linux", Architecture: "amd64", Config: &container.Config{Labels: labels}}
	for _, tag := range []string{"1.43.2", "latest", oldVersion} {
		result := ResolveImageUpdate(context.Background(), image+tag, installed)
		if result.Status != "available" || result.Error != "" || result.CurrentVersion != oldVersion || result.LatestVersion != newVersion || result.LatestImage != image+newVersion || result.LatestImageID != newID {
			t.Fatalf("%s: %+v", tag, result)
		}
	}
	installed.ID = newID
	installed.Config.Labels["build_version"] = "Linuxserver.io version:- " + newVersion
	result := ResolveImageUpdate(context.Background(), image+newVersion, installed)
	if result.Status != "up_to_date" || result.Error != "" {
		t.Fatalf("repeated update: %+v", result)
	}
}

func TestPlexReleaseCandidatesPreserveDowngradeAndChannelChecks(t *testing.T) {
	installed := "1.43.2.10687-563d026ea-ls310"
	tags := []string{
		installed, "1.43.2.10687-563d026ea-ls309", "1.43.2.10686-abcdef123-ls999",
		"1.43.2.10687-abcdef123-ls999", // Changed hash cannot establish ordering.
		"1.43.2.10687-563d026ea-ls313", "1.43.3.10896-cb3ebc72d-ls323",
		"1.43.3", "2026.9.9", "1.43.4.11000-ls324", "1.43.4.11000-abcdef123-ls324-beta",
	}
	for _, tc := range []struct {
		channel string
		want    []string
	}{
		{"1.43.2", []string{tags[5], tags[4], installed}},
		{"1.43", []string{tags[5], tags[4], installed}},
		{"1.43.3", []string{tags[5]}},
		{"1.42", nil},
		{"latest-rootless", nil},
	} {
		var got []string
		for _, release := range releaseCandidates(tc.channel, installed, tags) {
			got = append(got, release.tag)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.channel, got, tc.want)
		}
	}
	for _, tag := range []string{"1.43.2-563d026ea", "1.43.2.10687-563d026ea", "1.43.2.10687-dev-ls310", "1.43.2.10687-563d026ea-ls310-beta"} {
		if _, ok := parseImageRelease(tag); ok {
			t.Fatalf("accepted unsupported release %s", tag)
		}
	}
}
