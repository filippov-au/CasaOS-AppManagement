package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/IceWhaleTech/CasaOS-AppManagement/codegen"
	"github.com/IceWhaleTech/CasaOS-AppManagement/common"
	"github.com/IceWhaleTech/CasaOS-AppManagement/pkg/config"
	"github.com/IceWhaleTech/CasaOS-AppManagement/pkg/docker"
	"github.com/compose-spec/compose-go/types"
	"github.com/docker/compose/v2/pkg/api"
	reference "github.com/docker/distribution/reference"
	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/imdario/mergo"
	"github.com/mohae/deepcopy"
	"gopkg.in/yaml.v3"
)

type dockerUpdateRuntime struct{}

func cloneCompose(app *ComposeApp) (*ComposeApp, error) {
	return deepcopy.Copy(app).(*ComposeApp), nil
}

func resolvedComposeYAML(app *ComposeApp) ([]byte, error) {
	b, err := yaml.Marshal(app)
	// Loaded projects are already interpolated. Escape literal dollars for subsequent Compose loads.
	return []byte(strings.ReplaceAll(string(b), "$", "$$")), err
}

// A source is only selected when unambiguous; never silently switch between stores with the same app ID.
func storeUpdateTarget(app *ComposeApp) (*ComposeApp, error) {
	info, err := app.StoreInfo(false)
	if err != nil || info == nil || info.StoreAppID == nil || *info.StoreAppID == "" {
		return nil, ErrStoreInfoNotFound
	}
	if info.IsUncontrolled != nil && *info.IsUncontrolled {
		return nil, errors.New("this app uses a custom version; switch to its store version in app settings first")
	}
	var stored *ComposeApp
	source := ""
	if ext, ok := app.Extensions[common.ComposeExtensionNameXCasaOS].(map[string]interface{}); ok {
		source, _ = ext["store_url"].(string)
	}
	sources := config.ServerInfo.AppStoreList
	if source != "" {
		found := false
		for _, url := range sources {
			if url == source {
				found = true
			}
		}
		if !found {
			return nil, ErrNotFoundInAppStore
		}
		sources = []string{source}
	}
	for _, url := range sources {
		store, err := AppStoreByURL(url)
		if err != nil {
			return nil, err
		}
		candidate, err := store.ComposeApp(*info.StoreAppID)
		if err != nil {
			return nil, err
		}
		if candidate == nil {
			continue
		}
		if stored != nil {
			return nil, errors.New("this app exists in multiple stores; remove the duplicate source before updating")
		}
		stored = candidate
		source = url
	}
	if stored == nil && source == "" {
		store, err := NewDefaultAppStore()
		if err == nil {
			stored, err = store.ComposeApp(*info.StoreAppID)
			if err != nil {
				return nil, err
			}
		}
	}
	if stored == nil {
		return nil, ErrNotFoundInAppStore
	}
	target, err := mergeStoreImages(app, stored)
	if err != nil {
		return nil, err
	}
	if ext, ok := target.Extensions[common.ComposeExtensionNameXCasaOS].(map[string]interface{}); ok {
		ext["store_url"] = source
	}
	return target, nil
}

func mergeStoreImages(app, store *ComposeApp) (*ComposeApp, error) {
	if len(app.Services) != len(store.Services) {
		return nil, ErrComposeAppNotMatch
	}
	target, err := cloneCompose(app)
	if err != nil {
		return nil, err
	}
	store, err = cloneCompose(store)
	if err != nil {
		return nil, err
	}
	for i, s := range target.Services {
		other := store.App(s.Name)
		if other == nil || other.Image == "" {
			return nil, ErrComposeAppNotMatch
		}
		// Add current catalog defaults while retaining explicit installed settings,
		// especially data mounts, ports, commands and credential values.
		if err := mergo.Merge(&target.Services[i], types.ServiceConfig(*other)); err != nil {
			return nil, err
		}
		target.Services[i].Image = other.Image
		target.Services[i].PullPolicy = "never" // downloads are explicit and verified before any replacement
		target.Services[i].Build = nil
	}
	if err := mergo.Merge(&target.Volumes, store.Volumes); err != nil {
		return nil, err
	}
	if err := mergo.Merge(&target.Networks, store.Networks); err != nil {
		return nil, err
	}
	if err := mergo.Merge(&target.Configs, store.Configs); err != nil {
		return nil, err
	}
	if err := mergo.Merge(&target.Secrets, store.Secrets); err != nil {
		return nil, err
	}
	if installed, ok := target.Extensions[common.ComposeExtensionNameXCasaOS].(map[string]interface{}); ok {
		if metadata, ok := store.Extensions[common.ComposeExtensionNameXCasaOS].(map[string]interface{}); ok {
			for _, key := range []string{"description", "tagline", "icon", "thumbnail", "screenshot_link", "author", "developer", "category", "architectures", "tips", "changelog"} {
				if value, exists := metadata[key]; exists {
					installed[key] = value
				}
			}
		}
	}
	target.injectEnvVariableToComposeApp()
	return target, nil
}

