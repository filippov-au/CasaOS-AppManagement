package assistant

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCatalogFiltersIncompatibleModelsAndPinsEndpoints(t *testing.T) {
	var source map[string]interface{}
	if err := json.Unmarshal(catalogSnapshot, &source); err != nil {
		t.Fatal(err)
	}
	p := source["deepseek"].(map[string]interface{})
	p["api"] = "https://untrusted.example/steal-keys"
	p["models"] = map[string]interface{}{
		"valid":           map[string]interface{}{"name": "Usable model", "tool_call": true, "limit": map[string]interface{}{"context": 128000}},
		"no-tools":        map[string]interface{}{"tool_call": false},
		"old":             map[string]interface{}{"tool_call": true, "status": "deprecated"},
		"responses":       map[string]interface{}{"tool_call": true, "provider": map[string]string{"npm": "@ai-sdk/openai"}},
		"anthropic":       map[string]interface{}{"tool_call": true, "provider": map[string]string{"npm": "@ai-sdk/anthropic"}},
		"other-reasoning": map[string]interface{}{"tool_call": true, "interleaved": map[string]string{"field": "reasoning_details"}},
		"redirect-model":  map[string]interface{}{"tool_call": true, "provider": map[string]string{"api": "https://untrusted.example"}},
	}
	data, _ := json.Marshal(source)
	result, err := parseCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(result[0].Models) != 1 || result[0].Models[0] != "valid" || result[0].Endpoint != adapters[0].Endpoint || result[0].ModelDetails[0].Context != 128000 {
		t.Fatalf("unexpected catalog: %+v", result[0])
	}
}
func TestCatalogRefreshCachesAndSurvivesUnavailableSource(t *testing.T) {
	for _, success := range []bool{true, false} {
		calls := 0
		c := &ModelCatalog{Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.String() != "https://models.dev/api.json" || r.Header.Get("Authorization") != "" {
				t.Fatal("catalog must not receive credentials")
			}
			body := string(catalogSnapshot)
			if !success {
				body = `{"unexpected":"schema"}`
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}}
		c.Refresh(context.Background())
		c.Refresh(context.Background())
		if calls != 1 || len(c.Current()) != 2 {
			t.Fatal("missing cache or offline fallback")
		}
		if strings.Contains(c.Source(), "bundled") == success {
			t.Fatal("incorrect catalog source status")
		}
	}
}
