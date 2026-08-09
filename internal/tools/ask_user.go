package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// AskFunc is the seam the ask_user tool reaches the human through. It is defined
// here (not in internal/tui) to break the import cycle — internal/agent imports
// internal/tools, and the tool must be constructed alongside every other tool —
// exactly as BlackboardStore and permissions.ApprovalFunc are seams the concrete
// TUI (or a test stub) fills. In `fuse shell` the implementation sends an
// AskQuestionMsg onto the model channel and blocks until the user answers; in
// one-shot / non-interactive mode it is nil and the tool reports that no human
// is reachable.
//
// The call is deliberately unbounded on the human side (a person may take
// arbitrarily long to answer); only ctx cancellation aborts it.
type AskFunc func(ctx context.Context, q Question) (Answer, error)

// Question is one structured, multiple-choice question posed to the human. It
// mirrors Claude Code's AskUserQuestion contract closely enough to reuse the
// same prompting habits: a short header chip, the full question text, and 2–4
// mutually-exclusive options (unless MultiSelect). "Other / free text" is always
// offered by the UI, so the model never has to include an escape-hatch option.
type Question struct {
	// Header is a <=12-char chip shown as a tag (e.g. "Auth", "DB driver").
	Header string `json:"header"`
	// Question is the full, specific question ending in a question mark.
	Question string `json:"question"`
	// Options are the 2–4 concrete choices. Each is a distinct, mutually
	// exclusive answer unless MultiSelect is set.
	Options []Option `json:"options"`
	// MultiSelect lets the human pick more than one option.
	MultiSelect bool `json:"multiSelect"`
}

// Option is one selectable choice with a short label and an explanatory line.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Answer is the human's decision, carried back to the tool (and thence to the
// model as the tool result). Selected holds the chosen option labels in display
// order; FreeText holds the user's custom "Other" input when they bypassed the
// listed options; Chat holds a prose reply the human gave via "Chat about this"
// (a steer/clarification rather than a pick — ADR-0021 Phase 1 reply-to-asker).
// A cancelled prompt sets Cancelled so the model learns the human declined
// rather than treating an empty answer as a choice.
type Answer struct {
	Selected  []string `json:"selected"`
	FreeText  string   `json:"free_text,omitempty"`
	Chat      string   `json:"chat,omitempty"`
	Cancelled bool     `json:"cancelled,omitempty"`
}

// maxOptions bounds the choice list: more than a handful stops being a quick
// pick and should be decomposed into narrower questions (matches Claude Code's
// 2–4 range, with a little slack).
const maxOptions = 6

// NewAskUserTool returns the ask_user tool wired to ask. A nil ask means no
// human is reachable (one-shot / scripted mode); the tool then returns a plain
// error result telling the model to proceed on its own best judgment rather than
// blocking forever on a prompt nobody will see.
func NewAskUserTool(ask AskFunc) Tool { return &askUserTool{ask: ask} }

type askUserTool struct{ ask AskFunc }

func (t *askUserTool) Name() string { return "ask_user" }

func (t *askUserTool) Description() string {
	// The description IS the prompt engineering: it teaches the model WHEN to
	// ask (only genuinely user-owned decisions) and HOW to shape a good question
	// (specific, mutually-exclusive options, a recommended-first ordering). Kept
	// terse but directive — this text is the only guidance the model gets.
	return strings.TrimSpace(`
Ask the human a structured multiple-choice question and block until they answer,
then continue with their decision recorded in context.

Use this ONLY for a decision that is genuinely the user's to make and that you
cannot resolve from the request, the code, or a sensible default — an ambiguous
requirement, a fork with real trade-offs, a preference you cannot infer. Do NOT
use it to ask permission to run a tool (that has its own approval flow), to
confirm you should proceed, or for anything you can settle yourself; pick the
obvious default and say so instead.

Provide 2–4 concrete, mutually exclusive options. Put your recommended option
first and append " (recommended)" to its label. Each option needs a one-line
description of what choosing it means or implies. A free-text "Other" choice is
always offered automatically, so never add one yourself. Set multiSelect only
when the choices are not mutually exclusive. The header is a <=12-char chip.

The answer is returned as JSON (the selected labels, or the user's free text)
and becomes part of the conversation, so you can rely on it in later turns.`)
}

func (t *askUserTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"header": map[string]any{
				"type":        "string",
				"description": "Short tag/chip for the question, at most 12 characters (e.g. \"Auth\", \"DB driver\").",
			},
			"question": map[string]any{
				"type":        "string",
				"description": "The full question, specific and ending in a question mark.",
			},
			"options": map[string]any{
				"type":        "array",
				"description": "2–4 mutually exclusive choices; recommended option first with \" (recommended)\" in its label.",
				"minItems":    2,
				"maxItems":    maxOptions,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"label":       map[string]any{"type": "string", "description": "Concise choice label (1–5 words)."},
						"description": map[string]any{"type": "string", "description": "One line on what this choice means or implies."},
					},
					"required": []string{"label", "description"},
				},
			},
			"multiSelect": map[string]any{
				"type":        "boolean",
				"description": "Allow selecting more than one option. Default false.",
			},
		},
		"required": []string{"question", "options"},
	}
}

func (t *askUserTool) Execute(ctx context.Context, args string) Result {
	var q Question
	if err := json.Unmarshal([]byte(args), &q); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("ask_user: invalid args: %v", err)}
	}
	if strings.TrimSpace(q.Question) == "" {
		return Result{IsError: true, Output: "ask_user: question is required"}
	}
	if len(q.Options) < 2 {
		return Result{IsError: true, Output: "ask_user: provide at least 2 options"}
	}
	if len(q.Options) > maxOptions {
		return Result{IsError: true, Output: fmt.Sprintf("ask_user: at most %d options (decompose into narrower questions)", maxOptions)}
	}
	if t.ask == nil {
		// No human is reachable (one-shot / scripted run). Don't wedge the loop on
		// a prompt nobody will answer — tell the model to decide for itself.
		return Result{IsError: true, Output: "ask_user: no interactive user is available in this session; proceed using your own best judgment and state the assumption you made"}
	}

	ans, err := t.ask(ctx, q)
	if err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("ask_user: %v", err)}
	}
	if ans.Cancelled {
		// A dismissed prompt is a real outcome, not an error the model should
		// retry — it means "you decide". Report it plainly.
		return Result{Output: "user dismissed the question without answering; proceed using your own best judgment and note the assumption you made"}
	}

	out, mErr := json.Marshal(ans)
	if mErr != nil {
		return Result{IsError: true, Output: fmt.Sprintf("ask_user: encode answer: %v", mErr)}
	}
	return Result{Output: string(out)}
}

var _ Tool = (*askUserTool)(nil)
