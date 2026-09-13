package service

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	composeTypes "github.com/compose-spec/compose-go/types"
)

func contentTestDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
func testROM() []byte {
	return append([]byte{'N', 'E', 'S', 0x1a, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, bytes.Repeat([]byte{0x42}, 16384)...)
}
func testMOBI() []byte { b := make([]byte, 128); copy(b[60:], "BOOKMOBI"); return b }
func testContentZIP(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for name, data := range entries {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func validateContentFixture(t *testing.T, name string, data []byte, limit int64) error {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "content")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	return assistantValidateContent(context.Background(), f, name, limit)
}

func TestAssistantContentSafetyDoesNotClassifyAppData(t *testing.T) {
	// New data types and unfamiliar applications need no backend registration.
	for _, name := range []string{"book.mobi", "game.nes", "dictionary.dic", "data.csv", "catalog.json", "weights.safetensors", "records.customdata"} {
		if err := assistantSafeContentName(name); err != nil {
			t.Errorf("data name %s rejected: %v", name, err)
		}
	}
	for _, name := range []string{"install.exe", "install.exe.mobi", "plugin.js", "script.py", "model.pkl", "package.deb"} {
		if err := assistantSafeContentName(name); err == nil {
			t.Errorf("executable name %s accepted", name)
		}
	}
	for _, tool := range (AssistantRuntime{}).Tools() {
		if tool.Function.Name == "download_app_content" {
			props := tool.Function.Parameters["properties"].(map[string]interface{})
			if _, exists := props["kind"]; exists {
				t.Fatal("backend still asks for a predefined content category")
			}
			if _, exists := props["purpose"]; !exists {
				t.Fatal("missing model explanation")
			}
		}
	}
	if !(AssistantRuntime{}).IsWrite("download_app_content") || (AssistantRuntime{}).IsWrite("web_search") || (AssistantRuntime{}).IsWrite("web_read") || (AssistantRuntime{}).IsWrite("app_content") {
		t.Fatal("incorrect permission classes")
	}
}

func TestAssistantContentPathUsesMostSpecificWritableMount(t *testing.T) {
	root := contentTestDir(t)
	source := filepath.Join(root, "library")
	if err := os.Mkdir(source, 0755); err != nil {
		t.Fatal(err)
	}
	s := composeTypes.ServiceConfig{Volumes: []composeTypes.ServiceVolumeConfig{{Type: "bind", Source: source, Target: "/books"}}}
	host, err := assistantContentPath(s, "/books/incoming", root)
	if err != nil || host != filepath.Join(source, "incoming") {
		t.Fatal(host, err)
	}
	for _, target := range []string{"/books/../escape", "/books//incoming", "/other", "/books2", "/config", "/books/evil\\file"} {
		if _, err := assistantContentPath(s, target, root); err == nil {
			t.Errorf("accepted %s", target)
		}
	}
	for _, nested := range []composeTypes.ServiceVolumeConfig{{Type: "bind", Source: source, Target: "/books/private", ReadOnly: true}, {Type: "volume", Source: "config", Target: "/books/private"}} {
		copy := s
		copy.Volumes = append(append([]composeTypes.ServiceVolumeConfig{}, s.Volumes...), nested)
		if _, err := assistantContentPath(copy, "/books/private/new", root); err == nil {
			t.Fatal("bypassed nested mount", nested)
		}
	}
	out := contentTestDir(t)
	if err := os.Symlink(out, filepath.Join(source, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := assistantContentPath(s, "/books/escape", root); err == nil {
		t.Fatal("accepted symlink")
	}
	s.Volumes[0].Source = out
	if _, err := assistantContentPath(s, "/books", root); err == nil {
		t.Fatal("accepted source outside content root")
	}
}

func TestAssistantContentRejectsExecutablesAndOpaqueArchives(t *testing.T) {
	for _, magic := range [][]byte{[]byte("MZ"), []byte("\x7fELF"), {0xcf, 0xfa, 0xed, 0xfe}, []byte("#!/bin/sh\n"), []byte("@echo off\n"), []byte("PK\x03\x04"), []byte("7z\xbc\xaf\x27\x1c")} {
		b := append(magic, bytes.Repeat([]byte{0x42}, 4096)...)
		if err := validateContentFixture(t, "game.bin", b, 1<<20); err == nil {
			t.Errorf("accepted disguised content %q", magic)
		}
	}
	// Compatibility is the model's decision; the guard permits non-executable
	// supplemental data without requiring a known format or validating a type label.
	for _, sample := range []struct {
		name string
		data []byte
	}{
		{"book.mobi", testMOBI()}, {"game.nes", testROM()},
		{"dictionary.dic", []byte("hello=привет\n")},
		{"records.customdata", []byte("arbitrary application data")},
	} {
		if err := validateContentFixture(t, sample.name, sample.data, 1<<20); err != nil {
			t.Fatal(sample.name, err)
		}
	}

}

func TestAssistantContentInspectsEveryArchiveEntry(t *testing.T) {
	good := testContentZIP(t, map[string][]byte{"games/homebrew.nes": testROM()})
	if err := validateContentFixture(t, "games.zip", good, 1<<20); err != nil {
		t.Fatal(err)
	}
	for _, entries := range []map[string][]byte{
		{"../escape.nes": testROM()},
		{"C:\\escape.nes": testROM()},
		{"game.nes": testROM(), "installer.exe": []byte("MZ executable")},
		{"game.nes": testROM(), "evil.bin": append([]byte("\x7fELF"), bytes.Repeat([]byte{0}, 128)...)},
		{"nested.zip": good},
		{"nested.bin": good},
		{"Game.nes": testROM(), "game.nes": testROM()},
	} {
		if err := validateContentFixture(t, "games.zip", testContentZIP(t, entries), 1<<20); err == nil {
			t.Fatal("unsafe archive accepted", entries)
		}
	}
	if err := validateContentFixture(t, "games.zip", good, 128); err == nil {
		t.Fatal("expanded size ignored")
	}
	corrupt := append([]byte{}, good...)
	// Corrupt compressed data while retaining the central directory/header.
	corrupt[55] ^= 0xff
	if err := validateContentFixture(t, "games.zip", corrupt, 1<<20); err == nil {
		t.Fatal("CRC/stream corruption ignored")
	}
}

func TestAssistantContentEPUBRejectsActiveAndBinaryAttachments(t *testing.T) {
	base := map[string][]byte{"mimetype": []byte("application/epub+zip"), "META-INF/container.xml": []byte(`<container/>`), "text/chapter.xhtml": []byte(`<html><body><p>Book text</p></body></html>`)}
	if err := validateContentFixture(t, "book.epub", testContentZIP(t, base), 1<<20); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct {
		name string
		data []byte
	}{
		{"setup.exe", []byte("MZ")},
		{"evil.html", []byte("\x7fELF fake binary")},
		{"script.js", []byte("run()")},
		{"font.ttf", testContentZIP(t, map[string][]byte{"install.exe": []byte("MZ")})},
		{"evil.xhtml", []byte(`<html><script>run()</script></html>`)},
		{"evil.xhtml", []byte(`<html><body onload="run()">text</body></html>`)},
		{"evil.svg", []byte(`<svg><a href="javascript:run()"/></svg>`)},
	} {
		entries := map[string][]byte{}
		for k, v := range base {
			entries[k] = v
		}
		entries[bad.name] = bad.data
		if err := validateContentFixture(t, "book.epub", testContentZIP(t, entries), 1<<20); err == nil {
			t.Fatal("unsafe EPUB accepted", bad.name)
		}
	}
}

func TestAssistantEPUBMarkupHasBoundedMemory(t *testing.T) {
	if err := assistantPassiveMarkup(strings.NewReader("<p>" + strings.Repeat("x", 2<<20) + "</p>")); err == nil {
		t.Fatal("accepted an unbounded markup token")
	}
}

type contentRoundTripper func(*http.Request) (*http.Response, error)

func (f contentRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func contentHTTPFixture(data []byte, length int64) *http.Client {
	return &http.Client{Transport: contentRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			return nil, fmt.Errorf("credentials leaked")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: io.NopCloser(bytes.NewReader(data)), ContentLength: length, Request: r}, nil
	})}
}
func contentArgs() assistantContentArgs {
	return assistantContentArgs{App: "books", Service: "calibre", URL: "https://example.org/book.mobi", Directory: "/books/incoming", Filename: "book.mobi", Purpose: "Add the requested book to the Calibre library", MaxBytes: 1 << 20}
}
func assertNoContent(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("partial content left behind", entries, err)
	}
}

func TestAssistantDownloadStoresValidatedDataWithoutOverwrite(t *testing.T) {
	dir := contentTestDir(t)
	a := contentArgs()
	body := testMOBI()
	hash := sha256.Sum256(body)
	a.SHA256 = hex.EncodeToString(hash[:])
	result, err := assistantDownloadContent(context.Background(), contentHTTPFixture(body, int64(len(body))), a, dir)
	if err != nil || !strings.Contains(result, `"stored": true`) || !strings.Contains(result, a.SHA256) || !strings.Contains(result, `"imported": false`) {
		t.Fatal(result, err)
	}
	f := filepath.Join(dir, a.Filename)
	st, err := os.Stat(f)
	if err != nil || st.Mode().Perm()&0111 != 0 {
		t.Fatal("executable file mode", st, err)
	}
	if _, err := assistantDownloadContent(context.Background(), contentHTTPFixture(body, -1), a, dir); err == nil {
		t.Fatal("overwrote existing content")
	}
	saved, _ := os.ReadFile(f)
	entries, _ := os.ReadDir(dir)
	if !bytes.Equal(saved, body) || len(entries) != 1 {
		t.Fatal("existing file changed or staging leaked", entries)
	}
}

func TestAssistantDownloadCleansFailures(t *testing.T) {
	for _, test := range []struct {
		name        string
		body        []byte
		length, max int64
		hash        string
	}{
		{"known-size", testMOBI(), 128, 50, ""},
		{"stream-size", testMOBI(), -1, 50, ""},
		{"truncated", testMOBI(), 256, 1024, ""},
		{"checksum", testMOBI(), 128, 1024, strings.Repeat("0", 64)},
		{"executable", append([]byte("MZ"), bytes.Repeat([]byte{0}, 128)...), 130, 1024, ""},
		{"empty", nil, 0, 1024, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := contentTestDir(t)
			a := contentArgs()
			a.MaxBytes = test.max
			a.SHA256 = test.hash
			if _, err := assistantDownloadContent(context.Background(), contentHTTPFixture(test.body, test.length), a, dir); err == nil {
				t.Fatal("invalid download succeeded")
			}
			assertNoContent(t, dir)
		})
	}
}

