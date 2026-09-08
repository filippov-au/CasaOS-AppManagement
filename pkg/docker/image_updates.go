package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	cliconfig "github.com/docker/cli/cli/config"
	"github.com/docker/distribution"
	_ "github.com/docker/distribution/manifest/manifestlist"
	_ "github.com/docker/distribution/manifest/ocischema"
	_ "github.com/docker/distribution/manifest/schema2"
	"github.com/docker/distribution/reference"
	registry "github.com/docker/distribution/registry/client"
	"github.com/docker/distribution/registry/client/auth"
	"github.com/docker/distribution/registry/client/auth/challenge"
	"github.com/docker/distribution/registry/client/transport"
	dockerTypes "github.com/docker/docker/api/types"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// ImageUpdate is advisory. Checking never pulls images or changes a Compose file.
type ImageUpdate struct {
	Service        string `json:"service"`
	Image          string `json:"image"`
	Status         string `json:"status"`
	CurrentImageID string `json:"current_image_id,omitempty"`
	LatestImageID  string `json:"latest_image_id,omitempty"`
	LatestImage    string `json:"latest_image,omitempty"`
	LatestVersion  string `json:"latest_version,omitempty"`
	Error          string `json:"error,omitempty"`
	InstalledImage string `json:"installed_image,omitempty"`
}

// Attach the caller's deadline to registry requests, including the authentication
// library's token requests. Never downgrade registry credentials to HTTP.
type registryTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t registryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" {
		return nil, errors.New("registry checks require HTTPS")
	}
	return t.base.RoundTrip(r.Clone(t.ctx))
}

type registryCredentials struct{ dockerTypes.AuthConfig }

func (c *registryCredentials) Basic(*url.URL) (string, string)             { return c.Username, c.Password }
func (c *registryCredentials) RefreshToken(*url.URL, string) string        { return c.IdentityToken }
func (c *registryCredentials) SetRefreshToken(_ *url.URL, _, token string) { c.IdentityToken = token }

func imageRepository(ctx context.Context, named reference.Named) (distribution.Repository, error) {
	host := reference.Domain(named)
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	baseURL := "https://" + host
	rt := registryTransport{ctx: ctx, base: http.DefaultTransport}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v2/", nil)
	if err != nil {
		return nil, err
	}
	response, err := (&http.Client{Transport: rt}).Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusUnauthorized {
		return nil, fmt.Errorf("registry returned HTTP %d", response.StatusCode)
	}
	challenges := challenge.NewSimpleManager()
	if err := challenges.AddResponse(response); err != nil {
		return nil, err
	}
	creds := &registryCredentials{}
	if encoded, err := EncodedEnvAuth(named.String()); err == nil {
		data, err := base64.URLEncoding.DecodeString(encoded)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &creds.AuthConfig); err != nil {
			return nil, err
		}
	} else {
		// Use the same config directory as image pulls, with Docker's standard
		// registry keys and per-registry credential helpers.
		configDir := os.Getenv("DOCKER_CONFIG")
		if configDir == "" {
			configDir = "/"
		}
		config, err := cliconfig.Load(configDir)
		if err != nil {
			return nil, err
		}
		key := reference.Domain(named)
		if key == "docker.io" {
			key = "https://index.docker.io/v1/"
		}
		credentials, err := config.GetAuthConfig(key)
		if err != nil {
			return nil, err
		}
		creds.AuthConfig = dockerTypes.AuthConfig{Username: credentials.Username, Password: credentials.Password, IdentityToken: credentials.IdentityToken}
	}
	authorizer := auth.NewAuthorizer(challenges,
		auth.NewTokenHandler(rt, creds, reference.Path(named), "pull"), auth.NewBasicHandler(creds))
	name, err := reference.WithName(reference.Path(named))
	if err != nil {
		return nil, err
	}
	return registry.NewRepository(name, baseURL, transport.NewTransport(rt, authorizer))
}

var errImagePlatform = errors.New("image is unavailable for the installed platform")

// Compare the platform's config digest to Docker's installed image ID. Comparing
// only an index digest incorrectly flags changes confined to other architectures.
func platformImageID(ctx context.Context, repo distribution.Repository, tag string, id digest.Digest, platform v1.Platform, depth int) (string, error) {
	if depth > 4 {
		return "", errors.New("image manifest nesting is too deep")
	}
	manifests, err := repo.Manifests(ctx)
	if err != nil {
		return "", err
	}
	var options []distribution.ManifestServiceOption
	if tag != "" {
		options = append(options, distribution.WithTag(tag))
	}
	manifest, err := manifests.Get(ctx, id, options...)
	if err != nil {
		return "", err
	}
	_, payload, err := manifest.Payload()
	if err != nil {
		return "", err
	}
	var document struct {
		Config    v1.Descriptor   `json:"config"`
		Manifests []v1.Descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		return "", err
	}
	if len(document.Manifests) > 0 {
		for _, child := range document.Manifests {
			p := child.Platform
			if p != nil && p.OS == platform.OS && p.Architecture == platform.Architecture && (platform.Variant == "" || p.Variant == platform.Variant) {
				return platformImageID(ctx, repo, "", child.Digest, platform, depth+1)
			}
		}
		return "", errImagePlatform
	}
	if err := document.Config.Digest.Validate(); err != nil {
		return "", errors.New("registry returned an invalid image config digest")
	}
	// Single-platform manifests have no platform descriptor; inspect their config.
	config, err := repo.Blobs(ctx).Get(ctx, document.Config.Digest)
	if err != nil {
		return "", err
	}
	if digest.FromBytes(config) != document.Config.Digest {
		return "", errors.New("registry image config digest mismatch")
	}
	var image v1.Image
	if err := json.Unmarshal(config, &image); err != nil {
		return "", err
	}
	if image.OS != platform.OS || image.Architecture != platform.Architecture || (platform.Variant != "" && image.Variant != platform.Variant) {
		return "", errImagePlatform
	}
	return document.Config.Digest.String(), nil
}

