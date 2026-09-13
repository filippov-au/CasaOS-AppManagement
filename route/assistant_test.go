package route

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestAssistantValidationErrorsDoNotExposeSubmittedValues(t *testing.T) {
	e := echo.New()
	c := e.NewContext(httptest.NewRequest("POST", V2APIPath+"/assistant/sessions/fixture/credentials", nil), httptest.NewRecorder())
	original := echo.NewHTTPError(400, "invalid private-credential-value").SetInternal(errors.New("schema input: private-credential-value"))
	clean := assistantValidationError(c, original).(*echo.HTTPError)
	if clean.Code != 400 || clean.Internal != nil || strings.Contains(clean.Error(), "private-credential-value") {
		t.Fatal("validation error can leak a submitted credential", clean)
	}
}

func TestAssistantRequiresJWTOnLoopbackAndIgnoresForgedUserHeader(t *testing.T) {
	handler := InitV2Router()
	for _, ip := range []string{"127.0.0.1:1234", "[::1]:1234", "192.0.2.1:1234"} {
		req := httptest.NewRequest(http.MethodGet, V2APIPath+"/assistant/settings", nil)
		req.RemoteAddr = ip
		req.Header.Set("user_id", "1")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusBadRequest && recorder.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s got %d %s", ip, recorder.Code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), "presets") {
			t.Fatal("settings exposed without a JWT")
		}
	}
}
