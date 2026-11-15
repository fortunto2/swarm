// Package prompts renders the worker and lead prompts from embedded templates.
package prompts

import (
	"bytes"
	_ "embed"
	"text/template"
)

//go:embed worker.md
var workerSrc string

var worker = template.Must(template.New("worker").Parse(workerSrc))

type Worker struct {
	ID                      int64
	Worktree, Branch        string
	Title, Spec, Acceptance string
	Files, Previous, Gate   []string
	Rules                   string
	Provider                string // provider/model, for the commit trailer
	// GateElsewhere: the node the repo's suite runs on when it is not this attempt's machine
	// (gate_on names it): the worker runs only the static checks and its own tests here.
	GateElsewhere string
}

func RenderWorker(w Worker) (string, error) {
	var b bytes.Buffer
	err := worker.Execute(&b, w)
	return b.String(), err
}