func currentImages(ctx context.Context, cli client.APIClient, app *ComposeApp) (map[string]string, error) {
	containers, err := cli.ContainerList(ctx, dockerTypes.ContainerListOptions{All: true, Filters: filters.NewArgs(
		filters.Arg("label", "com.docker.compose.project="+app.Name),
		filters.Arg("label", "com.docker.compose.oneoff=False"),
	)})
	if err != nil {
		return nil, err
	}
	images := map[string]string{}
	for _, c := range containers {
		name := c.Labels["com.docker.compose.service"]
		if id, ok := images[name]; ok && id != c.ImageID {
			return nil, fmt.Errorf("service %s has containers using different images", name)
		}
		images[name] = c.ImageID
	}
	for _, s := range app.Services {
		if images[s.Name] == "" {
			return nil, fmt.Errorf("no installed container found for service %s", s.Name)
		}
	}
	return images, nil
}

func (dockerUpdateRuntime) Capture(ctx context.Context, app *ComposeApp) (*updateSnapshot, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	images, err := currentImages(ctx, cli, app)
	if err != nil {
		return nil, err
	}
	copy, err := cloneCompose(app)
	if err != nil {
		return nil, err
	}
	copy.injectEnvVariableToComposeApp()
	snapshot := &updateSnapshot{Images: map[string]string{}, CreatedAt: time.Now().UTC()}
	snapshot.Version = updateVersion(app)
	success := false
	defer func() {
		if !success {
			(dockerUpdateRuntime{}).Release(ctx, snapshot)
		}
	}()
	for i, s := range copy.Services {
		id := images[s.Name]
		tag := fmt.Sprintf("casaos-rollback/%x:%d-%d", sha256.Sum256([]byte(app.Name)), snapshot.CreatedAt.UnixNano(), i)
		if err := cli.ImageTag(ctx, id, tag); err != nil {
			return nil, err
		}
		snapshot.Images[tag] = id
		copy.Services[i].Image = tag
		copy.Services[i].PullPolicy = "never"
		copy.Services[i].Build = nil
	}
	// Preserve effective environment values, including interpolated env files and global defaults.
	containers, err := cli.ContainerList(ctx, dockerTypes.ContainerListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", "com.docker.compose.project="+app.Name), filters.Arg("label", "com.docker.compose.oneoff=False"))})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, container := range containers {
		service := copy.App(container.Labels["com.docker.compose.service"])
		if service == nil {
			continue
		}
		inspect, err := cli.ContainerInspect(ctx, container.ID)
		if err != nil {
			return nil, err
		}
		if seen[service.Name] {
			return nil, fmt.Errorf("rollback for replicated service %s is not supported", service.Name)
		}
		seen[service.Name] = true
		// Pin anonymous volumes to their existing Docker names, so recovery can find them
		// even if a failed recreate removed the containers that originally owned them.
		// Dockerfiles may declare volumes that were never written into Compose.
		for _, mount := range inspect.Mounts {
			if string(mount.Type) != "volume" {
				continue
			}
			found := false
			for _, volume := range service.Volumes {
				if volume.Target == mount.Destination {
					found = true
					break
				}
			}
			if !found {
				service.Volumes = append(service.Volumes, types.ServiceVolumeConfig{Type: "volume", Target: mount.Destination})
			}
		}
		for i, volume := range service.Volumes {
			if volume.Type != "volume" || volume.Source != "" {
				continue
			}
			found := false
			for _, mount := range inspect.Mounts {
				if mount.Destination != volume.Target || mount.Name == "" {
					continue
				}
				if copy.Volumes == nil {
					copy.Volumes = map[string]types.VolumeConfig{}
				}
				copy.Volumes[mount.Name] = types.VolumeConfig{Name: mount.Name, External: types.External{External: true}}
				service.Volumes[i].Source = mount.Name
				found = true
				break
			}
			if !found {
				return nil, fmt.Errorf("could not retain volume %s for service %s", volume.Target, service.Name)
			}
		}
		service.Environment = types.NewMappingWithEquals(inspect.Config.Env)
		service.EnvFile = nil
	}
	snapshot.YAML, err = resolvedComposeYAML(copy)
	success = err == nil
	return snapshot, err
}

