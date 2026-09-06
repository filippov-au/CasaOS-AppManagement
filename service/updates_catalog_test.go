package service

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/IceWhaleTech/CasaOS-AppManagement/pkg/config"
	"github.com/IceWhaleTech/CasaOS-Common/utils/logger"
)

func TestManualUpdateCheckRefreshesEqualSizedCatalog(t *testing.T) {
	logger.LogInitConsoleOnly()
	oldPath := config.AppInfo.AppStorePath
	config.AppInfo.AppStorePath = t.TempDir()
	t.Cleanup(func() { config.AppInfo.AppStorePath = oldPath })
	archive := func(tag string) []byte {
		var data bytes.Buffer
		writer := zip.NewWriter(&data)
		file, err := writer.CreateHeader(&zip.FileHeader{Name: "Apps/demo/docker-compose.yml", Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		_, err = fmt.Fprintf(file, "name: demo\nservices:\n  web:\n    image: busybox:%s\nx-casaos:\n  main: web\n", tag)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return data.Bytes()
	}
	first, second := archive("1.36"), archive("1.37")
	if len(first) != len(second) {
		t.Fatal("fixture archives must have equal size")
	}
	var current atomic.Value
	current.Store(first)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := current.Load().([]byte)
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.Header().Set("Content-Type", "application/zip")
		if r.Method != "HEAD" {
			_, _ = w.Write(data)
		}
	}))
	defer server.Close()
	store := &appStore{url: server.URL + "/store.zip"}
	if err := store.UpdateCatalog(); err != nil {
		t.Fatal(err)
	}
	current.Store(second)
	if err := store.refreshCatalog(true); err != nil {
		t.Fatal(err)
	}
	app, err := store.ComposeApp("demo")
	if err != nil {
		t.Fatal(err)
	}
	if app == nil || app.Services[0].Image != "busybox:1.37" {
		t.Fatal("manual check used stale catalog")
	}
}
