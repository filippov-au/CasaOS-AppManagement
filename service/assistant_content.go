package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/IceWhaleTech/CasaOS-AppManagement/internal/assistant"
	composeTypes "github.com/compose-spec/compose-go/types"
	"golang.org/x/sys/unix"
)

type assistantContentArgs struct {
	App       string `json:"app"`
	Service   string `json:"service"`
	URL       string `json:"url"`
	Directory string `json:"directory"`
	Filename  string `json:"filename"`
	Purpose   string `json:"purpose"`
	MaxBytes  int64  `json:"max_bytes"`
	SHA256    string `json:"sha256"`
}

func assistantContentPath(s composeTypes.ServiceConfig, directory, root string) (string, error) {
	if !path.IsAbs(directory) || directory != path.Clean(directory) || strings.ContainsAny(directory, "\\\x00$") || len(directory) > 2048 {
		return "", errors.New("use a clean absolute container content directory")
	}
	for _, forbidden := range []string{"/etc", "/proc", "/sys", "/dev", "/run", "/usr", "/bin", "/sbin", "/lib", "/root", "/config"} {
		if directory == forbidden || strings.HasPrefix(directory, forbidden+"/") {
			return "", errors.New("system/config directories are not content destinations; use or add a dedicated content mount")
		}
	}
	var best *composeTypes.ServiceVolumeConfig
	for i := range s.Volumes {
		v := &s.Volumes[i]
		if directory == v.Target || strings.HasPrefix(directory, strings.TrimRight(v.Target, "/")+"/") {
			if best == nil || len(v.Target) > len(best.Target) {
				best = v
			}
		}
	}
	// Choose the most specific mount BEFORE validating it: a nested read-only or
	// named mount must not be bypassed by falling back to a writable parent mount.
	if best == nil || best.Type != "bind" || best.ReadOnly || best.Source != filepath.Clean(best.Source) || !strings.HasPrefix(best.Source, root+"/") || best.Target == "/" {
		return "", errors.New("content destination must be in the app's writable persistent bind mount below /DATA")
	}
	host := filepath.Join(best.Source, strings.TrimPrefix(strings.TrimPrefix(directory, best.Target), "/"))
	if err := assistantNoSymlink(host); err != nil {
		return "", err
	}
	st, err := os.Stat(best.Source)
	if err != nil || !st.IsDir() {
		return "", errors.New("persistent content mount must already exist on the CasaOS host")
	}
	return host, nil
}

