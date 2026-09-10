package service

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/IceWhaleTech/CasaOS-AppManagement/pkg/docker"
	"github.com/docker/distribution/reference"
	"github.com/docker/docker/client"
	"golang.org/x/sync/errgroup"
)

func checkAppRegistryImages(ctx context.Context, app *ComposeApp) []docker.ImageUpdate {
	results := make([]docker.ImageUpdate, len(app.Services))
	cli, clientErr := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if clientErr == nil {
		defer cli.Close()
	}
	var images map[string]string
	if clientErr == nil {
		// Resolve the API version before the fan-out: the Docker client negotiates
		// it once, on its first request, without synchronization.
		images, clientErr = currentImages(ctx, cli, app)
	}
	group := &sync.WaitGroup{}
	slots := make(chan struct{}, updateCheckServiceParallelism)
	for i, service := range app.Services {
		i, service := i, service
		result := docker.ImageUpdate{Service: service.Name, Image: service.Image, Status: "failed"}
		switch {
		case service.Image == "" || service.Build != nil || strings.HasPrefix(service.Image, "sha256:") || strings.HasPrefix(service.Image, "casaos-rollback/"):
			result.Status = "unsupported"
			result.Error = "This service uses a local or restored image. Select a registry image in app settings to check versions."
			results[i] = result
			continue
		case clientErr != nil:
			result.Error = "Could not inspect the installed containers. Check Docker and retry."
			results[i] = result
			continue
		}
		results[i] = result
		slots <- struct{}{}
		group.Add(1)
		go func() {
			defer func() { <-slots; group.Done() }()
			installed, _, err := cli.ImageInspectWithRaw(ctx, images[service.Name])
			if err != nil {
				results[i].Error = "Could not inspect the installed image. Check Docker and retry."
				return
			}
			update := docker.CheckImageUpdate(ctx, service.Image, installed)
			update.Service = service.Name
			results[i] = update
		}()
	}
	group.Wait()
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
	// Resolve the API version before the fan-out: the Docker client negotiates
	// it once, on its first request, without synchronization.
	images, err := currentImages(ctx, cli, app)
	if err != nil {
		return nil, errors.New("could not inspect every installed service; check the app and retry")
	}
	results := make([]docker.ImageUpdate, len(target.Services))
	for i, service := range target.Services {
		old := app.App(service.Name)
		if old == nil || old.Build != nil || old.Image == "" {
			return results, errors.New("this app's service layout or local build requires a manual update")
		}
		// Placeholder for services whose check is skipped after a failure.
		results[i] = docker.ImageUpdate{
			Service: service.Name, Image: old.Image, InstalledImage: old.Image, Status: "failed",
			Error: "Could not check this image registry. Check connectivity, registry credentials and rate limits, then retry.",
		}
	}
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(updateCheckServiceParallelism)
	for i, service := range target.Services {
		i, service := i, service
		old := app.App(service.Name)
		group.Go(func() error {
			installed, _, err := cli.ImageInspectWithRaw(ctx, images[service.Name])
			if err != nil {
				return errors.New("could not inspect an installed image")
			}
			result := docker.ResolveImageUpdate(ctx, old.Image, installed)
			result.Service, result.InstalledImage = service.Name, old.Image
			if result.Status == "up_to_date" && !sameImageReference(old.Image, result.LatestImage) {
				result.Status = "available"
			}
			results[i] = result
			if result.Error != "" || result.LatestImage == "" {
				return nil
			}
			target.Services[i].Image = result.LatestImage
			target.Services[i].PullPolicy = "never"
			target.Services[i].Build = nil
			return nil
		})
	}
	return results, group.Wait()
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
