package assistant

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestProviderRoutingAndToolReasoning(t *testing.T) {
	for _, preset := range Presets {
		t.Run(preset.ID, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != preset.Endpoint || r.Header.Get("Authorization") != "Bearer key" || r.Header.Get("x-opencode-session") != "session123" || r.Header.Get("User-Agent") != "CasaOS-Assistant/1.0" {
					t.Fatal("incorrect provider routing or credentials")
				}
				var body struct {
					Messages []Message `json:"messages"`
				}
				if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
					t.Fatal(e)
				}
				if body.Messages[0].Reasoning != "preserve reasoning" {
					t.Fatal("lost reasoning needed for tool continuation")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"thinking","tool_calls":[{"id":"x","type":"function","function":{"name":"inspect","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`))}, nil
			})}
			msg, e := (ChatModel{Client: client}).Complete(context.Background(), Settings{Provider: preset.ID, Model: preset.Models[0], APIKey: "key"}, "session123", []Message{{Role: "assistant", Reasoning: "preserve reasoning"}}, nil)
			if e != nil || len(msg.Calls) != 1 || msg.Reasoning != "thinking" {
				t.Fatal(msg, e)
			}
		})
	}
}
func TestProviderDoesNotExposeErrorBody(t *testing.T) {
	model := ChatModel{Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("secret response body"))}, nil
	})}}
	_, e := model.Complete(context.Background(), testSettings(), "id", nil, nil)
	if e == nil || strings.Contains(e.Error(), "secret response body") {
		t.Fatal(e)
	}
}
