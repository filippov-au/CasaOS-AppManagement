package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IceWhaleTech/CasaOS-AppManagement/common"
	"github.com/IceWhaleTech/CasaOS-AppManagement/pkg/config"
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
	m.target = func(*ComposeApp) (*ComposeApp, error) {
		t.Fatal("image updates must never look up a marketplace definition")
		return nil, errors.New("duplicate or unavailable store")
	}
	m.resolve = func(_ context.Context, installed, target *ComposeApp) ([]docker.ImageUpdate, error) {
		if target.Services[0].Image != "ghcr.io/advplyr/audiobookshelf:2.23.0" {
			t.Fatal("did not preserve installed image repository and channel")
		}
		target.Services[0].Image = "ghcr.io/advplyr/audiobookshelf:2.36.0"
		return []docker.ImageUpdate{{Service: "web", Image: installed.Services[0].Image, CurrentVersion: "2.23.0", LatestVersion: "2.36.0", InstalledImage: installed.Services[0].Image, LatestImage: target.Services[0].Image, LatestImageID: "sha256:" + strings.Repeat("a", 64), Status: "available"}}, nil
	}
	return m, runtime, app
}

func TestImageUpdateInstallsOfferedRegistryVersionWithoutMarketplace(t *testing.T) {
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

func TestCompletedUpdatesAndPinsNeedNoRegistryRecheck(t *testing.T) {
	for _, kind := range []string{"update", "pin"} {
		t.Run(kind, func(t *testing.T) {
			m, _, app := combinedFixture(t)
			wantVersion := "2.36.0"
			if kind == "pin" {
				wantVersion = "2.23.0"
				m.resolve = func(_ context.Context, installed, target *ComposeApp) ([]docker.ImageUpdate, error) {
					return []docker.ImageUpdate{{Service: "web", Status: "available", CurrentVersion: wantVersion, LatestVersion: wantVersion, CurrentImageID: "sha256:same", LatestImageID: "sha256:same"}}, nil
				}
			}
			if err := m.CheckCombined(context.Background(), map[string]*ComposeApp{app.Name: app}); err != nil {
				t.Fatal(err)
			}
			record, _ := m.read(app.Name)
			if record.Plan == nil || record.Plan.Kind != kind {
				t.Fatalf("expected %s plan", kind)
			}
			target, err := loadCheckedTarget(app, record)
			if err != nil {
				t.Fatal(err)
			}
			m.resolve = func(context.Context, *ComposeApp, *ComposeApp) ([]docker.ImageUpdate, error) {
				t.Fatal("completion must not check the registry again")
				return nil, nil
			}
			if err := m.run(context.Background(), app, target, record, false); err != nil {
				t.Fatal(err)
			}
			status, err := m.Status(context.Background(), target)
			if err != nil {
				t.Fatal(err)
			}
			if status.Operation != "updated" || status.CheckStatus != "up_to_date" || status.CurrentVersion != wantVersion || status.UpdateReady || status.UpdateToken != "" || status.TargetVersion != "" || status.CheckError != "" {
				t.Fatalf("completed offer still pending: %+v", status)
			}
			if !status.RollbackAvailable || status.RollbackVersion != "2.23.0" {
				t.Fatalf("completion lost recovery: %+v", status)
			}
		})
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

func TestImageUpdatesIgnoreDuplicateAndOfflineMarketplaces(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	previous := config.ServerInfo.AppStoreList
	config.ServerInfo.AppStoreList = []string{server.URL, server.URL}
	defer func() { config.ServerInfo.AppStoreList = previous }()
	m, _, app := combinedFixture(t)
	ext := app.Extensions[common.ComposeExtensionNameXCasaOS].(map[string]interface{})
	ext["store_url"] = server.URL
	data, _ := resolvedComposeYAML(app)
	if err := os.WriteFile(app.ComposeFiles[0], data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckCombined(context.Background(), map[string]*ComposeApp{app.Name: app}); err != nil {
		t.Fatal(err)
	}
	record, _ := m.read(app.Name)
	if record.Plan == nil || record.Status.CheckStatus != "available" || requests != 0 {
		t.Fatalf("marketplace blocked image update: %+v, requests=%d", record.Status, requests)
	}
	target, err := loadCheckedTarget(app, record)
	if err != nil {
		t.Fatal(err)
	}
	if *target.Services[0].Environment["PASSWORD"] != "preserve$secret" {
		t.Fatal("installed settings changed")
	}
}

func TestCheckedVersionsUseMainServiceImageMetadata(t *testing.T) {
	_, _, app := combinedFixture(t)
	status := AppUpdateStatus{}
	setCheckedVersions(app, []docker.ImageUpdate{
		{Service: "sidecar", CurrentVersion: "wrong", LatestVersion: "wrong"},
		{Service: "web", CurrentVersion: "5.0.0-ls1", LatestVersion: "5.1.0-ls2"},
	}, &status)
	if status.CurrentVersion != "5.0.0-ls1" || status.TargetVersion != "5.1.0-ls2" {
		t.Fatalf("%+v", status)
	}
}

func TestOldUnversionedPlanRequiresAnotherCheck(t *testing.T) {
	m, _, app := combinedFixture(t)
	if err := m.CheckCombined(context.Background(), map[string]*ComposeApp{app.Name: app}); err != nil {
		t.Fatal(err)
	}
	record, _ := m.read(app.Name)
	record.Plan.NumberedReleases = false
	if err := m.save(app.Name, record); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status(context.Background(), app)
	if err != nil || status.UpdateReady || status.UpdateToken != "" {
		t.Fatalf("old build offer is installable: %+v %v", status, err)
	}
	if err := m.StartChecked(context.Background(), app, record.Plan.Token); !errors.Is(err, ErrUpdatePlanStale) {
		t.Fatal("old build offer accepted", err)
	}
}

func TestPinRequiresIdenticalImagesForEveryService(t *testing.T) {
	m, _, app := combinedFixture(t)
	m.resolve = func(_ context.Context, installed, target *ComposeApp) ([]docker.ImageUpdate, error) {
		target.Services[0].Image = "ghcr.io/advplyr/audiobookshelf:2.23.0"
		return []docker.ImageUpdate{{Service: "web", Status: "available", CurrentVersion: "2.23.0", LatestVersion: "2.23.0", CurrentImageID: "sha256:same", LatestImageID: "sha256:same"}}, nil
	}
	if err := m.CheckCombined(context.Background(), map[string]*ComposeApp{app.Name: app}); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status(context.Background(), app)
	if err != nil || !status.UpdateReady || status.UpdateKind != "pin" {
		t.Fatalf("%+v %v", status, err)
	}
	for _, images := range [][]docker.ImageUpdate{
		{{CurrentImageID: "sha256:a", LatestImageID: "sha256:b", CurrentVersion: "1.0", LatestVersion: "1.0"}},
		{{CurrentImageID: "sha256:a", LatestImageID: "sha256:a"}, {CurrentImageID: "sha256:b", LatestImageID: "sha256:c"}},
		{{LatestImageID: "sha256:a"}},
		nil,
	} {
		if checkedUpdateKind(images) != "update" {
			t.Fatal("real or unknown image change reported as a pin", images)
		}
	}
	record, _ := m.read(app.Name)
	record.Plan.ReleasePolicy = 0
	if err := m.save(app.Name, record); err != nil {
		t.Fatal(err)
	}
	status, _ = m.Status(context.Background(), app)
	if status.UpdateReady {
		t.Fatal("old sorting policy remains installable")
	}
	if err := m.StartChecked(context.Background(), app, record.Plan.Token); !errors.Is(err, ErrUpdatePlanStale) {
		t.Fatal(err)
	}
}

// Each app keeps its own record, so a check must publish every app's result even
// though the apps are checked together.
func combinedAppsFixture(t *testing.T, names ...string) (*UpdateManager, map[string]*ComposeApp) {
	t.Helper()
	m, _, _ := updateFixture(t)
	dir := t.TempDir()
	apps := make(map[string]*ComposeApp, len(names))
	for _, name := range names {
		app := &ComposeApp{
			Name:       name,
			Services:   types.Services{{Name: "web", Image: "ghcr.io/advplyr/audiobookshelf:2.23.0"}},
			Extensions: types.Extensions{common.ComposeExtensionNameXCasaOS: map[string]interface{}{"main": "web"}},
		}
		path := filepath.Join(dir, name+".yaml")
		data, err := resolvedComposeYAML(app)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadComposeAppFromConfigFile(name, path)
		if err != nil {
			t.Fatal(err)
		}
		apps[name] = loaded
	}
	return m, apps
}

func TestCombinedUpdateRecordsEveryCheckedApp(t *testing.T) {
	m, apps := combinedAppsFixture(t, "first-app", "second-app", "third-app")
	m.resolve = func(_ context.Context, installed, _ *ComposeApp) ([]docker.ImageUpdate, error) {
		image := installed.Services[0].Image
		return []docker.ImageUpdate{{
			Service: "web", Image: image, Status: "up_to_date",
			CurrentVersion: installed.Name, LatestVersion: installed.Name,
			CurrentImageID: "sha256:same", LatestImageID: "sha256:same", LatestImage: image,
		}}, nil
	}
	if err := m.CheckCombined(context.Background(), apps); err != nil {
		t.Fatal(err)
	}
	for name := range apps {
		record, err := m.read(name)
		if err != nil {
			t.Fatal(err)
		}
		if record.Status.CheckStatus != "up_to_date" || record.Status.CurrentVersion != name || record.Status.TargetVersion != name || record.Status.CheckedAt == nil {
			t.Fatalf("%s: %+v", name, record.Status)
		}
	}
}

// A check that is abandoned must leave the last saved status alone: its results
// are incomplete and would otherwise report healthy apps as failed.
func TestAbandonedCheckKeepsLastSavedStatus(t *testing.T) {
	m, _, app := combinedFixture(t)
	checkedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	before, err := m.read(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	before.Combined = true
	before.Status = AppUpdateStatus{ID: app.Name, Operation: "idle", CheckStatus: "up_to_date", CheckedAt: &checkedAt}
	if err := m.save(app.Name, before); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	m.resolve = func(ctx context.Context, _, _ *ComposeApp) ([]docker.ImageUpdate, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.CheckCombined(ctx, map[string]*ComposeApp{app.Name: app}) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	after, err := m.read(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status.CheckStatus != "up_to_date" || after.Status.CheckedAt == nil || !after.Status.CheckedAt.Equal(checkedAt) {
		t.Fatalf("%+v", after.Status)
	}
}
