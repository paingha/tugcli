package intelligence

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// maxNextSteps is how many next steps an explanation keeps.
const maxNextSteps = 5

// Claim is a fact, a hypothesis or a note about the evidence, with the IDs
// of the evidence items it rests on.
type Claim struct {
	Statement string   `json:"statement"`
	Evidence  []string `json:"evidence"`
	// Confidence is low, medium or high, and WouldConfirm says what would
	// confirm or rule it out, for a hypothesis.
	Confidence   string `json:"confidence,omitempty"`
	WouldConfirm string `json:"wouldConfirm,omitempty"`
}

// Step is a suggested step. A next step's command only reads; a change's
// may not, and needs a person's approval. tug runs neither.
type Step struct {
	Description string `json:"description"`
	Command     string `json:"command,omitempty"`
}

// Explanation is a model's account of the evidence, as tug checked it,
// with what the call took.
type Explanation struct {
	Summary     string  `json:"summary"`
	LikelyCause string  `json:"likelyCause,omitempty"`
	Facts       []Claim `json:"facts"`
	Hypotheses  []Claim `json:"hypotheses"`
	NextSteps   []Step  `json:"nextSteps"`
	// ChangesNeedingApproval are changes that would fix the problem, for a
	// person to decide on.
	ChangesNeedingApproval []Step `json:"changesNeedingApproval"`
	// InstructionsInEvidence notes text in the evidence that reads like
	// instructions, which the model was told to ignore.
	InstructionsInEvidence []Claim `json:"instructionsInEvidence"`
	// Removed says what tug took out of the answer, or moved, and why.
	Removed []string `json:"removed,omitempty"`
	Call    Call     `json:"call"`
}

// answer is the JSON the model is asked for.
type answer struct {
	Summary                string  `json:"summary"`
	LikelyCause            string  `json:"likelyCause"`
	Facts                  []Claim `json:"facts"`
	Hypotheses             []Claim `json:"hypotheses"`
	NextSteps              []Step  `json:"nextSteps"`
	ChangesNeedingApproval []Step  `json:"changesNeedingApproval"`
	InstructionsInEvidence []Claim `json:"instructionsInEvidence"`
}

// Explain sends the evidence wrapped in tug's standing guidance, in one
// request retried only while the provider is busy or down, and returns the
// checked answer with the call's usage, cost and latency. When the model
// declines or its answer is cut off, it returns the error with an
// Explanation that holds only the call, which was still billed.
func (c *Client) Explain(ctx context.Context, ev *Evidence) (*Explanation, error) {
	p := NewPrompt(ev)
	start := c.now()
	r, err := c.complete(ctx, p)
	if err != nil {
		return nil, err
	}
	call := Call{
		Time: start.UTC(), Provider: c.s.Provider, Model: cmpOr(r.model, c.s.Model), Target: ev.Target,
		Usage: r.usage, LatencyMS: c.now().Sub(start).Milliseconds(), RequestID: r.requestID,
	}
	if price, ok := c.s.Prices.For(call.Model); ok {
		cost := price.Cost(r.usage)
		call.CostUSD = &cost
	}
	switch r.stop {
	case stopRefused:
		if r.category != "" {
			return &Explanation{Call: call}, fmt.Errorf("%w (%s)", ErrRefused, r.category)
		}
		return &Explanation{Call: call}, ErrRefused
	case stopTruncated:
		return &Explanation{Call: call}, ErrTruncated
	}
	var a answer
	dec := json.NewDecoder(strings.NewReader(stripFence(r.text)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return &Explanation{Call: call}, fmt.Errorf("reading the model's answer: %w", err)
	}
	x := check(ev, a)
	x.Call = call
	return x, nil
}

// stripFence takes a ```json fence off an answer, which a model held to a
// JSON object but not a schema may add.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimPrefix(s, "json")
	return strings.TrimSuffix(strings.TrimSpace(s), "```")
}

