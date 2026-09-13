package v2

import (
	"context"
	"errors"
	"github.com/IceWhaleTech/CasaOS-AppManagement/internal/assistant"
	"github.com/IceWhaleTech/CasaOS-AppManagement/service"
	"github.com/labstack/echo/v4"
	"net/http"
	"time"
)

func assistantOwner(c echo.Context) string { return c.Request().Header.Get("user_id") }
func assistantReply(c echo.Context, data interface{}, err error) error {
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, assistant.ErrNotFound) {
			code = http.StatusNotFound
		}
		if errors.Is(err, assistant.ErrBusy) {
			code = http.StatusConflict
		}
		return c.JSON(code, map[string]string{"message": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"data": data})
}
func (a *AppManagement) GetAssistantSettings(c echo.Context) error {
	assistant.Catalog.Refresh(c.Request().Context())
	cfg, err := service.GetAssistantSettings().Read(assistantOwner(c))
	cfg.APIKey = ""
	connections, connectionErr := service.GetAssistantSettings().Connections(assistantOwner(c))
	if err == nil {
		err = connectionErr
	}
	return assistantReply(c, map[string]interface{}{"settings": cfg, "presets": assistant.Catalog.Current(), "connections": connections, "catalog_source": assistant.Catalog.Source()}, err)
}
func (a *AppManagement) SaveAssistantSettings(c echo.Context) error {
	var request struct {
		assistant.Settings
		Verify bool `json:"verify"`
	}
	if err := c.Bind(&request); err != nil {
		return echo.ErrBadRequest
	}
	cfg := request.Settings
	if request.Verify {
		if cfg.APIKey == "" {
			existing, err := service.GetAssistantSettings().ReadProvider(assistantOwner(c), cfg.Provider)
			if err != nil {
				return assistantReply(c, nil, err)
			}
			cfg.APIKey = existing.APIKey
		}
		ctx, cancel := context.WithTimeout(c.Request().Context(), 30*time.Second)
		defer cancel()
		if _, err := (assistant.ChatModel{}).Complete(ctx, cfg, "casaos-connect-"+assistant.ID(), []assistant.Message{{Role: "user", Content: "Reply with OK. Do not call any tools."}}, service.Assistant.Runtime.Tools()); err != nil {
			return assistantReply(c, nil, err)
		}
	}
	saved, err := service.GetAssistantSettings().Save(assistantOwner(c), cfg)
	return assistantReply(c, saved, err)
}
func (a *AppManagement) DeleteAssistantSettings(c echo.Context) error {
	return assistantReply(c, map[string]bool{"deleted": true}, service.GetAssistantSettings().Delete(assistantOwner(c)))
}
func (a *AppManagement) StartAssistantSession(c echo.Context) error {
	var req struct {
		Message string `json:"message"`
		Mode    string `json:"mode"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.ErrBadRequest
	}
	cfg, err := service.GetAssistantSettings().Read(assistantOwner(c))
	if err != nil {
		return assistantReply(c, nil, err)
	}
	view, err := service.Assistant.Start(assistantOwner(c), cfg, req.Message, req.Mode)
	return assistantReply(c, view, err)
}
func (a *AppManagement) GetAssistantSession(c echo.Context, id string) error {
	view, err := service.Assistant.Get(assistantOwner(c), id)
	return assistantReply(c, view, err)
}
func (a *AppManagement) ReplyAssistantSession(c echo.Context, id string) error {
	var req struct {
		Message string `json:"message"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.ErrBadRequest
	}
	view, err := service.Assistant.Reply(assistantOwner(c), id, req.Message)
	return assistantReply(c, view, err)
}
func (a *AppManagement) DecideAssistantSession(c echo.Context, id string) error {
	var req struct {
		ApprovalID string `json:"approval_id"`
		Allow      bool   `json:"allow"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.ErrBadRequest
	}
	view, err := service.Assistant.Approve(assistantOwner(c), id, req.ApprovalID, req.Allow)
	return assistantReply(c, view, err)
}
func (a *AppManagement) CancelAssistantSession(c echo.Context, id string) error {
	view, err := service.Assistant.Cancel(assistantOwner(c), id)
	return assistantReply(c, view, err)
}
func (a *AppManagement) DeleteAssistantSession(c echo.Context, id string) error {
	return assistantReply(c, map[string]bool{"deleted": true}, service.Assistant.Delete(assistantOwner(c), id))
}

func (a *AppManagement) ListAssistantSessions(c echo.Context) error {
	return assistantReply(c, service.Assistant.List(assistantOwner(c)), nil)
}

func (a *AppManagement) AddAssistantCredential(c echo.Context, id string) error {
	var req struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.ErrBadRequest
	}
	view, err := service.Assistant.AddSecret(assistantOwner(c), id, req.Name, req.Value)
	return assistantReply(c, view, err)
}

func (a *AppManagement) DeleteAssistantConnection(c echo.Context, provider string) error {
	return assistantReply(c, map[string]bool{"deleted": true}, service.GetAssistantSettings().Disconnect(assistantOwner(c), provider))
}
