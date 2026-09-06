package v2

import (
	"context"
	"errors"
	"net/http"

	"github.com/IceWhaleTech/CasaOS-AppManagement/codegen"
	"github.com/IceWhaleTech/CasaOS-AppManagement/common"
	"github.com/IceWhaleTech/CasaOS-AppManagement/service"
	"github.com/labstack/echo/v4"
)

func (a *AppManagement) AppUpdates(ctx echo.Context) error {
	apps, err := service.MyService.Compose().List(ctx.Request().Context())
	if err != nil {
		return updateError(ctx, err)
	}
	if err := service.Updates.RecoverApps(apps); err != nil {
		return updateError(ctx, err)
	}
	statuses, err := service.Updates.List(ctx.Request().Context(), apps)
	if err != nil {
		return updateError(ctx, err)
	}
	return ctx.JSON(http.StatusOK, map[string]interface{}{"data": statuses})
}
func (a *AppManagement) CheckAppUpdates(ctx echo.Context) error {
	apps, err := service.MyService.Compose().List(ctx.Request().Context())
	if err != nil {
		return updateError(ctx, err)
	}
	if err := service.Updates.RecoverApps(apps); err != nil {
		return updateError(ctx, err)
	}
	if err := service.Updates.Check(ctx.Request().Context(), apps); err != nil {
		return updateError(ctx, err)
	}
	return a.AppUpdates(ctx)
}
func (a *AppManagement) RollbackComposeApp(ctx echo.Context, id codegen.ComposeAppID) error {
	apps, err := service.MyService.Compose().List(ctx.Request().Context())
	if err != nil {
		return updateError(ctx, err)
	}
	if err := service.Updates.RecoverApps(apps); err != nil {
		return updateError(ctx, err)
	}
	app, ok := apps[id]
	if !ok {
		return echo.ErrNotFound
	}
	background := common.WithProperties(context.Background(), PropertiesFromQueryParams(ctx))
	if err := service.Updates.Start(background, app, true); err != nil {
		return updateError(ctx, err)
	}
	return ctx.JSON(http.StatusAccepted, map[string]string{"message": "Reverting to the previous version"})
}
func updateError(ctx echo.Context, err error) error {
	status := http.StatusInternalServerError
	if errors.Is(err, service.ErrAppOperationBusy) {
		status = http.StatusConflict
	}
	return ctx.JSON(status, map[string]string{"message": err.Error()})
}
