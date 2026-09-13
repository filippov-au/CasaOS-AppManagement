package service

import (
	"context"
	stdjson "encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func assistantNZBTestServer(t *testing.T, result func(string) interface{}) *assistantMediaTarget {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string `json:"method"`
		}
		if err := stdjson.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid RPC", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = stdjson.NewEncoder(w).Encode(map[string]interface{}{"result": result(request.Method), "error": nil})
	}))
	t.Cleanup(server.Close)
	return &assistantMediaTarget{kind: "nzbget", base: server.URL}
}

func TestAssistantNZBVerifyRequiresActiveSettings(t *testing.T) {
	for _, stale := range []bool{true, false} {
		m := assistantNZBTestServer(t, func(method string) interface{} {
			value := "/data/completed"
			if method == "config" && stale {
				value = "/old/downloads"
			}
			if method == "status" {
				return map[string]interface{}{}
			}
			return []assistantNZBOption{{Name: "DestDir", Value: value}}
		})
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		err := assistantNZBVerify(ctx, m, []assistantNZBOption{{Name: "DestDir", Value: "/data/completed"}}, map[string]string{"DestDir": "/data/completed"})
		cancel()
		if (err != nil) != stale {
			t.Fatalf("stale active settings=%v: %v", stale, err)
		}
	}
}

func TestAssistantNZBDirectoryChangePreservesQueueAndRejectsQueuedJobs(t *testing.T) {
	m := assistantNZBTestServer(t, func(method string) interface{} {
		if method == "config" {
			return []assistantNZBOption{{Name: "QueueDir", Value: "/downloads/queue/"}}
		}
		return []map[string]interface{}{{"NZBID": 1, "Status": "PAUSED"}}
	})
	current := []assistantNZBOption{{Name: "MainDir", Value: "/downloads"}, {Name: "QueueDir", Value: "${MainDir}/queue"}}
	changes := map[string]string{"MainDir": "/data/downloads"}
	next, err := assistantNZBMerge(current, changes)
	if err != nil {
		t.Fatal(err)
	}
	verify := map[string]string{}
	if err = assistantNZBPinQueue(context.Background(), m, current, next, changes, verify); err != nil {
		t.Fatal(err)
	}
	if next[1].Value != "/downloads/queue/" || current[1].Value != "${MainDir}/queue" || verify["QueueDir"] != "/downloads/queue/" {
		t.Fatal("queue/history directory moved")
	}
	if err = assistantNZBDirectoryChange(context.Background(), m, current, changes); err == nil || !strings.Contains(err.Error(), "queued") {
		t.Fatal("allowed paths to change with queued downloads", err)
	}
}
