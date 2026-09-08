package service

import (
	"context"
	"errors"
	"strings"

	"github.com/IceWhaleTech/CasaOS-AppManagement/pkg/docker"
	"github.com/docker/distribution/reference"
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

func updateVersion(app *ComposeApp) string {
	service, err := app.MainService()
	if err != nil || service == nil {
		if len(app.Services) == 0 {
			return ""
		}
		service = (*App)(&app.Services[0])
	}
	named, err := reference.ParseNormalizedNamed(service.Image)
	if err != nil {
		return ""
	}
	if tag, ok := named.(reference.Tagged); ok {
		return tag.Tag()
	}
	if pin, ok := named.(reference.Canonical); ok {
		return pin.Digest().Encoded()[:12]
	}
	return "latest"
}

func resolveAppUpdateImages(ctx context.Context, app, target *ComposeApp) ([]docker.ImageUpdate, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, errors.New("could not connect to Docker")
	}
	defer cli.Close()
	images, err := currentImages(ctx, cli, app)
	if err != nil {
		return nil, errors.New("could not inspect every installed service; check the app and retry")
	}
	results := make([]docker.ImageUpdate, 0, len(target.Services))
	for i, service := range target.Services {
		old := app.App(service.Name)
		if old == nil || old.Build != nil || old.Image == "" {
			return results, errors.New("this app's service layout or local build requires a manual update")
		}
		installed, _, err := cli.ImageInspectWithRaw(ctx, images[service.Name])
		if err != nil {
			return results, errors.New("could not inspect an installed image")
		}
		result := docker.ResolveImageUpdate(ctx, old.Image, installed)
		result.Service, result.InstalledImage = service.Name, old.Image
		if result.Status == "up_to_date" && !sameImageReference(old.Image, result.LatestImage) {
			result.Status = "available"
		}
		results = append(results, result)
		if result.Error != "" || result.LatestImage == "" {
			continue
		}
		target.Services[i].Image = result.LatestImage
		target.Services[i].PullPolicy = "never"
		target.Services[i].Build = nil
	}
	return results, nil
}

func sameImageReference(a, b string) bool {
	left, err := reference.ParseNormalizedNamed(a)
	if err != nil {
		return false
	}
	right, err := reference.ParseNormalizedNamed(b)
	return err == nil && reference.TagNameOnly(left).String() == reference.TagNameOnly(right).String()
}

// The main service determines the app version; sidecar versions remain in Details.
func setCheckedVersions(app *ComposeApp, images []docker.ImageUpdate, status *AppUpdateStatus) {
	main, err := app.MainService()
	if err != nil || main == nil {
		if len(app.Services) == 0 {
			return
		}
		main = (*App)(&app.Services[0])
	}
	for _, image := range images {
		if image.Service == main.Name {
			if image.CurrentVersion != "" {
				status.CurrentVersion = image.CurrentVersion
			}
			status.TargetVersion = image.LatestVersion
			return
		}
	}
}