func (dockerUpdateRuntime) Pull(ctx context.Context, app *ComposeApp) error {
	for _, s := range app.Services {
		if err := docker.PullImage(ctx, s.Image, nil); err != nil {
			return fmt.Errorf("could not download service %s: %w", s.Name, err)
		}
	}
	return nil
}

func (dockerUpdateRuntime) Verify(ctx context.Context, app *ComposeApp, expected map[string]string) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()
	for _, service := range app.Services {
		image, _, err := cli.ImageInspectWithRaw(ctx, service.Image)
		if err != nil {
			return err
		}
		if expected[service.Name] == "" || image.ID != expected[service.Name] {
			return fmt.Errorf("the image for %s changed since the update check; check for updates again", service.Name)
		}
	}
	return nil
}

func (dockerUpdateRuntime) Apply(ctx context.Context, old *ComposeApp, data []byte) error {
	project, err := NewComposeAppFromYAML(data, false, false)
	if err != nil {
		return err
	}
	project.Name = old.Name
	project.WorkingDir = filepath.Dir(old.ComposeFiles[0])
	project.ComposeFiles = old.ComposeFiles
	svc, cli, err := apiService()
	if err != nil {
		return err
	}
	defer cli.Close()
	// Resolve the local images before writing or replacing any containers. Pulling is forbidden here.
	for i, s := range project.Services {
		image, _, err := cli.ImageInspectWithRaw(ctx, s.Image)
		if err != nil {
			return err
		}
		project.Services[i].CustomLabels = map[string]string{
			api.ProjectLabel:     project.Name,
			api.ServiceLabel:     s.Name,
			api.VersionLabel:     api.ComposeVersion,
			api.WorkingDirLabel:  project.WorkingDir,
			api.ConfigFilesLabel: strings.Join(project.ComposeFiles, ","),
			api.OneoffLabel:      "False",
		}
		project.Services[i].Image = image.ID
		project.Services[i].PullPolicy = "never"
		project.Services[i].Build = nil
	}
	if err := atomicPrivateWrite(old.ComposeFiles[0], data); err != nil {
		return err
	}
	return svc.Up(ctx, (*codegen.ComposeApp)(project), api.UpOptions{
		Create: api.CreateOptions{Recreate: api.RecreateForce, Inherit: true},
		Start:  api.StartOptions{Wait: true, WaitTimeout: 5 * time.Minute},
	})
}

func (dockerUpdateRuntime) Available(ctx context.Context, snapshot *updateSnapshot) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()
	for ref, id := range snapshot.Images {
		img, _, err := cli.ImageInspectWithRaw(ctx, ref)
		if err != nil {
			return err
		}
		if img.ID != id {
			return errors.New("retained image reference no longer matches the saved image")
		}
	}
	return nil
}
func (dockerUpdateRuntime) Release(ctx context.Context, snapshot *updateSnapshot) {
	if snapshot == nil {
		return
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return
	}
	defer cli.Close()
	for ref := range snapshot.Images {
		// Remove only our retention tags. Never force removal of an image still used by a container.
		_, _ = cli.ImageRemove(ctx, ref, dockerTypes.ImageRemoveOptions{PruneChildren: false})
	}
}

func (dockerUpdateRuntime) Check(ctx context.Context, app, target *ComposeApp) (bool, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return false, err
	}
	defer cli.Close()
	images, err := currentImages(ctx, cli, app)
	if err != nil {
		return false, err
	}
	available := false
	for _, s := range target.Services {
		image, _, err := cli.ImageInspectWithRaw(ctx, images[s.Name])
		if err != nil {
			return false, err
		}
		match, err := matchesStoreImage(s.Image, image.RepoDigests, docker.CompareDigest)
		if err != nil {
			return false, fmt.Errorf("could not check service %s: %w", s.Name, err)
		}
		available = available || !match
	}
	return available, nil
}

func matchesStoreImage(image string, digests []string, compare func(string, []string) (bool, error)) (bool, error) {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return false, err
	}
	if pinned, ok := named.(reference.Canonical); ok {
		for _, digest := range digests {
			parts := strings.SplitN(digest, "@", 2)
			if len(parts) == 2 && parts[1] == pinned.Digest().String() {
				return true, nil
			}
		}
		return false, nil
	}
	return compare(image, digests)
}
