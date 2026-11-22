package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/paingha/tugcli/internal/intelligence"
	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/resolve"
	"github.com/paingha/tugcli/internal/version"
)

// deepRun is what --deep needs before the cluster is read: the settings
// and the API key.
type deepRun struct {
	settings intelligence.Settings
	key      string
	// keySource says where the key came from, for an error about it.
	keySource string
	yes       bool
}

// deepEstimate is what tug expected a call to take, before sending it.
type deepEstimate struct {
	InputTokens int `json:"inputTokens"`
	// CostLowUSD and CostHighUSD are nil when the model has no price.
	CostLowUSD  *float64 `json:"costLowUSD"`
	CostHighUSD *float64 `json:"costHighUSD"`
}

// deepOutput is what --deep adds to why's JSON and YAML.
type deepOutput struct {
	// Sent is false when nothing was sent: the target was healthy, or the
	// question was declined.
	Sent        bool                      `json:"sent"`
	Provider    string                    `json:"provider"`
	Model       string                    `json:"model"`
	Estimate    *deepEstimate             `json:"estimate,omitempty"`
	Evidence    *intelligence.Evidence    `json:"evidence,omitempty"`
	Explanation *intelligence.Explanation `json:"explanation,omitempty"`
}

// deepView is a diagnosis with what --deep added, so the diagnosis keeps
// the fields it has without --deep.
type deepView struct {
	*ops.Diagnosis
	Deep deepOutput `json:"deep"`
}

// prepareDeep checks, before the cluster is read, that --deep can run: it
// is turned on, runs on one cluster, and has an API key. It returns nil
// without --deep.
func (a *app) prepareDeep(o whyOptions) (*deepRun, error) {
	if !o.deep {
		if o.provider != "" || o.model != "" {
			return nil, errors.New("--provider and --model choose the model --deep asks; add --deep")
		}
		return nil, nil
	}
	if a.fixed != nil {
		return nil, errors.New("--deep explains one object on one cluster at a time; leave out --cluster-selector")
	}
	s, err := intelligence.SettingsFrom(a.cfg.Intelligence, intelligence.Overrides{Provider: o.provider, Model: o.model})
	if err != nil {
		return nil, err
	}
	if !s.Enabled {
		return nil, intelligence.ErrDisabled
	}
	key, source, err := a.intelligenceKey(s.Provider)
	if err != nil {
		return nil, err
	}
	if key == "" && intelligence.KeyRequired(s.Provider) {
		return nil, fmt.Errorf("--deep needs an API key for %s; run `tug intelligence key set %s`, or set %s",
			s.Provider, s.Provider, intelligence.KeyEnv(s.Provider))
	}
	return &deepRun{settings: s, key: key, keySource: source, yes: o.yes}, nil
}

// whyDeep shows a diagnosis with an AI explanation of it: it collects the
// evidence, shows it with what the call is expected to cost, asks before
// sending it, and then shows the answer and what the call took.
func (a *app) whyDeep(ctx context.Context, env *opsEnv, clients ops.Clients, obj resolve.Object, d *ops.Diagnosis, format render.Format, run *deepRun) error {
	table := format == render.FormatTable
	if table {
		if err := a.printWhy(d); err != nil {
			return err
		}
	}
	out := deepOutput{Provider: run.settings.Provider, Model: run.settings.Model}
	if d.Healthy {
		fmt.Fprintf(a.stderr, "%s is healthy, so --deep sent nothing.\n", d.Target)
	} else if err := a.explain(ctx, env, clients, obj, d, run, &out); err != nil {
		return err
	}
	if !table {
		return render.Structured(a.stdout, format, deepView{Diagnosis: d, Deep: out})
	}
	if out.Explanation == nil {
		return nil
	}
	return a.printDeepExplanation(out.Evidence, out.Explanation)
}

