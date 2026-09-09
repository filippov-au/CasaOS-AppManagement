package docker

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/distribution"
	"github.com/docker/distribution/reference"
	dockerTypes "github.com/docker/docker/api/types"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// Accept release numbers and explicit packaging variants, never arbitrary
// suffixes such as rc, beta, dev, nightly or commit hashes.
var releaseTagPattern = regexp.MustCompile(`^(v?)([0-9]+(?:\.[0-9]+){1,3})(?:-ls([0-9]+))?(-(?:rootless|alpine|slim|bookworm|bullseye))?$`)
var releaseChannelPattern = regexp.MustCompile(`^(latest|stable|v?[0-9]+(?:\.[0-9]+)?)(-(?:rootless|alpine|slim|bookworm|bullseye))?$`)

type imageRelease struct {
	tag, variant string
	numbers      [5]uint64
	precision    int
	lsBuild      bool
}

func parseImageRelease(tag string) (imageRelease, bool) {
	match := releaseTagPattern.FindStringSubmatch(tag)
	release := imageRelease{tag: tag}
	if match == nil {
		return release, false
	}
	release.variant = match[4]
	release.precision = len(strings.Split(match[2], "."))
	for i, part := range strings.Split(match[2], ".") {
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return release, false
		}
		release.numbers[i] = number
	}
	if match[3] != "" {
		release.lsBuild = true
		number, err := strconv.ParseUint(match[3], 10, 64)
		if err != nil {
			return release, false
		}
		release.numbers[4] = number
	}
	return release, true
}

func compareImageReleases(a, b imageRelease) int {
	for i := range a.numbers {
		if a.numbers[i] > b.numbers[i] {
			return 1
		}
		if a.numbers[i] < b.numbers[i] {
			return -1
		}
	}
	return 0
}

func sameReleaseScheme(a, b imageRelease) bool {
	calendar := func(r imageRelease) bool { return r.numbers[0] >= 1900 && r.numbers[0] <= 2999 }
	return a.lsBuild == b.lsBuild && calendar(a) == calendar(b)
}

func releaseCandidates(channel, installed string, tags []string) []imageRelease {
	configured, numbered := parseImageRelease(channel)
	variant := configured.variant
	var prefix []string
	if match := releaseChannelPattern.FindStringSubmatch(channel); match != nil {
		variant = match[2]
		if match[1] != "latest" && match[1] != "stable" {
			prefix = strings.Split(strings.TrimPrefix(match[1], "v"), ".")
		}
	} else if !numbered {
		return nil
	}
	baseline, known := parseImageRelease(installed)
	if numbered && known && !sameReleaseScheme(configured, baseline) {
		return nil
	}
	if numbered && (!known || compareImageReleases(configured, baseline) > 0) {
		baseline, known = configured, true
	}
	var candidates []imageRelease
	for _, tag := range tags {
		release, ok := parseImageRelease(tag)
		if !ok || release.variant != variant || (known && (!sameReleaseScheme(release, baseline) || compareImageReleases(release, baseline) < 0)) {
			continue
		}
		matches := true
		for i, part := range prefix {
			number, err := strconv.ParseUint(part, 10, 64)
			if err != nil || release.numbers[i] != number {
				matches = false
			}
		}
		if matches {
			candidates = append(candidates, release)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		order := compareImageReleases(candidates[i], candidates[j])
		if order == 0 {
			if candidates[i].precision != candidates[j].precision {
				return candidates[i].precision > candidates[j].precision
			}
			if candidates[i].tag == channel || candidates[j].tag == channel {
				return candidates[i].tag == channel
			}
			return candidates[i].tag < candidates[j].tag
		}
		return order > 0
	})
	return candidates
}

func checkRepositoryRelease(ctx context.Context, repo distribution.Repository, named reference.Named, tag string, installed dockerTypes.ImageInspect, result ImageUpdate) ImageUpdate {
	tags, err := repo.Tags(ctx).All(ctx)
	if err != nil {
		return result
	}
	candidates := releaseCandidates(tag, result.CurrentVersion, tags)
	result.Error = "No compatible numbered stable release was found. Unversioned and development builds are not offered."
	platform := v1.Platform{OS: installed.Os, Architecture: installed.Architecture, Variant: installed.Variant}
	// When labels cannot identify the installed release, first match its actual
	// config digest to a published release. Never guess update direction from a tag.
	cache := map[string]registryImageInfo{}
	if _, known := parseImageRelease(result.CurrentVersion); !known {
		matched := false
		for i, candidate := range candidates {
			if i == 20 {
				break
			}
			info, err := platformImageInfo(ctx, repo, candidate.tag, "", platform, 0)
			if errors.Is(err, errImagePlatform) {
				continue
			}
			if err != nil {
				return result
			}
			cache[candidate.tag] = info
			if info.ID == installed.ID {
				result.CurrentVersion, matched = candidate.tag, true
				break
			}
		}
		if !matched {
			result.Error = "Could not identify the installed release. Updating is disabled because a downgrade cannot be ruled out."
			return result
		}
		candidates = releaseCandidates(tag, result.CurrentVersion, tags)
	}
	for i, candidate := range candidates {
		if i == 20 {
			break
		}
		info, cached := cache[candidate.tag]
		var err error
		if !cached {
			info, err = platformImageInfo(ctx, repo, candidate.tag, "", platform, 0)
		}
		if errors.Is(err, errImagePlatform) {
			continue
		}
		if err != nil {
			result.Error = "Could not verify the numbered release. Check registry connectivity and retry."
			return result
		}
		ref, _ := reference.WithTag(reference.TrimNamed(named), candidate.tag)
		result.LatestImage = reference.FamiliarString(ref)
		// The verified release tag supplies the version even when labels are absent.
		result.LatestVersion, result.LatestImageID = candidate.tag, info.ID
		if info.ID == installed.ID {
			result.CurrentVersion = candidate.tag
		}
		result.Status, result.Error = "up_to_date", ""
		if info.ID != installed.ID || candidate.tag != tag {
			result.Status = "available"
		}
		return result
	}
	return result
}
