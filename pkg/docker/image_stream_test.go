package docker

import (
	"bytes"
	"strings"
	"testing"
)

func TestPullStreamDetectsDaemonErrors(t *testing.T) {
	for _, progress := range []bool{false, true} {
		for _, tc := range []struct {
			stream string
			fail   bool
		}{
			{`{"status":"Downloading"}` + "\n" + `{"status":"Download complete"}`, false},
			{`{"status":"Downloading"}` + "\n" + `{"error":"disk full"}`, true},
			{`{"status":`, true},
		} {
			var output bytes.Buffer
			var err error
			if progress {
				err = consumePullMessages(strings.NewReader(tc.stream), &output)
			} else {
				err = consumePullMessages(strings.NewReader(tc.stream), nil)
			}
			if (err != nil) != tc.fail {
				t.Fatalf("stream %q: %v", tc.stream, err)
			}
		}
	}
}
