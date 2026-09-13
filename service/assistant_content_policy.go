package service

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	composeTypes "github.com/compose-spec/compose-go/types"
	"golang.org/x/net/html"
)

// Compatibility and usefulness are decided by the LLM using the content
// instructions. This file only enforces a uniform no-execution boundary.
// There is deliberately no app registry or allowlist of data formats.
func assistantContentService(app *ComposeApp, name string) (composeTypes.ServiceConfig, error) {
	for _, s := range app.Services {
		if s.Name == name {
			return s, nil
		}
	}
	return composeTypes.ServiceConfig{}, errors.New("service does not belong to this app")
}

func assistantSafeContentName(name string) error {
	// A denial list for executable packages/scripts, not a list of formats an
	// application may consume. Unknown data extensions remain available.
	blocked := " .exe .dll .com .scr .msi .msp .bat .cmd .ps1 .psm1 .psd1 .sh .bash .zsh .fish .py .pyc .pyo .pl .pm .rb .php .phtml .js .mjs .cjs .vbs .vbe .wsf .wsh .jse .hta .jar .class .wasm .so .dylib .app .appimage .deb .rpm .pkg .dmg .apk .aab .ipa .run .desktop .lnk .url .reg .service .pkl .pickle .joblib .pt .pth .docm .xlsm .pptm .xlam .xll "
	for _, part := range strings.Split(strings.ToLower(path.Base(name)), ".")[1:] {
		if strings.Contains(blocked, " ."+strings.TrimSpace(part)+" ") {
			return errors.New("executable, script, installer or active-document filename refused")
		}
	}
	// These containers can hide executables; only inspectable ZIP containers
	// are currently supported by the download safety boundary.
	opaque := " .7z .rar .tar .gz .tgz .bz2 .xz .zst .iso .img "
	if ext := strings.ToLower(path.Ext(name)); ext != "" && strings.Contains(opaque, " "+ext+" ") {
		return errors.New("archive/container cannot be inspected for executable files; use individual data files or ZIP")
	}
	return nil
}

func assistantNativeExecutable(b []byte) bool {
	for _, magic := range [][]byte{[]byte("MZ"), []byte("ZM"), []byte("\x7fELF"), {0xfe, 0xed, 0xfa, 0xce}, {0xce, 0xfa, 0xed, 0xfe}, {0xfe, 0xed, 0xfa, 0xcf}, {0xcf, 0xfa, 0xed, 0xfe}, {0xca, 0xfe, 0xba, 0xbe}, {0xbe, 0xba, 0xfe, 0xca}, {0xca, 0xfe, 0xba, 0xbf}, []byte("\x00asm"), []byte("dex\n")} {
		if bytes.HasPrefix(b, magic) {
			return true
		}
	}
	return false
}

