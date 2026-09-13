package v2

import (
	"context"
	"github.com/IceWhaleTech/CasaOS-AppManagement/internal/assistant"
	"github.com/IceWhaleTech/CasaOS-AppManagement/service"
	"github.com/labstack/echo/v4"
	"net/http"
	"time"
)

type npmConnectionRequest struct {
	App          string `json:"app"`
	Service      string `json:"service"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	Suffix       string `json:"suffix"`
	AccessListID *int   `json:"access_list_id"`
}

func (r npmConnectionRequest) connection() assistant.NPMConnection {
	id := -1
	if r.AccessListID != nil {
		id = *r.AccessListID
	}
	return assistant.NPMConnection{App: r.App, Service: r.Service, Username: r.Username, Password: r.Password, Suffix: r.Suffix, AccessListID: id}
}
func (*AppManagement) DiscoverAssistantNPM(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), 20*time.Second)
	defer cancel()
	v, e := service.AssistantNPMDiscover(ctx)
	return assistantReply(c, v, e)
}
func (*AppManagement) GetAssistantNPM(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), time.Minute)
	defer cancel()
	v, e := service.AssistantNPMInspect(ctx, assistantOwner(c))
	if e != nil && v.Connection != nil {
		v.Error = e.Error()
		e = nil
	}
	return assistantReply(c, v, e)
}
func (*AppManagement) VerifyAssistantNPM(c echo.Context) error {
	var r npmConnectionRequest
	if c.Bind(&r) != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid NPM connection fields")
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), time.Minute)
	defer cancel()
	v, e := service.AssistantNPMVerify(ctx, assistantOwner(c), r.connection())
	return assistantReply(c, v, e)
}
func (*AppManagement) SaveAssistantNPM(c echo.Context) error {
	var r npmConnectionRequest
	if c.Bind(&r) != nil || r.AccessListID == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "Choose the NPM connection and access policy")
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), time.Minute)
	defer cancel()
	e := service.AssistantNPMSave(ctx, assistantOwner(c), r.connection())
	return assistantReply(c, map[string]bool{"saved": e == nil}, e)
}
func (*AppManagement) DeleteAssistantNPM(c echo.Context) error {
	return assistantReply(c, map[string]bool{"disconnected": true}, service.AssistantNPMDisconnect(assistantOwner(c)))
}