var stableVersionTag = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)

// Only unambiguous stable version tags are ordered. Floating channels and custom
// suffixes continue following their configured tag; prereleases are not suggested.
func newerStableTags(current string, tags []string) []string {
	if !stableVersionTag.MatchString(current) {
		return nil
	}
	version, err := semver.NewVersion(current)
	if err != nil {
		return nil
	}
	var result []string
	for _, tag := range tags {
		if !stableVersionTag.MatchString(tag) || strings.HasPrefix(current, "v") != strings.HasPrefix(tag, "v") {
			continue
		}
		other, err := semver.NewVersion(tag)
		if err == nil && other.GreaterThan(version) {
			result = append(result, tag)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, _ := semver.NewVersion(result[i])
		b, _ := semver.NewVersion(result[j])
		return a.GreaterThan(b)
	})
	return result
}

func CheckImageUpdate(ctx context.Context, image string, installed dockerTypes.ImageInspect) ImageUpdate {
	return checkImageUpdate(ctx, image, installed, false)
}

// ResolveImageUpdate also resolves store-pinned digests for verified installation.
func ResolveImageUpdate(ctx context.Context, image string, installed dockerTypes.ImageInspect) ImageUpdate {
	return checkImageUpdate(ctx, image, installed, true)
}

func checkImageUpdate(ctx context.Context, image string, installed dockerTypes.ImageInspect, resolvePinned bool) ImageUpdate {
	result := ImageUpdate{Image: image, CurrentImageID: installed.ID, Status: "failed"}
	// Error details from authentication may contain token URLs. Expose a safe
	// actionable message rather than serializing remote errors into app status.
	result.Error = "Could not check this image registry. Check connectivity, registry credentials and rate limits, then retry."
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		result.Error = "This service does not reference a registry image."
		return result
	}
	pinned, isPinned := named.(reference.Canonical)
	if isPinned && !resolvePinned {
		result.Status, result.Error = "pinned", ""
		return result
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	repo, err := imageRepository(ctx, named)
	if err != nil {
		return result
	}
	if isPinned {
		id, err := platformImageID(ctx, repo, "", pinned.Digest(), v1.Platform{OS: installed.Os, Architecture: installed.Architecture, Variant: installed.Variant}, 0)
		if err != nil {
			return result
		}
		result.Status, result.Error = "up_to_date", ""
		result.LatestImageID, result.LatestImage = id, image
		if id != installed.ID {
			result.Status = "available"
		}
		return result
	}
	named = reference.TagNameOnly(named)
	tag := named.(reference.Tagged).Tag()
	return checkRepositoryImage(ctx, repo, named, tag, installed, result)
}

func checkRepositoryImage(ctx context.Context, repo distribution.Repository, named reference.Named, tag string, installed dockerTypes.ImageInspect, result ImageUpdate) ImageUpdate {
	platform := v1.Platform{OS: installed.Os, Architecture: installed.Architecture, Variant: installed.Variant}
	latestID, err := platformImageID(ctx, repo, tag, "", platform, 0)
	if err != nil {
		return result
	}
	latestTag := tag
	if stableVersionTag.MatchString(tag) {
		tags, err := repo.Tags(ctx).All(ctx)
		if err != nil {
			return result
		}
		candidates := newerStableTags(tag, tags)
		for i, candidate := range candidates {
			if i == 20 {
				result.Error = "Too many newer tags lack a matching platform. Review the registry manually."
				return result
			}
			id, err := platformImageID(ctx, repo, candidate, "", platform, 0)
			if errors.Is(err, errImagePlatform) {
				continue
			}
			if err != nil {
				return result
			}
			latestID, latestTag = id, candidate
			break
		}
	}
	result.Status, result.Error = "up_to_date", ""
	result.LatestVersion, result.LatestImageID = latestTag, latestID
	ref, _ := reference.WithTag(reference.TrimNamed(named), latestTag)
	result.LatestImage = reference.FamiliarString(ref)
	if latestID != installed.ID || latestTag != tag {
		result.Status = "available"
	}
	return result
}
