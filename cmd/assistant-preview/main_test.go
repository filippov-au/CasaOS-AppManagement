package main

import (
	"context"
	"strings"
	"testing"
)

func TestPreviewCannotExposeOrExecuteDockerMutations(t *testing.T) {
	r := observationRuntime{}
	for _, tool := range r.Tools() {
		if r.IsWrite(tool.Function.Name) {
			t.Fatal("preview advertised a write tool", tool.Function.Name)
		}
	}
	checked := 0
	for _, tool := range r.AssistantRuntime.Tools() {
		if !r.IsWrite(tool.Function.Name) {
			continue
		}
		checked++
		if _, err := r.Prepare(context.Background(), tool.Function.Name, []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "only permits inspection") {
			t.Fatal("preview allowed a mutation", tool.Function.Name)
		}
	}
	if checked == 0 {
		t.Fatal("no mutation tools were checked")
	}
}