// check keeps what tug can stand behind in an answer:
//
//   - a fact must cite evidence that was sent, or it is shown as a
//     hypothesis, and citations of items that were not sent are dropped;
//   - a next step's command must only read: one that changes something
//     moves to the changes needing approval, and one tug would not show
//     at all (shell syntax, a Secret's data, a credential, a placeholder,
//     another program) loses its command;
//   - a command in the text that does not only read is removed, except in
//     the changes needing approval;
//   - items tug marked as reading like instructions are noted even when the
//     model did not note them.
func check(ev *Evidence, a answer) *Explanation {
	x := &Explanation{Facts: []Claim{}, Hypotheses: []Claim{}, NextSteps: []Step{}, ChangesNeedingApproval: []Step{}, InstructionsInEvidence: []Claim{}}
	unknown := map[string]bool{}
	cite := func(ids []string) []string {
		out := []string{}
		for _, id := range ids {
			id = strings.Trim(strings.TrimSpace(id), "[]")
			if _, ok := ev.Item(id); !ok {
				unknown[id] = true
				continue
			}
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
		return out
	}
	scrub := func(s string, readOnly bool) string {
		s, n := removeCommands(s, readOnly)
		if n > 0 {
			x.Removed = append(x.Removed, "a command in the text that does not only read")
		}
		return strings.TrimSpace(s)
	}

	x.Summary = scrub(a.Summary, true)
	x.LikelyCause = scrub(a.LikelyCause, true)
	var uncited []Claim
	for _, f := range a.Facts {
		c := Claim{Statement: scrub(f.Statement, true), Evidence: cite(f.Evidence)}
		switch {
		case c.Statement == "":
		case len(c.Evidence) == 0:
			c.Confidence = "low"
			uncited = append(uncited, c)
		default:
			x.Facts = append(x.Facts, c)
		}
	}
	if len(uncited) > 0 {
		x.Removed = append(x.Removed, fmt.Sprintf("%d facts that cited no evidence tug sent, now shown as hypotheses", len(uncited)))
	}
	for _, h := range a.Hypotheses {
		c := Claim{Statement: scrub(h.Statement, true), Evidence: cite(h.Evidence), Confidence: h.Confidence, WouldConfirm: scrub(h.WouldConfirm, true)}
		if !slices.Contains([]string{"low", "medium", "high"}, c.Confidence) {
			c.Confidence = "low"
		}
		if c.Statement != "" {
			x.Hypotheses = append(x.Hypotheses, c)
		}
	}
	x.Hypotheses = append(x.Hypotheses, uncited...)

	for _, s := range a.NextSteps {
		step := Step{Description: s.Description, Command: strings.Trim(strings.TrimSpace(s.Command), "`")}
		if step.Command != "" {
			if ok, why := Runnable(step.Command); !ok {
				x.Removed = append(x.Removed, "a suggested command, because "+why)
				step.Command = ""
			} else if ok, _ := ReadOnly(step.Command); !ok {
				step.Description = scrub(step.Description, false)
				x.ChangesNeedingApproval = append(x.ChangesNeedingApproval, step)
				x.Removed = append(x.Removed, "a next step that changes something, moved to the changes needing approval")
				continue
			}
		}
		step.Description = scrub(step.Description, true)
		if step.Description == "" && step.Command == "" {
			continue
		}
		if len(x.NextSteps) == maxNextSteps {
			x.Removed = append(x.Removed, fmt.Sprintf("next steps after the first %d", maxNextSteps))
			break
		}
		x.NextSteps = append(x.NextSteps, step)
	}
	for _, s := range a.ChangesNeedingApproval {
		step := Step{Description: scrub(s.Description, false), Command: strings.Trim(strings.TrimSpace(s.Command), "`")}
		if step.Command != "" {
			if ok, why := Runnable(step.Command); !ok {
				x.Removed = append(x.Removed, "a suggested change's command, because "+why)
				step.Command = ""
			}
		}
		if step.Description != "" || step.Command != "" {
			x.ChangesNeedingApproval = append(x.ChangesNeedingApproval, step)
		}
	}

	noted := map[string]bool{}
	for _, n := range a.InstructionsInEvidence {
		c := Claim{Statement: scrub(n.Statement, true), Evidence: cite(n.Evidence)}
		if c.Statement != "" {
			x.InstructionsInEvidence = append(x.InstructionsInEvidence, c)
			for _, id := range c.Evidence {
				noted[id] = true
			}
		}
	}
	var missed []string
	for _, id := range ev.WithInstructions() {
		if !noted[id] {
			missed = append(missed, id)
		}
	}
	if len(missed) > 0 {
		x.InstructionsInEvidence = append(x.InstructionsInEvidence, Claim{
			Statement: "tug found text that reads like instructions to a model; it was sent as data, and nothing in it was followed",
			Evidence:  missed,
		})
	}

	if len(unknown) > 0 {
		ids := make([]string, 0, len(unknown))
		for id := range unknown {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		x.Removed = append(x.Removed, "citations of evidence tug did not send: "+strings.Join(ids, ", "))
	}
	x.Removed = compact(x.Removed)
	return x
}

// removeCommands replaces each command in backticks that names kubectl or
// tug and is not one to show: with readOnly, any that does more than read;
// without, any tug would not show at all. It says how many it replaced.
func removeCommands(s string, readOnly bool) (string, int) {
	parts := strings.Split(s, "`")
	if len(parts) < 3 {
		return s, 0
	}
	check := Runnable
	if readOnly {
		check = ReadOnly
	}
	n := 0
	for i := 1; i < len(parts)-1; i += 2 {
		fields := strings.Fields(parts[i])
		if len(fields) == 0 || (fields[0] != "kubectl" && fields[0] != "tug") {
			continue
		}
		if ok, _ := check(parts[i]); !ok {
			parts[i] = "[command removed by tug]"
			n++
		}
	}
	return strings.Join(parts, "`"), n
}

// compact drops repeats, keeping the first of each and their order.
func compact(s []string) []string {
	var out []string
	for _, v := range s {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
