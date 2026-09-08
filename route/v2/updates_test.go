package v2

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/IceWhaleTech/CasaOS-AppManagement/service"
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
