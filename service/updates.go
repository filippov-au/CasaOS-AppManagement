package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/IceWhaleTech/CasaOS-AppManagement/common"
	"github.com/IceWhaleTech/CasaOS-AppManagement/pkg/config"
)

var ErrAppOperationBusy = errors.New("another operation is already running for this app")
var appOperations sync.Map

// LockAppOperation is shared by updates, settings, lifecycle operations and uninstall.
func LockAppOperation(id string) (func(), error) {
	if _, loaded := appOperations.LoadOrStore(id, true); loaded {
		return nil, ErrAppOperationBusy
	}
	return func() { appOperations.Delete(id) }, nil
}
func appOperationBusy(id string) bool { _, ok := appOperations.Load(id); return ok }

type AppUpdateStatus struct {
	ID                string            `json:"id"`
	Title             map[string]string `json:"title"`
	Icon              string            `json:"icon"`
	CurrentVersion    string            `json:"current_version"`
	TargetVersion     string            `json:"target_version,omitempty"`
	CheckStatus       string            `json:"check_status"`
	CheckedAt         *time.Time        `json:"checked_at,omitempty"`
	CheckError        string            `json:"check_error,omitempty"`
	Operation         string            `json:"operation"`
	Error             string            `json:"error,omitempty"`
	RollbackAvailable bool              `json:"rollback_available"`
	RollbackVersion   string            `json:"rollback_version,omitempty"`
	RollbackDate      *time.Time        `json:"rollback_date,omitempty"`
	RollbackReason    string            `json:"rollback_reason,omitempty"`
}

type updateSnapshot struct {
	YAML      []byte            `json:"yaml"`
	Images    map[string]string `json:"images"` // retained reference -> immutable image ID
	Version   string            `json:"version"`
	CreatedAt time.Time         `json:"created_at"`
}

type updateRecord struct {
	ConfigFile string          `json:"config_file,omitempty"`
	Active     *updateSnapshot `json:"active,omitempty"` // restored images referenced by the current Compose file
	Status     AppUpdateStatus `json:"status"`
	Previous   *updateSnapshot `json:"previous,omitempty"`
	Pending    *updateSnapshot `json:"pending,omitempty"`
}

type updateRuntime interface {
	Capture(context.Context, *ComposeApp) (*updateSnapshot, error)
	Pull(context.Context, *ComposeApp) error
	Apply(context.Context, *ComposeApp, []byte) error
	Available(context.Context, *updateSnapshot) error
	Release(context.Context, *updateSnapshot)
	Check(context.Context, *ComposeApp, *ComposeApp) (bool, error)
}

type UpdateManager struct {
	mu      sync.Mutex
	checkMu sync.Mutex
	root    func() string
	runtime updateRuntime
	target  func(*ComposeApp) (*ComposeApp, error)
}

var Updates = &UpdateManager{
	root:    func() string { return filepath.Join(filepath.Dir(config.AppInfo.AppsPath), "app-updates") },
	runtime: dockerUpdateRuntime{},
	target:  storeUpdateTarget,
}

func (m *UpdateManager) recordPath(id string) string {
	return filepath.Join(m.root(), fmt.Sprintf("%x.json", sha256.Sum256([]byte(id))))
}

// Atomic writes protect both recovery information and status from partial writes.
func atomicPrivateWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".update-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (m *UpdateManager) read(id string) (*updateRecord, error) {
	b, err := os.ReadFile(m.recordPath(id))
	if os.IsNotExist(err) {
		return &updateRecord{Status: AppUpdateStatus{ID: id, Operation: "idle", CheckStatus: "unchecked"}}, nil
	}
	if err != nil {
		return nil, err
	}
	var r updateRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
func (m *UpdateManager) save(id string, r *updateRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return atomicPrivateWrite(m.recordPath(id), b)
}
func runningOperation(op string) bool {
	return op == "preparing" || op == "pulling" || op == "applying" || op == "reverting"
}
func recoverySnapshot(r *updateRecord) *updateSnapshot {
	if r.Pending != nil {
		return r.Pending
	}
	return r.Previous
}

func (m *UpdateManager) Status(ctx context.Context, app *ComposeApp) (AppUpdateStatus, error) {
	m.mu.Lock()
	r, err := m.read(app.Name)
	if err == nil && runningOperation(r.Status.Operation) && !appOperationBusy(app.Name) {
		r.Status.Operation = "interrupted"
		r.Status.Error = "The service restarted during this operation. Check the app before retrying or reverting."
		err = m.save(app.Name, r)
	}
	m.mu.Unlock()
	if err != nil {
		return AppUpdateStatus{}, err
	}
	s := r.Status
	s.Title = map[string]string{"en_us": app.Name}
	if info, err := app.StoreInfo(false); err == nil && info != nil {
		if info.Title != nil {
			s.Title = info.Title
		}
		s.Icon = info.Icon
	}
	if s.CurrentVersion == "" {
		s.CurrentVersion, _ = app.MainTag()
	}
	if snapshot := recoverySnapshot(r); snapshot != nil {
		s.RollbackVersion = snapshot.Version
		s.RollbackDate = &snapshot.CreatedAt
		if err := m.runtime.Available(ctx, snapshot); err != nil {
			s.RollbackReason = "Previous images are unavailable. They may have been removed outside CasaOS."
		} else {
			s.RollbackAvailable = true
		}
	}
	if appOperationBusy(app.Name) && !runningOperation(s.Operation) {
		s.Operation = "busy"
	}
	return s, nil
}

