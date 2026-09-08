package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/IceWhaleTech/CasaOS-AppManagement/common"
	"github.com/IceWhaleTech/CasaOS-AppManagement/pkg/docker"
)

var ErrUpdatePlanStale = errors.New("app settings or the available update changed; check for updates again")

// Plans stay in the private recovery record, never in API responses. The button
// installs the checked definition and verifies downloaded images before applying.
type appUpdatePlan struct {
	Token      string            `json:"token"`
	YAML       []byte            `json:"yaml"`
	ConfigHash string            `json:"config_hash"`
	ImageIDs   map[string]string `json:"image_ids"`
	CreatedAt  time.Time         `json:"created_at"`
}

func appConfigHash(app *ComposeApp) (string, error) {
	data, err := resolvedComposeYAML(app)
	return fmt.Sprintf("%x", sha256.Sum256(data)), err
}

func loadCheckedTarget(app *ComposeApp, record *updateRecord) (*ComposeApp, error) {
	plan := record.Plan
	if plan == nil || record.Status.CheckStatus != "available" || time.Since(plan.CreatedAt) > 24*time.Hour {
		return nil, ErrUpdatePlanStale
	}
	hash, err := appConfigHash(app)
	if err != nil {
		return nil, err
	}
	if hash != plan.ConfigHash {
		return nil, ErrUpdatePlanStale
	}
	target, err := NewComposeAppFromYAML(plan.YAML, false, false)
	if err != nil {
		return nil, err
	}
	if len(target.Services) != len(app.Services) || len(plan.ImageIDs) != len(target.Services) {
		return nil, ErrUpdatePlanStale
	}
	for _, s := range target.Services {
		if app.App(s.Name) == nil || plan.ImageIDs[s.Name] == "" {
			return nil, ErrUpdatePlanStale
		}
	}
	return target, nil
}

func definitionChanged(app, target *ComposeApp) (bool, error) {
	current, _ := cloneCompose(app)
	next, _ := cloneCompose(target)
	for _, project := range []*ComposeApp{current, next} {
		for i := range project.Services {
			project.Services[i].Image = ""
			project.Services[i].PullPolicy = ""
		}
	}
	a, err := resolvedComposeYAML(current)
	if err != nil {
		return false, err
	}
	b, err := resolvedComposeYAML(next)
	return !bytes.Equal(a, b), err
}

// The default check combines refreshed catalog defaults with newer registry
// images. Explicit legacy store/registry modes remain available to old clients.
func (m *UpdateManager) CheckCombined(ctx context.Context, apps map[string]*ComposeApp) error {
	if !m.checkMu.TryLock() {
		return ErrAppOperationBusy
	}
	defer m.checkMu.Unlock()
	sourceErrors := refreshUpdateCatalogs()
	for _, app := range apps {
		if err := ctx.Err(); err != nil {
			return err
		}
		unlock, err := LockAppOperation(app.Name)
		if err != nil {
			continue
		}
		err = func() error {
			defer unlock()
			// Match Start's resolved configuration, including edits made outside CasaOS.
			var checkErr error
			if len(app.ComposeFiles) != 1 {
				checkErr = errors.New("updates require one saved Compose configuration; review this app's settings")
			} else if loaded, err := LoadComposeAppFromConfigFile(app.Name, app.ComposeFiles[0]); err != nil {
				checkErr = errors.New("could not read this app's configuration; review its settings and retry")
			} else {
				app = loaded
			}
			m.mu.Lock()
			r, err := m.read(app.Name)
			m.mu.Unlock()
			if err != nil {
				return err
			}
			r.Plan, r.Combined = nil, true
			now := time.Now().UTC()
			s := &r.Status
			s.CheckedAt, s.RegistryCheckedAt = &now, &now
			s.RegistryImages = nil
			s.TargetVersion, s.CheckError = "", ""
			s.CheckStatus = "failed"
			s.CurrentVersion = updateVersion(app)
			var target *ComposeApp
			if checkErr == nil {
				target, checkErr = m.target(app)
			}
			if errors.Is(checkErr, ErrStoreInfoNotFound) {
				target, checkErr = cloneCompose(app) // custom Compose apps still receive image updates
			}
			if checkErr == nil {
				if ext, ok := target.Extensions[common.ComposeExtensionNameXCasaOS].(map[string]interface{}); ok {
					if source, _ := ext["store_url"].(string); sourceErrors[source] != nil {
						checkErr = errors.New("this app's store could not be refreshed; retry the check")
					}
				}
			}
			if checkErr == nil {
				var images []docker.ImageUpdate
				images, checkErr = m.resolve(ctx, app, target)
				s.RegistryImages = images
				if checkErr == nil {
					checkErr = prepareCheckedUpdate(app, target, images, r, now)
				}
			}
			if checkErr != nil {
				s.CheckStatus, s.CheckError = "failed", checkErr.Error()
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

func prepareCheckedUpdate(app, target *ComposeApp, images []docker.ImageUpdate, r *updateRecord, now time.Time) error {
	available, err := definitionChanged(app, target)
	if err != nil {
		return err
	}
	ids := map[string]string{}
	for _, image := range images {
		if image.Error != "" || image.LatestImageID == "" || (image.Status != "available" && image.Status != "up_to_date") {
			return fmt.Errorf("could not prepare the update for %s; check its image details and retry", image.Service)
		}
		ids[image.Service] = image.LatestImageID
		available = available || image.Status == "available"
	}
	if len(ids) == 0 || len(ids) != len(target.Services) {
		return errors.New("could not check every app service")
	}
	r.Status.TargetVersion = updateVersion(target)
	r.Status.CheckStatus = "up_to_date"
	if !available {
		return nil
	}
	data, err := resolvedComposeYAML(target)
	if err != nil {
		return err
	}
	hash, err := appConfigHash(app)
	if err != nil {
		return err
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	r.Plan = &appUpdatePlan{Token: hex.EncodeToString(token[:]), YAML: data, ConfigHash: hash, ImageIDs: ids, CreatedAt: now}
	r.Status.CheckStatus = "available"
	return nil
}