// explain collects the evidence, shows it on stderr and, once agreed and
// within the spend cap, sends it and records the call in the ledger.
func (a *app) explain(ctx context.Context, env *opsEnv, clients ops.Clients, obj resolve.Object, d *ops.Diagnosis, run *deepRun, out *deepOutput) error {
	s := run.settings
	ev, err := intelligence.Collect(ctx, clients, obj, d, intelligence.CollectOptions{Cluster: env.session.State.ClusterName, Now: a.now})
	if err != nil {
		return err
	}
	out.Evidence = ev
	prompt := intelligence.NewPrompt(ev)
	tokens := prompt.EstimateTokens()
	price, priced := s.Prices.For(s.Model)
	out.Estimate = &deepEstimate{InputTokens: tokens}
	ledger := intelligence.OpenLedger(a.paths.Data, a.now)
	calls, err := ledger.Calls()
	if err != nil {
		return err
	}
	spent := intelligence.MonthSpend(calls, a.now())

	client := intelligence.NewClient(s, run.key, intelligence.WithUserAgent("tug/"+version.String()), intelligence.WithClock(a.now))
	fmt.Fprintf(a.stderr, "\n--deep will send this to %s, model %s at %s, after tug's standing guidance and before its closing manner:\n\n%s\n",
		s.Provider, s.Model, client.Endpoint(), prompt.Shown())
	estimate := fmt.Sprintf("About %s input tokens", tokenCount(tokens))
	var high float64
	if priced {
		low, hi := price.Estimate(tokens, s.MaxOutputTokens)
		high = hi
		out.Estimate.CostLowUSD, out.Estimate.CostHighUSD = &low, &hi
		estimate += fmt.Sprintf("; estimated cost %s to %s with up to %s output tokens", dollars(low), dollars(hi), tokenCount(s.MaxOutputTokens))
	} else {
		estimate += fmt.Sprintf("; cost unknown, as %s has no price (add one with `tug settings add intelligence.prices model=%s input=<$ per million> output=<$ per million>`)", s.Model, s.Model)
	}
	fmt.Fprintln(a.stderr, estimate+".")
	if ev.Redactions > 0 {
		fmt.Fprintf(a.stderr, "%d values were replaced by placeholders.\n", ev.Redactions)
	}
	month := fmt.Sprintf("This month (UTC): %s spent", dollars(spent.CostUSD))
	if s.MonthlySpendCapUSD > 0 {
		month += " of a " + dollars(s.MonthlySpendCapUSD) + " cap"
	}
	fmt.Fprintln(a.stderr, month+".")
	if err := intelligence.CheckCap(s.MonthlySpendCapUSD, spent, high, priced); err != nil {
		return err
	}

	if !run.yes {
		if !a.interactive() {
			return fmt.Errorf("--deep sends the evidence above to %s; without a terminal, pass --yes to agree", s.Provider)
		}
		if !a.confirm(fmt.Sprintf("Send this to %s?", s.Provider), false) {
			fmt.Fprintln(a.stderr, "Nothing was sent.")
			return nil
		}
	}

	fmt.Fprintf(a.stderr, "Asking %s…\n", s.Model)
	x, err := client.Explain(ctx, ev)
	if x != nil {
		out.Sent = true
		a.reportCall(x.Call)
		if rerr := ledger.Record(x.Call); rerr != nil {
			fmt.Fprintf(a.stderr, "%s the call was not recorded in the usage ledger: %v\n", a.colors().Yellow("warning:"), rerr)
		}
	}
	if apiErr := (*intelligence.APIError)(nil); errors.As(err, &apiErr) && apiErr.Unauthorized() {
		if strings.HasPrefix(run.keySource, "TUG_SECRET_") {
			return fmt.Errorf("%s rejected the API key in %s: %w", s.Provider, run.keySource, err)
		}
		return fmt.Errorf("%s rejected the stored API key; replace it with `tug intelligence key set %s`: %w", s.Provider, s.Provider, err)
	}
	if err != nil {
		return err
	}
	out.Explanation = x
	for _, r := range x.Removed {
		fmt.Fprintf(a.stderr, "%s tug removed %s\n", a.colors().Yellow("warning:"), r)
	}
	return nil
}

