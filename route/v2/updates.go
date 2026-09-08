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
	return ctx.JSON(http.StatusOK, map[string]interface{}{"data": statuses, "registry_supported": true, "combined_updates_supported": true})
}
func (a *AppManagement) CheckAppUpdates(ctx echo.Context, params codegen.CheckAppUpdatesParams) error {
	if params.Source != nil && *params.Source != "registry" && *params.Source != "store" {
		return echo.NewHTTPError(http.StatusBadRequest, "source must be registry or store")
	}
	apps, err := service.MyService.Compose().List(ctx.Request().Context())
	if err != nil {
		return updateError(ctx, err)
	}
	if err := service.Updates.RecoverApps(apps); err != nil {
		return updateError(ctx, err)
	}
	check := service.Updates.CheckCombined
	if params.Source != nil {
		check = service.Updates.Check
		if *params.Source == "registry" {
			check = service.Updates.CheckRegistry
		}
	}
	if err := check(ctx.Request().Context(), apps); err != nil {
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
	if errors.Is(err, service.ErrAppOperationBusy) || errors.Is(err, service.ErrUpdatePlanStale) {
		status = http.StatusConflict
	}
	return ctx.JSON(status, map[string]string{"message": err.Error()})
}