func assistantContentPlan(ctx context.Context, name string, raw stdjson.RawMessage) (assistant.Prepared, error) {
	var a assistantContentArgs
	if err := assistantDecode(raw, &a); err != nil {
		return assistant.Prepared{}, err
	}
	app, err := assistantLoad(a.App)
	if err != nil {
		return assistant.Prepared{}, err
	}
	s, err := assistantContentService(app, a.Service)
	if err != nil {
		return assistant.Prepared{}, err
	}
	if name == "app_content" {
		return assistant.Prepared{Summary: "Inspect supplemental content for " + a.App + " / " + a.Service, Execute: func(ctx context.Context) (string, error) {
			fresh, err := assistantLoad(a.App)
			if err != nil {
				return "", err
			}
			s, err := assistantContentService(fresh, a.Service)
			if err != nil {
				return "", err
			}
			mounts := []map[string]string{}
			for _, v := range s.Volumes {
				if host, e := assistantContentPath(s, v.Target, "/DATA"); e == nil {
					mounts = append(mounts, map[string]string{"directory": v.Target, "host_directory": host})
				}
			}
			return assistantJSON(map[string]interface{}{"app": a.App, "service": a.Service, "image": s.Image, "content_mounts": mounts,
				"instructions": "Determine content suitability yourself from the user's request, inspect_app and the application's documentation. Explain what the file is and why this app can use it in download_app_content.purpose. These mounts only describe eligible storage, not which files belong here. Ask when suitability is uncertain. Use a dedicated content mount/subdirectory; if none exists, propose one with configure_service. Saving data does not prove the application imported it."}), nil
		}}, nil
	}
	if _, err := assistant.PublicURL(a.URL); err != nil {
		return assistant.Prepared{}, err
	}
	if len(a.Filename) == 0 || len(a.Filename) > 180 || a.Filename != path.Base(a.Filename) || strings.HasPrefix(a.Filename, ".") || strings.ContainsAny(a.Filename, "\\:$") || strings.IndexFunc(a.Filename, unicode.IsControl) >= 0 {
		return assistant.Prepared{}, errors.New("use a plain content filename without directories or control characters")
	}
	if len(strings.TrimSpace(a.Purpose)) < 12 || len(a.Purpose) > 800 {
		return assistant.Prepared{}, errors.New("explain how this file supplies data for the selected application")
	}
	if a.MaxBytes < 1 || a.MaxBytes > 4<<30 {
		return assistant.Prepared{}, errors.New("max_bytes must be between 1 byte and 4 GiB")
	}
	if a.SHA256 != "" {
		b, err := hex.DecodeString(a.SHA256)
		if err != nil || len(b) != sha256.Size {
			return assistant.Prepared{}, errors.New("sha256 must contain 64 hexadecimal characters")
		}
	}
	if err := assistantSafeContentName(a.Filename); err != nil {
		return assistant.Prepared{}, err
	}
	host, err := assistantContentPath(s, a.Directory, "/DATA")
	if err != nil {
		return assistant.Prepared{}, err
	}
	if _, err := os.Lstat(filepath.Join(host, a.Filename)); !os.IsNotExist(err) {
		return assistant.Prepared{}, errors.New("destination already exists or cannot be inspected; existing files are never replaced")
	}
	snapshot, _ := stdjson.Marshal(s)
	fingerprint := sha256.Sum256(snapshot)
	summary := fmt.Sprintf("Download supplemental data for %s / %s\nPurpose: %s\nSource: %s\nContainer file: %s\nHost directory: %s\nLimit: %d bytes, 15 minutes. Check for executable content. Existing files are never replaced.", a.App, a.Service, a.Purpose, a.URL, path.Join(a.Directory, a.Filename), host, a.MaxBytes)
	if a.SHA256 != "" {
		summary += "\nRequired SHA-256: " + strings.ToLower(a.SHA256)
	}
	return assistant.Prepared{Summary: summary, Execute: func(ctx context.Context) (string, error) {
		unlock, err := LockAppOperation(a.App)
		if err != nil {
			return "", err
		}
		defer unlock()
		fresh, err := assistantLoad(a.App)
		if err != nil {
			return "", err
		}
		s, err := assistantContentService(fresh, a.Service)
		if err != nil {
			return "", err
		}
		current, _ := stdjson.Marshal(s)
		if sha256.Sum256(current) != fingerprint {
			return "", errors.New("app settings changed since the download proposal; inspect and propose again")
		}
		now, err := assistantContentPath(s, a.Directory, "/DATA")
		if err != nil {
			return "", err
		}
		if now != host {
			return "", errors.New("content mount changed since proposal")
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		client := assistant.NewPublicHTTPClient(15 * time.Minute)
		defer client.CloseIdleConnections()
		return assistantDownloadContent(ctx, client, a, host)
	}}, nil
}

// Open each component relative to a pinned directory descriptor. O_NOFOLLOW
// prevents symlink swaps from redirecting writes even after path validation.
func assistantOpenContentDir(directory string, create bool) (*os.File, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, errors.New("invalid content directory")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(directory, "/"), "/") {
		if part == "" {
			continue
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e == unix.ENOENT && create {
			var parent unix.Stat_t
			if e = unix.Fstat(fd, &parent); e == nil {
				e = unix.Mkdirat(fd, part, 0755)
			}
			if e == nil || e == unix.EEXIST {
				created := e == nil
				next, e = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
				if e == nil && created && os.Geteuid() == 0 {
					e = unix.Fchown(next, int(parent.Uid), int(parent.Gid))
					if e != nil {
						unix.Close(next)
					}
				}
			}
		}
		unix.Close(fd)
		if e != nil {
			return nil, errors.New("cannot open content directory safely; check mounts, permissions and symlinks")
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), directory), nil
}

func assistantDownloadContent(ctx context.Context, client *http.Client, a assistantContentArgs, directory string) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	res, err := assistant.PublicGET(ctx, client, a.URL)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.ContentLength > a.MaxBytes {
		return "", errors.New("file exceeds the approved size limit")
	}
	if strings.Contains(strings.ToLower(res.Header.Get("Content-Type")), "text/html") {
		return "", errors.New("source returned an HTML page, not a content file; find the direct download URL")
	}
	dir, err := assistantOpenContentDir(directory, true)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	dfd := int(dir.Fd())
	stageName := ".casaos-download-" + assistant.ID()
	if err := unix.Mkdirat(dfd, stageName, 0700); err != nil {
		return "", err
	}
	defer unix.Unlinkat(dfd, stageName, unix.AT_REMOVEDIR)
	stage, err := unix.Openat(dfd, stageName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	defer unix.Close(stage)
	fd, err := unix.Openat(stage, "content", unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), "content")
	defer f.Close()
	defer unix.Unlinkat(stage, "content", 0)
	hash := sha256.New()
	progress := &assistantDownloadProgress{ctx: ctx, filename: a.Filename, total: res.ContentLength}
	assistant.Progress(ctx, "Downloading "+a.Filename+"…")
	n, err := io.Copy(io.MultiWriter(f, hash, progress), &assistantContextReader{ctx: ctx, r: io.LimitReader(res.Body, a.MaxBytes+1)})
	if err != nil {
		return "", fmt.Errorf("download interrupted; partial file removed: %w", err)
	}
	if n > a.MaxBytes {
		return "", errors.New("download exceeded approved size; partial file removed")
	}
	if n == 0 || (res.ContentLength >= 0 && n != res.ContentLength) {
		return "", errors.New("incomplete or empty download; partial file removed")
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if a.SHA256 != "" && !strings.EqualFold(a.SHA256, digest) {
		return "", errors.New("SHA-256 mismatch; downloaded file removed")
	}
	assistant.Progress(ctx, "Validating "+a.Filename+"…")
	if err := assistantValidateContent(ctx, f, a.Filename, a.MaxBytes); err != nil {
		return "", err
	}
	// Check that the visible directory is still the one we approved and opened.
	current, err := assistantOpenContentDir(directory, false)
	if err != nil {
		return "", err
	}
	oldStat, e1 := dir.Stat()
	newStat, e2 := current.Stat()
	current.Close()
	if e1 != nil || e2 != nil || !os.SameFile(oldStat, newStat) {
		return "", errors.New("content directory changed during download")
	}
	var parent unix.Stat_t
	if err := unix.Fstat(dfd, &parent); err != nil {
		return "", err
	}
	if os.Geteuid() == 0 {
		if err := f.Chown(int(parent.Uid), int(parent.Gid)); err != nil {
			return "", err
		}
	}
	if err := f.Chmod(0644); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	// A hard link publishes the completed file atomically without replacing any
	// existing file, directory or symlink. Staging is on the same filesystem.
	if err := unix.Linkat(stage, "content", dfd, a.Filename, 0); err != nil {
		return "", errors.New("could not publish downloaded content; destination may already exist (nothing replaced)")
	}
	if err := dir.Sync(); err != nil {
		return "File was created but directory sync failed; inspect before retrying.", err
	}
	assistant.Progress(ctx, fmt.Sprintf("Downloaded %s (%d bytes); safety checks passed.", a.Filename, n))
	return assistantJSON(map[string]interface{}{"app": a.App, "service": a.Service, "path": path.Join(a.Directory, a.Filename), "bytes": n, "sha256": digest, "stored": true, "imported": false, "note": "Content saved; executable-content checks passed. Check the app's documented scan/import workflow before claiming it is in the library."}), nil
}

type assistantDownloadProgress struct {
	ctx             context.Context
	filename        string
	total, received int64
	last            time.Time
}

func (p *assistantDownloadProgress) Write(b []byte) (int, error) {
	p.received += int64(len(b))
	if time.Since(p.last) >= 2*time.Second {
		text := fmt.Sprintf("Downloading %s: %d bytes", p.filename, p.received)
		if p.total > 0 {
			text += fmt.Sprintf(" / %d (%d%%)", p.total, p.received*100/p.total)
		}
		assistant.Progress(p.ctx, text)
		p.last = time.Now()
	}
	return len(b), nil
}
