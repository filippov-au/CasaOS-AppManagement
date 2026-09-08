package service

import (
	"context"
	"errors"
	"testing"

	"github.com/IceWhaleTech/CasaOS-AppManagement/pkg/docker"
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
