package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/IceWhaleTech/CasaOS-AppManagement/common"
	"github.com/IceWhaleTech/CasaOS-Common/utils/logger"
	"github.com/compose-spec/compose-go/types"
)

type fakeUpdateRuntime struct {
	calls    []string
	fail     string
	snapshot *updateSnapshot
	applied  []byte
}

func (f *fakeUpdateRuntime) call(name string) error {
	f.calls = append(f.calls, name)
	if f.fail == name {
		return errors.New(name + " failed")
	}
	return nil
}
func (f *fakeUpdateRuntime) Capture(context.Context, *ComposeApp) (*updateSnapshot, error) {
	return f.snapshot, f.call("capture")
}
func (f *fakeUpdateRuntime) Pull(context.Context, *ComposeApp) error { return f.call("pull") }
func (f *fakeUpdateRuntime) Verify(context.Context, *ComposeApp, map[string]string) error {
	return f.call("verify")
}
func (f *fakeUpdateRuntime) Apply(_ context.Context, _ *ComposeApp, b []byte) error {
	f.applied = b
	return f.call("apply")
}
func (f *fakeUpdateRuntime) Available(context.Context, *updateSnapshot) error {
	return f.call("available")
}
func (f *fakeUpdateRuntime) Release(_ context.Context, s *updateSnapshot) {
	if s != nil {
		f.calls = append(f.calls, "release:"+s.Version)
	}
}
func (f *fakeUpdateRuntime) Check(context.Context, *ComposeApp, *ComposeApp) (bool, error) {
	return true, f.call("check")
}

func updateFixture(t *testing.T) (*UpdateManager, *fakeUpdateRuntime, *ComposeApp) {
	t.Helper()
	logger.LogInitConsoleOnly()
	dir := t.TempDir()
	f := &fakeUpdateRuntime{snapshot: &updateSnapshot{Version: "1", YAML: []byte("old configuration with secret"), Images: map[string]string{"retained": "sha256:old"}, CreatedAt: time.Now().UTC()}}
	m := &UpdateManager{root: func() string { return dir }, runtime: f}
	app := &ComposeApp{Name: "test-updates", Services: types.Services{{Name: "web", Image: "test:2"}}, Extensions: types.Extensions{common.ComposeExtensionNameXCasaOS: map[string]interface{}{"main": "web"}}}
	return m, f, app
}

func TestUpdateTransactionAndRollback(t *testing.T) {
	m, f, app := updateFixture(t)
	r := &updateRecord{Status: AppUpdateStatus{ID: app.Name}, Previous: &updateSnapshot{Version: "0"}}
	if err := m.run(context.Background(), app, app, r, false); err != nil {
		t.Fatal(err)
	}
	if r.Previous != f.snapshot || r.Pending != nil || r.Status.Operation != "updated" {
		t.Fatalf("unexpected completed record: %+v", r)
	}
	if !reflect.DeepEqual(f.calls, []string{"capture", "pull", "apply", "release:0"}) {
		t.Fatal(f.calls)
	}
	f.calls = nil
	if err := m.run(context.Background(), app, nil, r, true); err != nil {
		t.Fatal(err)
	}
	if string(f.applied) != string(f.snapshot.YAML) {
		t.Fatal("rollback did not restore saved configuration")
	}
	if !reflect.DeepEqual(f.calls, []string{"available", "apply"}) {
		t.Fatalf("rollback must never pull: %v", f.calls)
	}
	if r.Status.Operation != "reverted" || r.Previous != nil || r.Active != f.snapshot {
		t.Fatal("rollback retention incorrect")
	}
}

func TestUpdateFailurePreservesRecovery(t *testing.T) {
	for _, stage := range []string{"capture", "pull", "apply"} {
		t.Run(stage, func(t *testing.T) {
			m, f, app := updateFixture(t)
			f.fail = stage
			old := &updateSnapshot{Version: "0"}
			r := &updateRecord{Status: AppUpdateStatus{ID: app.Name}, Previous: old}
			if err := m.run(context.Background(), app, app, r, false); err == nil {
				t.Fatal("expected failure")
			}
			if r.Previous != old {
				t.Fatal("failure discarded previous recovery point")
			}
			if stage == "apply" && r.Pending != f.snapshot {
				t.Fatal("startup failure lost immediate recovery point")
			}
			if stage == "pull" && r.Pending != nil {
				t.Fatal("pull failure should leave app unchanged")
			}
			if stage != "apply" && f.applied != nil {
				t.Fatal("containers were replaced before downloads completed")
			}
		})
	}
}

func TestRollbackMissingImageDoesNotApply(t *testing.T) {
	m, f, app := updateFixture(t)
	f.fail = "available"
	r := &updateRecord{Previous: f.snapshot}
	if err := m.run(context.Background(), app, nil, r, true); err == nil {
		t.Fatal("expected missing image error")
	}
	if f.applied != nil || r.Previous == nil {
		t.Fatal("missing image changed app or discarded snapshot")
	}
}

func TestUpdateStatusSurvivesRestartWithoutSecrets(t *testing.T) {
	m, f, app := updateFixture(t)
	r := &updateRecord{Status: AppUpdateStatus{ID: app.Name, Operation: "applying"}, Pending: f.snapshot}
	if err := m.save(app.Name, r); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	if status.Operation != "interrupted" || !status.RollbackAvailable {
		t.Fatalf("bad recovery status: %+v", status)
	}
	b, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "sha256:old") {
		t.Fatal("status exposed recovery secrets")
	}
	stat, err := os.Stat(m.recordPath(app.Name))
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0600 {
		t.Fatalf("insecure permissions: %v", stat.Mode())
	}
	if filepath.Dir(m.recordPath("../../elsewhere")) != m.root() {
		t.Fatal("app ID escaped state directory")
	}
}

