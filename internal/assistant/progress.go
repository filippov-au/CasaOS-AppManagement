package assistant

import "context"

type progressKey struct{}

// Progress is an observation for the chat UI, never a new model message.
func Progress(ctx context.Context, text string) {
	if report, ok := ctx.Value(progressKey{}).(func(string)); ok && ctx.Err() == nil {
		report(text)
	}
}

func toolProgressContext(ctx context.Context, s *session, tool string) context.Context {
	return context.WithValue(ctx, progressKey{}, func(text string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.view.Status != "running" {
			return
		}
		// Replace the current tool's progress instead of growing history per chunk.
		n := len(s.view.Events)
		if n > 0 && s.view.Events[n-1].Kind == "progress" && s.view.Events[n-1].Tool == tool {
			s.view.Events = s.view.Events[:n-1]
		}
		s.event("progress", text, tool)
	})
}