// reportCall says on stderr what a call took.
func (a *app) reportCall(c intelligence.Call) {
	tokens := fmt.Sprintf("%s input", tokenCount(c.InputTokens))
	if c.CacheReadTokens > 0 {
		tokens += fmt.Sprintf(" + %s from cache", tokenCount(c.CacheReadTokens))
	}
	tokens += fmt.Sprintf(", %s output tokens", tokenCount(c.OutputTokens))
	cost := "cost unknown"
	if c.CostUSD != nil {
		cost = dollars(*c.CostUSD)
	}
	line := fmt.Sprintf("%s %s: %s, %s, %.1fs", c.Provider, c.Model, tokens, cost, float64(c.LatencyMS)/1000)
	if c.RequestID != "" {
		line += ", request " + c.RequestID
	}
	fmt.Fprintln(a.stderr, a.colors().Dim(line))
}

// printDeepExplanation writes an explanation after the diagnosis it explains,
// with the evidence it cites.
func (a *app) printDeepExplanation(ev *intelligence.Evidence, x *intelligence.Explanation) error {
	c := a.colors()
	w := a.stdout
	width := render.Width(w)
	cites := func(ids []string) string {
		if len(ids) == 0 {
			return ""
		}
		return " " + c.Dim("["+strings.Join(ids, ", ")+"]")
	}
	fmt.Fprintf(w, "\n%s %s\n", c.Bold("Explanation by "+x.Call.Model), c.Dim("(AI-generated: check the evidence it cites)"))
	writeIndented(w, "", "", x.Summary, width)
	if x.LikelyCause != "" {
		writeIndented(w, c.Bold("Likely cause:")+" ", "              ", x.LikelyCause, width)
	}

	if len(x.Facts) > 0 {
		fmt.Fprintf(w, "\n%s\n", c.Bold("Facts:"))
		for _, f := range x.Facts {
			writeIndented(w, "  - ", "    ", f.Statement+cites(f.Evidence), width)
		}
	}
	if len(x.Hypotheses) > 0 {
		fmt.Fprintf(w, "\n%s\n", c.Bold("Hypotheses:"))
		for _, h := range x.Hypotheses {
			writeIndented(w, "  - ", "    ", h.Confidence+": "+h.Statement+cites(h.Evidence), width)
			if h.WouldConfirm != "" {
				writeIndented(w, "    ", "    ", c.Dim("would confirm:")+" "+inlineCommands(c, h.WouldConfirm), width)
			}
		}
	}
	steps := func(title string, list []intelligence.Step) {
		if len(list) == 0 {
			return
		}
		fmt.Fprintf(w, "\n%s\n", c.Bold(title))
		for i, s := range list {
			label := fmt.Sprintf("  %d. ", i+1)
			under := strings.Repeat(" ", len(label))
			if s.Description != "" {
				writeIndented(w, label, under, inlineCommands(c, s.Description), width)
				label = under
			}
			if s.Command != "" {
				fmt.Fprintf(w, "%s%s\n", label, c.Cyan(s.Command))
			}
		}
	}
	steps("Next steps (tug keeps only commands that read):", x.NextSteps)
	steps("Changes that need your approval (tug runs none of them):", x.ChangesNeedingApproval)
	if len(x.InstructionsInEvidence) > 0 {
		fmt.Fprintf(w, "\n%s\n", c.Yellow("Text in the evidence that reads like instructions, ignored:"))
		for _, n := range x.InstructionsInEvidence {
			writeIndented(w, "  - ", "    ", n.Statement+cites(n.Evidence), width)
		}
	}

	var cited []string
	for _, cl := range slices.Concat(x.Facts, x.Hypotheses, x.InstructionsInEvidence) {
		for _, id := range cl.Evidence {
			if !slices.Contains(cited, id) {
				cited = append(cited, id)
			}
		}
	}
	if len(cited) == 0 {
		return nil
	}
	fmt.Fprintf(w, "\n%s\n", c.Bold("Evidence cited:"))
	for _, id := range cited {
		if it, ok := ev.Item(id); ok {
			writeIndented(w, fmt.Sprintf("  %-4s ", id), "       ", it.Kind+": "+it.Source, width)
		}
	}
	return nil
}
