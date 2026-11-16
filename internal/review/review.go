// Package review: a model reads a gated change before it merges, as the last stage of the gate.
//
// The gate proves the code builds and its tests pass; it does not prove the change does what
// the task asked or that it is correct where the tests do not look. On the first day of real
// work every defect that mattered was found by a second reader, none by the gate. So a repo
// can name a reviewer (`review = "<provider>"`): after the gate's commands pass, that provider
// reads the task and the diff, read-only, and ends with VERDICT: approve or VERDICT: reject.
// A reject fails the gate with the reviewer's findings, which the next attempt reads like any
// gate failure. A review that could not run (no account, no verdict, a timeout) does not hold
// the work: it passes with a warning, which a person sees.
//
// An approval is remembered by the change's patch id and the task text, so a rebase that
// leaves the change as it was is not reviewed (and paid for) again.
package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/fortunto2/swarm/internal/gate"
	"github.com/fortunto2/swarm/internal/gitx"
)

// Task: what the change was asked to do.
type Task struct {
	ID                      int64
	Title, Spec, Acceptance string
}

// Runner: the reviewer. It reads prompt, may read files under dir, and returns its final text.
type Runner func(ctx context.Context, prompt, dir string) (string, error)

// Spec: one review.
type Spec struct {
	Task     Task
	Base     string // the diff is base...HEAD of Dir
	Dir      string // a checkout of the change
	Reviewer string // the provider's id, for the log and the cache
	Run      Runner
	Cache    string // directory of remembered approvals; "" remembers nothing
	Timeout  time.Duration
}

// maxDiff: the diff the prompt carries in full; past it the reviewer gets the stat and reads
// the files itself.
const maxDiff = 60_000

// Gate runs the review stage. OK when approved, or when the review could not run (with a
// warning); not OK, Stage "review", with the findings as Tail, when rejected.
func Gate(ctx context.Context, s Spec) gate.Verdict {
	diff, err := gitx.Run(ctx, s.Dir, "diff", "--no-color", "--no-ext-diff", s.Base+"...HEAD")
	if err != nil {
		return skipped("no diff: " + err.Error())
	}
	if strings.TrimSpace(diff) == "" {
		return gate.Verdict{OK: true, Stage: "review"}
	}
	key := cacheKey(ctx, s, diff)
	if s.Cache != "" && key != "" {
		if _, err := os.Stat(filepath.Join(s.Cache, key)); err == nil {
			return gate.Verdict{OK: true, Stage: "review", Reason: "approved before (same change, same task)"}
		}
	}
	stat, _ := gitx.Run(ctx, s.Dir, "diff", "--stat", "--no-color", s.Base+"...HEAD")
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = Timeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	text, err := s.Run(rctx, Prompt(s.Task, stat, diff), s.Dir)
	if err != nil {
		return skipped(fmt.Sprintf("%s: %v", s.Reviewer, err))
	}
	verdict, findings := Parse(text)
	switch verdict {
	case "approve":
		if s.Cache != "" && key != "" {
			_ = os.MkdirAll(s.Cache, 0o700)
			_ = os.WriteFile(filepath.Join(s.Cache, key), []byte(findings+"\n"), 0o600)
		}
		v := gate.Verdict{OK: true, Stage: "review", Reason: "approved by " + s.Reviewer}
		if findings != "" { // remarks that did not stop it: for a person, not a reason to redo
			v.Warnings = []string{"review notes (" + s.Reviewer + "): " + clip(findings, 1500)}
		}
		return v
	case "reject":
		return gate.Verdict{Stage: "review", Reason: "rejected by " + s.Reviewer, Tail: clip(findings, 4096)}
	}
	return skipped(s.Reviewer + " gave no VERDICT line")
}

func skipped(why string) gate.Verdict {
	return gate.Verdict{OK: true, Stage: "review", Reason: "not reviewed", Warnings: []string{"review did not run: " + why}}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n[cut]"
}

// cacheKey: the change's patch id (stable across a rebase that leaves it as it was) and the
// task text. "" when git cannot say.
func cacheKey(ctx context.Context, s Spec, diff string) string {
	cmd := exec.CommandContext(ctx, "git", "-C", s.Dir, "patch-id", "--stable")
	cmd.Stdin = strings.NewReader(diff)
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return ""
	}
	pid, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	h := sha256.Sum256([]byte(pid + "\x00" + s.Reviewer + "\x00" + s.Task.Title + "\x00" + s.Task.Spec + "\x00" + s.Task.Acceptance))
	return hex.EncodeToString(h[:16])
}

var verdictLine = regexp.MustCompile(`(?im)^\W*VERDICT\W*:\W*(approve|reject)\b`)

// Parse: the last VERDICT line decides; the text before it is the findings.
func Parse(text string) (verdict, findings string) {
	idx := verdictLine.FindAllStringSubmatchIndex(text, -1)
	if len(idx) == 0 {
		return "", strings.TrimSpace(text)
	}
	last := idx[len(idx)-1]
	return strings.ToLower(text[last[2]:last[3]]), strings.TrimSpace(text[:last[0]])
}

// Prompt: the task, the diff, and the rules of the verdict.
func Prompt(t Task, stat, diff string) string {
	body := diff
	if len(diff) > maxDiff {
		body = "(the diff is " + fmt.Sprint(len(diff)) + " characters; read the changed files listed above in the checkout)"
	}
	return fmt.Sprintf(`You review one change before it merges. You did not write it. Read it as its harshest
competent reader: the author's tests already pass, so look for what tests miss.

Task T%d: %s

What was asked:
%s

Acceptance:
%s

Changed files:
%s

Diff (base...change):
%s

The checkout of the change is your working directory: read any file you need. Do not edit
anything. Judge:
1. Does it do what was asked, all of it, and nothing the task did not ask for?
2. Correctness: inputs or states that give a wrong result, an error swallowed, a race, a
   resource left open, a boundary off by one. Only what you can point at in this diff.
3. Did a test get weaker, or does a new test pass without the change?

Reject only for a defect you can name with file:line and a concrete failing case, or for a
part of the task left undone. Style, naming and taste are not reasons. List each finding as
"file:line: what breaks, for which input". Then end with exactly one line:
VERDICT: approve
or
VERDICT: reject
`, t.ID, t.Title, t.Spec, t.Acceptance, stat, body)
}

// After runs the review once the gate's commands passed, keeping their warnings and flaky
// commands; a failed gate is returned as it was.
func After(ctx context.Context, v gate.Verdict, s Spec) gate.Verdict {
	if !v.OK {
		return v
	}
	rv := Gate(ctx, s)
	rv.Warnings = append(append([]string{}, v.Warnings...), rv.Warnings...)
	rv.Flaky = v.Flaky
	return rv
}

// Timeout: how long a review may take, added to a gate's limit when a repo has one.
const Timeout = 15 * time.Minute