func (m *UpdateManager) List(ctx context.Context, apps map[string]*ComposeApp) ([]AppUpdateStatus, error) {
	result := make([]AppUpdateStatus, 0, len(apps))
	for _, app := range apps {
		s, err := m.Status(ctx, app)
		if err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	sort.Slice(result, func(i, j int) bool {
		if (result[i].CheckStatus == "available") != (result[j].CheckStatus == "available") {
			return result[i].CheckStatus == "available"
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}

// Checks run separately from installation and never pull or replace an image.
func (m *UpdateManager) Check(ctx context.Context, apps map[string]*ComposeApp) error {
	if !m.checkMu.TryLock() {
		return ErrAppOperationBusy
	}
	defer m.checkMu.Unlock()
	// Refresh each configured source and retain its failure instead of reporting stale data as current.
	sourceErrors := map[string]error{}
	for _, source := range config.ServerInfo.AppStoreList {
		store, err := AppStoreByURL(source)
		if err == nil {
			if source, ok := store.(*appStore); ok {
				err = source.refreshCatalog(true)
			} else {
				err = store.UpdateCatalog()
			}
		}
		if err != nil {
			sourceErrors[source] = err
		}
	}
	for _, app := range apps {
		unlock, err := LockAppOperation(app.Name)
		if err != nil {
			continue
		}
		err = func() error {
			defer unlock()
			m.mu.Lock()
			r, err := m.read(app.Name)
			m.mu.Unlock()
			if err != nil {
				return err
			}
			target, checkErr := m.target(app)
			s := &r.Status
			now := time.Now().UTC()
			s.CheckedAt = &now
			s.TargetVersion = ""
			s.CheckError = ""
			s.CheckStatus = "up_to_date"
			if errors.Is(checkErr, ErrStoreInfoNotFound) || errors.Is(checkErr, ErrNotFoundInAppStore) {
				s.CheckStatus = "unmanaged"
			} else {
				if checkErr == nil {
					if ext, ok := target.Extensions[common.ComposeExtensionNameXCasaOS].(map[string]interface{}); ok {
						if source, ok := ext["store_url"].(string); ok && sourceErrors[source] != nil {
							checkErr = errors.New("this app's store could not be refreshed; retry the check")
						}
					}
				}
				if checkErr == nil {
					s.TargetVersion, _ = target.MainTag()
					var available bool
					available, checkErr = m.runtime.Check(ctx, app, target)
					if available {
						s.CheckStatus = "available"
					}
				}
				if checkErr != nil {
					s.CheckStatus = "failed"
					s.CheckError = checkErr.Error()
				}
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			return m.save(app.Name, r)
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

// Start reserves the app synchronously so repeated requests cannot launch competing jobs.
func (m *UpdateManager) Start(ctx context.Context, app *ComposeApp, rollback bool) error {
	unlock, err := LockAppOperation(app.Name)
	if err != nil {
		return err
	}
	started := false
	defer func() {
		if !started {
			unlock()
		}
	}()
	if len(app.ComposeFiles) != 1 {
		return errors.New("updates require exactly one Compose configuration file")
	}
	// Re-read after taking the lock so a settings operation cannot leave us a stale project.
	app, err = LoadComposeAppFromConfigFile(app.Name, app.ComposeFiles[0])
	if err != nil {
		return err
	}
	var target *ComposeApp
	if !rollback {
		target, err = m.target(app)
		if err != nil {
			return err
		}
	}
	m.mu.Lock()
	r, err := m.read(app.Name)
	if err == nil {
		if rollback && recoverySnapshot(r) == nil {
			err = errors.New("no previous version has been saved")
		} else if !rollback && r.Pending != nil {
			err = errors.New("revert the interrupted or failed update before starting another update")
		}
	}
	if err == nil {
		r.ConfigFile = app.ComposeFiles[0]
		r.Status.Operation = "preparing"
		if rollback {
			r.Status.Operation = "reverting"
		}
		r.Status.Error = ""
		err = m.save(app.Name, r)
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	properties := common.PropertiesFromContext(ctx)
	properties[common.PropertyTypeAppName.Name] = app.Name
	_ = app.UpdateEventPropertiesFromStoreInfo(properties)
	started = true
	go func() {
		defer unlock()
		jobCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		MyService.AppStoreManagement().StartUpgrade(app.Name)
		defer MyService.AppStoreManagement().FinishUpgrade(app.Name)
		PublishEventWrapper(jobCtx, common.EventTypeAppUpdateBegin, nil)
		defer PublishEventWrapper(jobCtx, common.EventTypeAppUpdateEnd, nil)
		err := m.run(jobCtx, app, target, r, rollback)
		if err != nil {
			PublishEventWrapper(jobCtx, common.EventTypeAppUpdateError, map[string]string{common.PropertyTypeMessage.Name: err.Error()})
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if err != nil {
			r.Status.Operation = "failed"
			r.Status.Error = err.Error()
		}
		if saveErr := m.save(app.Name, r); saveErr != nil {
			// The last durable stage will be reported as interrupted after the lock is released.
			fmt.Fprintf(os.Stderr, "cannot persist app update result for %s: %v\n", app.Name, saveErr)
		}
	}()
	return nil
}

func (m *UpdateManager) stage(id string, r *updateRecord, stage string) error {
	r.Status.Operation = stage
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.save(id, r)
}

func (m *UpdateManager) run(ctx context.Context, app, target *ComposeApp, r *updateRecord, rollback bool) error {
	if rollback {
		snapshot := recoverySnapshot(r)
		if err := m.runtime.Available(ctx, snapshot); err != nil {
			return fmt.Errorf("previous images are unavailable: %w", err)
		}
		if err := m.runtime.Apply(ctx, app, snapshot.YAML); err != nil {
			return err
		}
		r.Status.CurrentVersion = snapshot.Version
		// A successful revert consumes its recovery point; failed reverts remain retryable.
		old := r.Previous
		active := r.Active
		r.Active = snapshot
		r.Previous = nil
		r.Pending = nil
		r.Status.Operation = "reverted"
		r.Status.CheckStatus = "unchecked"
		if err := m.stage(app.Name, r, "reverted"); err != nil {
			return err
		}
		// Keep the restored images referenced by the active Compose file. Only obsolete history is released.
		if old != snapshot {
			m.runtime.Release(ctx, old)
		}
		if active != snapshot && active != old {
			m.runtime.Release(ctx, active)
		}
		return nil
	}
	snapshot, err := m.runtime.Capture(ctx, app)
	if err != nil {
		return err
	}
	if r.Status.CurrentVersion != "" {
		snapshot.Version = r.Status.CurrentVersion
	}
	r.Pending = snapshot
	if err := m.stage(app.Name, r, "pulling"); err != nil {
		return err
	}
	if err := m.runtime.Pull(ctx, target); err != nil {
		r.Pending = nil
		if saveErr := m.stage(app.Name, r, "failed"); saveErr != nil {
			r.Pending = snapshot
			return saveErr
		}
		m.runtime.Release(ctx, snapshot)
		return err
	}
	data, err := resolvedComposeYAML(target)
	if err != nil {
		return err
	}
	if err := m.stage(app.Name, r, "applying"); err != nil {
		return err
	}
	if err := m.runtime.Apply(ctx, app, data); err != nil {
		return err
	}
	old := r.Previous
	r.Previous = snapshot
	r.Pending = nil
	r.Status.CurrentVersion, _ = target.MainTag()
	r.Status.CheckStatus = "unchecked"
	if err := m.stage(app.Name, r, "updated"); err != nil {
		return err
	}
	m.runtime.Release(ctx, old)
	m.runtime.Release(ctx, r.Active)
	r.Active = nil
	return m.stage(app.Name, r, "updated")
}

// Forget is called only after a successful uninstall; it never touches app data.
func (m *UpdateManager) Forget(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.read(id)
	if err != nil {
		return err
	}
	m.runtime.Release(ctx, r.Previous)
	m.runtime.Release(ctx, r.Pending)
	m.runtime.Release(ctx, r.Active)
	err = os.Remove(m.recordPath(id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (m *UpdateManager) SettingsChanged(app *ComposeApp) error {
	updated, err := LoadComposeAppFromConfigFile(app.Name, app.ComposeFiles[0])
	if err != nil {
		return err
	}
	app = updated
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.read(app.Name)
	if err != nil {
		return err
	}
	r.Status.CurrentVersion, _ = app.MainTag()
	r.Status.CheckStatus = "unchecked"
	return m.save(app.Name, r)
}

// Include saved installations even if a failed recreate left no containers for Compose.List.
func (m *UpdateManager) RecoverApps(apps map[string]*ComposeApp) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, err := os.ReadDir(m.root())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.root(), entry.Name()))
		if err != nil {
			return err
		}
		var r updateRecord
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		if r.ConfigFile == "" || apps[r.Status.ID] != nil {
			continue
		}
		if _, err := os.Stat(r.ConfigFile); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		app, err := LoadComposeAppFromConfigFile(r.Status.ID, r.ConfigFile)
		if err != nil {
			return err
		}
		apps[r.Status.ID] = app
	}
	return nil
}