func TestUpdateOperationsExcludeConcurrentMutations(t *testing.T) {
	unlock, err := LockAppOperation("exclusive-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockAppOperation("exclusive-test"); !errors.Is(err, ErrAppOperationBusy) {
		t.Fatal("duplicate operation allowed")
	}
	unlock()
	next, err := LockAppOperation("exclusive-test")
	if err != nil {
		t.Fatal(err)
	}
	next()
}

func TestStoreUpdatePreservesCustomConfiguration(t *testing.T) {
	_, _, app := updateFixture(t)
	value := "secret$literal"
	app.Services[0].Environment = types.MappingWithEquals{"PASSWORD": &value}
	app.Services[0].Ports = []types.ServicePortConfig{{Published: "18080", Target: 80}}
	app.Services[0].Volumes = []types.ServiceVolumeConfig{{Type: "bind", Source: "/custom/data", Target: "/data"}}
	app.Services = append(app.Services, types.ServiceConfig{Name: "db", Image: "db:1"})
	store := &ComposeApp{Services: types.Services{{Name: "web", Image: "test:3"}, {Name: "db", Image: "db:2"}}}
	target, err := mergeStoreImages(app, store)
	if err != nil {
		t.Fatal(err)
	}
	if target.Services[1].Image != "db:2" || target.Services[0].Image != "test:3" {
		t.Fatal("not every service updated")
	}
	if app.Services[0].Image != "test:2" {
		t.Fatal("mutated installed project before saving snapshot")
	}
	if !reflect.DeepEqual(app.Services[0].Ports, target.Services[0].Ports) || !reflect.DeepEqual(app.Services[0].Volumes, target.Services[0].Volumes) || *target.Services[0].Environment["PASSWORD"] != value {
		t.Fatal("user configuration lost")
	}
	data, err := resolvedComposeYAML(target)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := NewComposeAppFromYAML(data, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if *loaded.App("web").Environment["PASSWORD"] != value {
		t.Fatal("literal dollars changed during reload")
	}
	store.Services = store.Services[:1]
	if _, err := mergeStoreImages(app, store); !errors.Is(err, ErrComposeAppNotMatch) {
		t.Fatal("incompatible layout allowed")
	}
}

func TestRecoveryWriteFailurePreventsPullAndApply(t *testing.T) {
	m, f, app := updateFixture(t)
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	m.root = func() string { return path }
	if err := m.run(context.Background(), app, app, &updateRecord{}, false); err == nil {
		t.Fatal("expected persistence failure")
	}
	if !reflect.DeepEqual(f.calls, []string{"capture"}) {
		t.Fatalf("unsafe work after write failure: %v", f.calls)
	}
}

func TestUpdateChecksDistinguishErrorsFromCurrent(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		targetErr    error
		runtimeFail  string
	}{
		{"unmanaged", "unmanaged", ErrStoreInfoNotFound, ""},
		{"missing store", "unmanaged", ErrNotFoundInAppStore, ""},
		{"bad store", "failed", errors.New("store unavailable"), ""},
		{"registry failure", "failed", nil, "check"},
		{"available", "available", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f, app := updateFixture(t)
			m.target = func(*ComposeApp) (*ComposeApp, error) { return app, tc.targetErr }
			f.fail = tc.runtimeFail
			if err := m.Check(context.Background(), map[string]*ComposeApp{app.Name: app}); err != nil {
				t.Fatal(err)
			}
			r, err := m.read(app.Name)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status.CheckStatus != tc.status || r.Status.CheckedAt == nil {
				t.Fatalf("incorrect check result: %+v", r.Status)
			}
			if tc.status == "failed" && r.Status.CheckError == "" {
				t.Fatal("check failure was hidden")
			}
		})
	}
}

func TestSavedInstallationSurvivesMissingContainers(t *testing.T) {
	m, _, app := updateFixture(t)
	path := filepath.Join(t.TempDir(), "compose.yaml")
	data, err := resolvedComposeYAML(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.save(app.Name, &updateRecord{ConfigFile: path, Status: AppUpdateStatus{ID: app.Name, Operation: "applying"}}); err != nil {
		t.Fatal(err)
	}
	apps := map[string]*ComposeApp{}
	if err := m.RecoverApps(apps); err != nil {
		t.Fatal(err)
	}
	if apps[app.Name] == nil {
		t.Fatal("failed installation disappeared from update list")
	}
}

func TestStoreDigestReferencesAndMutableTags(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	compare := func(image string, digests []string) (bool, error) { return false, nil }
	match, err := matchesStoreImage("example/app@"+digest, []string{"example/app@" + digest}, compare)
	if err != nil || !match {
		t.Fatalf("matching immutable image: %v %v", match, err)
	}
	match, err = matchesStoreImage("example/app:latest", []string{"example/app@" + digest}, compare)
	if err != nil || match {
		t.Fatalf("changed mutable image: %v %v", match, err)
	}
	_, err = matchesStoreImage("example/app:2", nil, func(string, []string) (bool, error) { return false, errors.New("registry down") })
	if err == nil {
		t.Fatal("registry failure hidden")
	}
}
