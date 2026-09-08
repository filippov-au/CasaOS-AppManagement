package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IceWhaleTech/CasaOS-AppManagement/common"
	"github.com/IceWhaleTech/CasaOS-AppManagement/pkg/docker"
	"github.com/compose-spec/compose-go/types"
)

func combinedFixture(t *testing.T) (*UpdateManager, *fakeUpdateRuntime, *ComposeApp) {
	t.Helper()
	m, runtime, app := updateFixture(t)
	app.Services[0].Image = "ghcr.io/advplyr/audiobookshelf:2.23.0"
	secret := "preserve$secret"
	app.Services[0].Environment = types.MappingWithEquals{"PASSWORD": &secret}
	path := filepath.Join(t.TempDir(), "compose.yaml")
	data, err := resolvedComposeYAML(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	app, err = LoadComposeAppFromConfigFile(app.Name, path)
	if err != nil {
		t.Fatal(err)
	}
	m.target = func(app *ComposeApp) (*ComposeApp, error) {
		target, _ := cloneCompose(app)
		target.Services[0].Image = "ghcr.io/advplyr/audiobookshelf:2.30.0"
		return target, nil
	}
	m.resolve = func(_ context.Context, installed, target *ComposeApp) ([]docker.ImageUpdate, error) {
		if target.Services[0].Image != "ghcr.io/advplyr/audiobookshelf:2.30.0" {
			t.Fatal("did not start with catalog image")
		}
		target.Services[0].Image = "ghcr.io/advplyr/audiobookshelf:2.36.0"
		return []docker.ImageUpdate{{Service: "web", Image: "ghcr.io/advplyr/audiobookshelf:2.30.0", InstalledImage: installed.Services[0].Image, LatestImage: target.Services[0].Image, LatestImageID: "sha256:" + strings.Repeat("a", 64), Status: "available"}}, nil
	}
	return m, runtime, app
}

func TestCombinedUpdateInstallsOfferedRegistryVersionOverCatalog(t *testing.T) {
	m, runtime, app := combinedFixture(t)
	if err := m.CheckCombined(context.Background(), map[string]*ComposeApp{app.Name: app}); err != nil {
		t.Fatal(err)
	}
	record, err := m.read(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status.CurrentVersion != "2.23.0" || record.Status.TargetVersion != "2.36.0" || record.Plan == nil || record.Status.CheckStatus != "available" {
		t.Fatalf("%+v", record.Status)
	}
	status, err := m.Status(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	if !status.UpdateReady || status.UpdateToken == "" {
		t.Fatal("checked update has no button token")
	}
	public, _ := json.Marshal(status)
	if strings.Contains(string(public), "preserve") || strings.Contains(string(public), "config_hash") {
		t.Fatal("private plan leaked in API")
	}
	if mode, _ := os.Stat(m.recordPath(app.Name)); mode.Mode().Perm() != 0600 {
		t.Fatal("plan not private")
	}
	target, err := loadCheckedTarget(app, record)
	if err != nil {
		t.Fatal(err)
	}
	if target.Services[0].Image != "ghcr.io/advplyr/audiobookshelf:2.36.0" || *target.Services[0].Environment["PASSWORD"] != "preserve$secret" {
		t.Fatal("checked image or settings lost")
	}
	if err := m.run(context.Background(), app, target, record, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(runtime.applied), "2.36.0") || strings.Contains(string(runtime.applied), "2.30.0") {
		t.Fatal("installed catalog version instead of displayed version")
	}
	if strings.Join(runtime.calls, ",") != "capture,pull,verify,apply" || record.Plan != nil || record.Previous == nil {
		t.Fatalf("transaction: %v", runtime.calls)
	}
	if err := m.run(context.Background(), app, nil, record, true); err != nil {
		t.Fatal(err)
	}
	if string(runtime.applied) != "old configuration with secret" || record.Status.Operation != "reverted" {
		t.Fatal("rollback lost")
	}
}

func TestCombinedUpdateRejectsStaleSettingsAndConfirmation(t *testing.T) {
	m, _, app := combinedFixture(t)
	if err := m.CheckCombined(context.Background(), map[string]*ComposeApp{app.Name: app}); err != nil {
		t.Fatal(err)
	}
	record, _ := m.read(app.Name)
	if err := m.StartChecked(context.Background(), app, "stale-token"); !errors.Is(err, ErrUpdatePlanStale) {
		t.Fatal(err)
	}
	app.Services[0].Ports = []types.ServicePortConfig{{Published: "9999", Target: 80}}
	if _, err := loadCheckedTarget(app, record); !errors.Is(err, ErrUpdatePlanStale) {
		t.Fatal("settings change not detected", err)
	}
	record.Plan.CreatedAt = time.Now().Add(-25 * time.Hour)
	if _, err := loadCheckedTarget(app, record); !errors.Is(err, ErrUpdatePlanStale) {
		t.Fatal("expired plan accepted")
	}
}

func TestCombinedUpdateFailureClearsStaleOffer(t *testing.T) {
	m, _, app := combinedFixture(t)
	if err := m.CheckCombined(context.Background(), map[string]*ComposeApp{app.Name: app}); err != nil {
		t.Fatal(err)
	}
	m.resolve = func(context.Context, *ComposeApp, *ComposeApp) ([]docker.ImageUpdate, error) {
		return []docker.ImageUpdate{{Service: "web", Status: "failed", Error: "Registry unavailable"}}, nil
	}
	if err := m.CheckCombined(context.Background(), map[string]*ComposeApp{app.Name: app}); err != nil {
		t.Fatal(err)
	}
	record, _ := m.read(app.Name)
	status, _ := m.Status(context.Background(), app)
	if record.Plan != nil || status.CheckStatus != "failed" || status.UpdateReady || status.UpdateToken != "" {
		t.Fatal("stale offer survived failure")
	}
	if _, err := loadCheckedTarget(app, record); !errors.Is(err, ErrUpdatePlanStale) {
		t.Fatal("failed check can start install")
	}
}

func TestCombinedUpdateVerifiesImagesBeforeReplacingContainers(t *testing.T) {
	m, runtime, app := combinedFixture(t)
	if err := m.CheckCombined(context.Background(), map[string]*ComposeApp{app.Name: app}); err != nil {
		t.Fatal(err)
	}
	record, _ := m.read(app.Name)
	target, err := loadCheckedTarget(app, record)
	if err != nil {
		t.Fatal(err)
	}
	runtime.fail = "verify"
	if err := m.run(context.Background(), app, target, record, false); err == nil {
		t.Fatal("changed image accepted")
	}
	if runtime.applied != nil || record.Pending != nil {
		t.Fatal("verification failure touched containers or left pending recovery")
	}
}

func TestCatalogDefaultsPreserveSettingsAndBecomeCurrentAfterInstall(t *testing.T) {
	_, _, app := updateFixture(t)
	value := "my-secret"
	app.Services[0].Environment = types.MappingWithEquals{"PASSWORD": &value}
	app.Services[0].Volumes = []types.ServiceVolumeConfig{{Type: "bind", Source: "/custom/data", Target: "/data"}}
	store, _ := cloneCompose(app)
	newDefault := "new-default"
	store.Services[0].Environment["NEW_SETTING"] = &newDefault
	differentPassword := "store-password"
	store.Services[0].Environment["PASSWORD"] = &differentPassword
	store.Services[0].Volumes[0].Source = "/store/data"
	store.Services[0].HealthCheck = &types.HealthCheckConfig{Test: types.HealthCheckTest{"CMD", "true"}}
	store.Extensions[common.ComposeExtensionNameXCasaOS].(map[string]interface{})["icon"] = "https://example.com/new.png"
	target, err := mergeStoreImages(app, store)
	if err != nil {
		t.Fatal(err)
	}
	if *target.Services[0].Environment["PASSWORD"] != value || *target.Services[0].Environment["NEW_SETTING"] != newDefault || target.Services[0].Volumes[0].Source != "/custom/data" || target.Services[0].HealthCheck == nil {
		t.Fatal("store defaults or installed settings lost")
	}
	changed, err := definitionChanged(app, target)
	if err != nil || !changed {
		t.Fatal("catalog-only update missed", err)
	}
	next, err := mergeStoreImages(target, store)
	if err != nil {
		t.Fatal(err)
	}
	changed, err = definitionChanged(target, next)
	if err != nil || changed {
		t.Fatal("catalog update repeats forever", err)
	}
}

func TestUpdateImageBaselineNeverDowngradesOrUnpins(t *testing.T) {
	for _, tc := range []struct{ installed, catalog, want string }{
		{"app:2.36.0", "app:2.30.0", "app:2.36.0"},
		{"app:2.23.0", "app:2.30.0", "app:2.30.0"},
		{"app:latest", "app:stable", "app:stable"},
		{"old/app:2.36.0", "new/app:3.0.0", "new/app:3.0.0"},
		{"app@sha256:" + strings.Repeat("a", 64), "app:2.30.0", "app@sha256:" + strings.Repeat("a", 64)},
	} {
		if got := updateImageBaseline(tc.installed, tc.catalog); got != tc.want {
			t.Fatalf("%s: %s", tc.installed, got)
		}
	}
}
