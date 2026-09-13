package assistant

import _ "embed"

// Embedded at build time and included for every conversation, including restored
// history. Content suitability is model reasoning, not a server-side classifier.
//
//go:embed instructions/app-content.md
var appContentInstructions string

var systemPrompt = baseSystemPrompt + "\n\n" + appContentInstructions
