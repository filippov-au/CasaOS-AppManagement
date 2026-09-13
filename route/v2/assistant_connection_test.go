package v2

import (
	stdjson "encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IceWhaleTech/CasaOS-AppManagement/internal/assistant"
	"github.com/IceWhaleTech/CasaOS-AppManagement/service"
	"github.com/labstack/echo/v4"
)

type connectionTransport func(*http.Request) (*http.Response, error)

func (f connectionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestAssistantConnectionVerifiesBeforeSavingAndDoesNotExposeKey(t *testing.T) {
	store := service.GetAssistantSettings()
	oldRoot := store.Root
	store.Root = t.TempDir()
	defer func() { store.Root = oldRoot }()
	transport := http.DefaultTransport
	defer func() { http.DefaultTransport = transport }()
	status := 401
	calls := 0
	http.DefaultTransport = connectionTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		var input struct {
			Tools []assistant.Tool `json:"tools"`
		}
		if err := stdjson.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Tools) == 0 {
			t.Fatal("connection verification must validate the tool-enabled request")
		}
		if r.URL.String() != assistant.Presets[0].Endpoint || r.Header.Get("Authorization") != "Bearer submitted-key" {
			t.Fatal("incorrect verification routing")
		}
		body := `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`
		if status != 200 {
			body = "submitted-key"
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	for _, code := range []int{401, 200} {
		status = code
		req := httptest.NewRequest("PUT", "/assistant/settings", strings.NewReader(`{"provider":"deepseek","model":"deepseek-flash","api_key":"submitted-key","verify":true}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("user_id", "1")
		rec := httptest.NewRecorder()
		ctx := echo.New().NewContext(req, rec)
		if err := new(AppManagement).SaveAssistantSettings(ctx); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(rec.Body.String(), "submitted-key") {
			t.Fatal("verification leaked credentials")
		}
		stored, err := store.Read("1")
		if err != nil {
			t.Fatal(err)
		}
		if code == 401 && (rec.Code != 400 || stored.Configured) {
			t.Fatal("failed key was saved")
		}
		if code == 200 && (rec.Code != 200 || !stored.Configured) {
			t.Fatal("valid connection not saved")
		}
	}
	if calls != 2 {
		t.Fatal("verification not executed")
	}
}