func assistantExecutable(b []byte) bool {
	if assistantNativeExecutable(b) {
		return true
	}
	text := strings.TrimSpace(strings.TrimPrefix(string(b), "\ufeff"))
	lower := strings.ToLower(text)
	for _, prefix := range []string{"#!", "@echo", "<?php", "<script", "powershell", "#!/", "import os", "import sys"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func assistantArchiveHeader(b []byte) bool {
	for _, magic := range []string{"PK\x03\x04", "PK\x05\x06", "\x1f\x8b", "7z\xbc\xaf\x27\x1c", "Rar!", "BZh", "\xfd7zXZ\x00"} {
		if bytes.HasPrefix(b, []byte(magic)) {
			return true
		}
	}
	return false
}

func assistantContentHeader(b []byte) error {
	if len(b) == 0 || assistantExecutable(b) {
		return errors.New("empty file, executable or script content refused")
	}
	return nil
}

// ZIP-based formats are recognized by their bytes, independent of the filename
// or the application. Inspect every entry before publishing the original file.
func assistantValidateContent(ctx context.Context, f *os.File, name string, maxBytes int64) error {
	if err := assistantSafeContentName(name); err != nil {
		return err
	}
	b := make([]byte, 4096)
	n, err := f.ReadAt(b, 0)
	if err != nil && err != io.EOF {
		return err
	}
	b = b[:n]
	if err := assistantContentHeader(b); err != nil {
		return err
	}
	isZIP := bytes.HasPrefix(b, []byte("PK\x03\x04")) || bytes.HasPrefix(b, []byte("PK\x05\x06"))
	if !isZIP {
		if assistantArchiveHeader(b) || (len(b) >= 262 && string(b[257:262]) == "ustar") {
			return errors.New("archive cannot be inspected for executables; use individual data files or ZIP")
		}
		return ctx.Err()
	}
	st, err := f.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		return errors.New("invalid ZIP content")
	}
	if len(zr.File) == 0 || len(zr.File) > 512 {
		return errors.New("archive must contain 1–512 entries")
	}
	var expanded uint64
	seen := map[string]bool{}
	files := 0
	for _, entry := range zr.File {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		clean := strings.TrimSuffix(entry.Name, "/")
		if clean == "" || clean != path.Clean(clean) || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsAny(clean, "\\:\x00") || seen[strings.ToLower(clean)] || entry.Mode()&(os.ModeSymlink|os.ModeDevice|os.ModeNamedPipe|os.ModeSocket) != 0 || entry.Flags&1 != 0 {
			return errors.New("archive contains an unsafe path, duplicate, link or encrypted entry")
		}
		seen[strings.ToLower(clean)] = true
		if err := assistantSafeContentName(clean); err != nil {
			return err
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		files++
		if entry.UncompressedSize64 > uint64(maxBytes) || expanded > uint64(maxBytes)-entry.UncompressedSize64 {
			return errors.New("expanded archive exceeds download size limit")
		}
		expanded += entry.UncompressedSize64
		r, err := entry.Open()
		if err != nil {
			return errors.New("cannot read archive entry")
		}
		prefix := make([]byte, 4096)
		n, readErr := io.ReadFull(r, prefix)
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			readErr = nil
		}
		if readErr == nil && n > 0 {
			readErr = assistantContentHeader(prefix[:n])
		}
		if readErr == nil && (assistantArchiveHeader(prefix[:n]) || (n >= 262 && string(prefix[257:262]) == "ustar")) {
			readErr = errors.New("nested archives are refused because they may hide executables")
		}
		if readErr == nil && strings.Contains(" .html .htm .xhtml .svg .xml .opf ", " "+strings.ToLower(path.Ext(clean))+" ") {
			readErr = assistantPassiveMarkup(&assistantContextReader{ctx: ctx, r: io.MultiReader(bytes.NewReader(prefix[:n]), io.LimitReader(r, int64(entry.UncompressedSize64)+1))})
		}
		if readErr == nil {
			_, readErr = io.Copy(io.Discard, &assistantContextReader{ctx: ctx, r: io.LimitReader(r, int64(entry.UncompressedSize64)+1)})
		}
		r.Close()
		if readErr != nil {
			return fmt.Errorf("archive safety check failed: %w", readErr)
		}
	}
	if files == 0 {
		return errors.New("archive contains no data files")
	}
	return nil
}

// Markup within data containers must not carry scripts or embedded programs.
func assistantPassiveMarkup(r io.Reader) error {
	z := html.NewTokenizer(r)
	z.SetMaxBuf(1 << 20)
	for {
		t := z.Next()
		if t == html.ErrorToken {
			if z.Err() == io.EOF {
				return nil
			}
			return z.Err()
		}
		if t != html.StartTagToken && t != html.SelfClosingTagToken {
			continue
		}
		tok := z.Token()
		if strings.Contains(" script iframe object embed foreignobject ", " "+tok.Data+" ") {
			return errors.New("active content in downloaded data is refused")
		}
		for _, a := range tok.Attr {
			value := strings.ToLower(strings.Join(strings.Fields(a.Val), ""))
			if strings.HasPrefix(a.Key, "on") || strings.HasPrefix(value, "javascript:") || strings.HasPrefix(value, "vbscript:") || strings.HasPrefix(value, "data:") {
				return errors.New("active content in downloaded data is refused")
			}
		}
	}
}

type assistantContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *assistantContextReader) Read(p []byte) (int, error) {
	if r.ctx.Err() != nil {
		return 0, r.ctx.Err()
	}
	return r.r.Read(p)
}
