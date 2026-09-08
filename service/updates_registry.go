package service

import (
	"context"
	"strings"

	"github.com/IceWhaleTech/CasaOS-AppManagement/pkg/docker"
	"github.com/docker/docker/client"
)

func checkAppRegistryImages(ctx context.Context, app *ComposeApp) []docker.ImageUpdate {
	results := make([]docker.ImageUpdate, 0, len(app.Services))
	cli, clientErr := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if clientErr == nil {
		defer cli.Close()
	}
	var images map[string]string
	if clientErr == nil {
		images, clientErr = currentImages(ctx, cli, app)
	}
	for _, service := range app.Services {
		result := docker.ImageUpdate{Service: service.Name, Image: service.Image, Status: "failed"}
		switch {
		case service.Image == "" || service.Build != nil || strings.HasPrefix(service.Image, "sha256:") || strings.HasPrefix(service.Image, "casaos-rollback/"):
			result.Status = "unsupported"
			result.Error = "This service uses a local or restored image. Select a registry image in app settings to check versions."
		case clientErr != nil:
			result.Error = "Could not inspect the installed containers. Check Docker and retry."
		default:
			installed, _, err := cli.ImageInspectWithRaw(ctx, images[service.Name])
			if err != nil {
				result.Error = "Could not inspect the installed image. Check Docker and retry."
			} else {
				result = docker.CheckImageUpdate(ctx, service.Image, installed)
				result.Service = service.Name
			}
		}
		results = append(results, result)
	}
	return results
}
