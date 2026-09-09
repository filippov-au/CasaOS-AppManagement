package v2

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IceWhaleTech/CasaOS-AppManagement/codegen"
	"github.com/IceWhaleTech/CasaOS-AppManagement/pkg/config"
	"github.com/IceWhaleTech/CasaOS-AppManagement/service"
	"github.com/IceWhaleTech/CasaOS-Common/utils/logger"
	"github.com/labstack/echo/v4"
)

func TestStaleUpdateConfirmationReturnsConflict(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx := echo.New().NewContext(httptest.NewRequest(http.MethodPatch, "/compose/demo", nil), recorder)
	if err := updateError(ctx, service.ErrUpdatePlanStale); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusConflict {
		t.Fatalf("stale confirmation returned %d", recorder.Code)
	}
}

type updateRouteServices struct {
	service.Services
	store      *service.AppStoreManagement
	storeCalls int
}

func (s *updateRouteServices) AppStoreManagement() *service.AppStoreManagement {
	s.storeCalls++
	return s.store
}

func TestUpdateRouteUsesRegistryPlanBeforeLegacyStoreAvailability(t *testing.T) {
	logger.LogInitConsoleOnly()
	force := false
	token := "reviewed-plex-plan"
	for _, tc := range []struct {
		name, record string
		token        *string
		wantStatus   int
		storeCalls   int
	}{
		{"legacy client with saved registry check", `{"combined":true,"status":{"check_status":"available"}}`, nil, http.StatusConflict, 0},
		{"failed registry check", `{"combined":true,"status":{"check_status":"failed"}}`, nil, http.StatusConflict, 0},
		{"reviewed token without saved plan", "", &token, http.StatusConflict, 0},
		{"legacy store client", "", nil, http.StatusOK, 1},
		{"unreadable check record", `{`, nil, http.StatusInternalServerError, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			previousPath, previousService := config.AppInfo.AppsPath, service.MyService
			config.AppInfo.AppsPath = filepath.Join(root, "apps")
			services := &updateRouteServices{store: service.NewAppStoreManagement()}
			service.MyService = services
			t.Cleanup(func() { config.AppInfo.AppsPath, service.MyService = previousPath, previousService })
			path := filepath.Join(root, "compose.yaml")
			if err := os.WriteFile(path, []byte("name: plex\nservices:\n  plex:\n    image: lscr.io/linuxserver/plex:1.43.2\n"), 0600); err != nil {
				t.Fatal(err)
			}
			app, err := service.LoadComposeAppFromConfigFile("plex", path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.record != "" {
				dir := filepath.Join(root, "app-updates")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, fmt.Sprintf("%x.json", sha256.Sum256([]byte("plex"))))
				if err := os.WriteFile(path, []byte(tc.record), 0600); err != nil {
					t.Fatal(err)
				}
			}
			recorder := httptest.NewRecorder()
			ctx := echo.New().NewContext(httptest.NewRequest(http.MethodPatch, "/compose/plex?force=false", nil), recorder)
			if err := updateComposeApp(ctx, app, codegen.UpdateComposeAppParams{Force: &force, UpdateToken: tc.token}); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != tc.wantStatus || services.storeCalls != tc.storeCalls {
				t.Fatalf("status=%d store calls=%d body=%s", recorder.Code, services.storeCalls, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), "is up to date") != (tc.wantStatus == http.StatusOK) {
				t.Fatalf("registry state was reported as a store result: %s", recorder.Body.String())
			}
		})
	}
}
