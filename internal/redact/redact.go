// Package redact runs the profile's redaction filter over text swarm stores or forwards
// outside Claude: gate output, worker notes, notifications. Claude's own hooks guard what
// a model sees; this guards the database, the next prompt and the operator's phone.
package redact

import (
	"bytes"
	"context"
	"os/exec"
	"time"
)

// Withheld replaces text whose filter failed: fail closed, never forward it unread.
const Withheld = "[redaction failed: text withheld]"

// Filter is a command reading text on stdin and writing it back masked (e.g.
// secret_guard.py filter). Empty: text passes through unchanged.
type Filter []string

func (f Filter) Apply(text string) string {
	if len(f) == 0 || text == "" {
		return text
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, f[0], f[1:]...)
	cmd.Stdin = bytes.NewBufferString(text)
	out, err := cmd.Output()
	if err != nil {
		return Withheld
	}
	return string(out)
}