type cancellingContentReader struct {
	cancel context.CancelFunc
	sent   bool
}

func (r *cancellingContentReader) Read(b []byte) (int, error) {
	if r.sent {
		return 0, io.EOF
	}
	r.sent = true
	n := copy(b, testMOBI())
	r.cancel()
	return n, nil
}
func TestAssistantDownloadStopAndSymlinkProtection(t *testing.T) {
	dir := contentTestDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &http.Client{Transport: contentRoundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(&cancellingContentReader{cancel: cancel}), ContentLength: -1, Request: r}, nil
	})}
	if _, err := assistantDownloadContent(ctx, client, contentArgs(), dir); err == nil {
		t.Fatal("cancelled download published")
	}
	assertNoContent(t, dir)
	outside := contentTestDir(t)
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := assistantDownloadContent(context.Background(), contentHTTPFixture(testMOBI(), 128), contentArgs(), filepath.Join(dir, "escape")); err == nil {
		t.Fatal("followed symlink directory")
	}
	if err := os.Symlink(filepath.Join(outside, "sentinel"), filepath.Join(dir, "book.mobi")); err != nil {
		t.Fatal(err)
	}
	if _, err := assistantDownloadContent(context.Background(), contentHTTPFixture(testMOBI(), 128), contentArgs(), dir); err == nil {
		t.Fatal("followed destination symlink")
	}
	assertNoContent(t, outside)
}
