package cli

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/stackorder/stackorder/api/v1"
	"github.com/stackorder/stackorder/internal/client"
	"github.com/stackorder/stackorder/internal/config"
)

const forkNote = "Fork pull requests get a read-only GITHUB_TOKEN and no OIDC token, so Stackorder cannot reach its server or the cloud role from them; nothing is planned. A maintainer can re-run the change from a branch in this repository."

type resolveOptions struct {
	base   string
	stacks []string
}

func (a *app) resolveCommand() *cobra.Command {
	var o resolveOptions
	cmd := &cobra.Command{
		Use:   "resolve",
		Short: "Scan the repository, resolve the affected stacks with the server and write the plan matrix",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runResolve(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.base, "base", "", "base ref to diff against (default: the pull request base, else the merge base with origin/<default branch>)")
	f.StringSliceVar(&o.stacks, "stacks", nil, "restrict the run to these stack keys, comma separated")
	return cmd
}

type scanResult struct {
	gh      *Context
	cfg     *v1.RepoConfig
	graph   *v1.Graph
	changed []string
	base    string
}

func (a *app) scan(ctx context.Context, withChanges bool, base string) (*scanResult, error) {
	gh, err := a.github(ctx)
	if err != nil {
		return nil, err
	}
	cfg, _, err := config.Load(a.root)
	if err != nil {
		return nil, failed("loading %s: %w", config.RootFile, err)
	}
	g, err := scanRepo(ctx, a.root, gh.Repository, gh.SHA, cfg)
	if err != nil {
		return nil, failed("scanning %s: %w", a.root, err)
	}
	for _, w := range g.Warnings {
		a.warn(w)
	}
	res := &scanResult{gh: gh, cfg: cfg, graph: g}
	if !withChanges {
		return res, nil
	}
	if res.base, err = a.baseRef(ctx, gh, base); err != nil {
		return nil, failed("%w", err)
	}
	if res.changed, err = changedPaths(ctx, a.root, res.base, "HEAD"); err != nil {
		return nil, failed("listing the paths changed since %s: %w", res.base, err)
	}
	a.log.Debug("changed paths", "base", res.base, "count", len(res.changed))
	return res, nil
}

func (a *app) runResolve(ctx context.Context, o resolveOptions) error {
	if err := a.requireFormat(false); err != nil {
		return err
	}
	gh, err := a.github(ctx)
	if err != nil {
		return err
	}
	if gh.CI && gh.IsFork {
		return a.resolveFork(ctx, gh)
	}
	if gh.PushToDefault() {
		return a.recordDefaultBranchGraph(ctx, gh)
	}
	cl, run, reason, err := a.registerResolve(ctx, gh, o.base)
	if err != nil {
		return err
	}
	base, preview := "", gh.StackPreview
	if run != nil && run.VerifiedBaseSHA != "" {
		base = run.VerifiedBaseSHA
		if run.Run == nil {
			return failed("the server did not return the pull request plan scope")
		}
		preview = run.Run.StackPreview
	} else if preview != nil {
		if run != nil {
			return failed("the server does not support GitHub stack previews; upgrade it before planning this PR")
		}
		base = preview.BaseSHA
	}
	if preview != nil {
		if err := a.verifyPreviewCheckout(ctx, gh.SHA, preview); err != nil {
			return err
		}
	}
	if base != "" {
		if o.base != "" && a.baseSHA(ctx, o.base) != base {
			return failed("--base must match the verified base commit %s", base)
		}
		o.base = base
	}
	sr, err := a.scan(ctx, true, o.base)
	if err != nil {
		return err
	}
	requested := normalizeKeys(o.stacks)
	var resp *v1.ResolveResponse
	if run != nil {
		upload := v1.GraphUploadRequest{
			Graph: *sr.graph, ChangedPaths: sr.changed, BaseSHA: a.baseSHA(ctx, sr.base), Config: sr.cfg, Stacks: requested,
		}
		if preview != nil {
			upload.CheckoutTree = preview.TreeSHA
		}
		resp, err = cl.UploadGraph(ctx, run.RunID, upload)
		if isUnreachable(err) {
			reason = "the server is unreachable: " + err.Error()
		} else if err != nil {
			return fmt.Errorf("resolving with the server: %w", err)
		}
		if resp != nil && resp.RunID == "" {
			resp.RunID = run.RunID
		}
	}
	if resp == nil {
		a.log.Info("resolving locally", "reason", reason)
		if resp, err = a.resolveLocally(sr, requested); err != nil {
			return err
		}
	}
	if len(resp.Cycles) > 0 {
		return a.cycleError(resp)
	}
	if preview != nil && resp.Unconfirmed {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("Cumulative preview for GitHub stack #%d, layer %d, against %s at %s. Stackorder applies are disabled while the PR is in the stack.", preview.Number, preview.Position, preview.BaseRef, shortRef(preview.BaseSHA)))
	}
	note := ""
	if resp.Unconfirmed && gh.CI {
		note = "Unconfirmed: " + reason + ". The affected set was computed locally; plans run, applies are refused until the server confirms a plan of this commit."
		a.warn("the resolution is unconfirmed: " + reason)
		a.neutralCheck(ctx, gh, "stackorder/resolve", resolveTitle(resp), resolveMarkdown(resp, note))
	}
	for _, w := range resp.Warnings {
		a.warn(w)
	}
	if err := a.resolveOutputs(resp); err != nil {
		return err
	}
	a.stepSummary(resolveMarkdown(resp, note))
	return a.printResolve(resp)
}

func (a *app) recordDefaultBranchGraph(ctx context.Context, gh *Context) error {
	sr, err := a.scan(ctx, false, "")
	if err != nil {
		return err
	}
	if a.server == "" {
		a.log.Info("no server is configured; the default-branch graph is not recorded")
		return a.emitRecord(gh, nil)
	}
	cl, err := a.newClient(gh, false)
	if err != nil {
		return failed("%w", err)
	}
	resp, err := cl.RecordDefaultGraph(ctx, gh.Repository, v1.RecordGraphRequest{Graph: *sr.graph})
	switch {
	case err == nil:
		if resp.Recorded {
			a.log.Info("recorded the default-branch graph", "sha", resp.SHA)
		} else {
			a.log.Info("the default-branch graph was not recorded", "sha", resp.SHA, "reason", resp.Reason)
		}
		return a.emitRecord(gh, resp)
	case isUnreachable(err):
		a.warn("the server is unreachable; the default-branch graph is not recorded: " + err.Error())
		return a.emitRecord(gh, nil)
	}
	return failed("recording the default-branch graph: %w", err)
}

func (a *app) emitRecord(gh *Context, resp *v1.RecordGraphResponse) error {
	if resp == nil {
		resp = &v1.RecordGraphResponse{SHA: gh.SHA}
	}
	a.stepSummary(recordMarkdown(resp))
	if a.format == formatJSON {
		return a.writeJSON(resp)
	}
	_, err := fmt.Fprintln(a.stdout, recordLine(resp))
	return err
}

func recordLine(resp *v1.RecordGraphResponse) string {
	if resp.Recorded {
		return "recorded the default-branch graph at " + shortRef(resp.SHA)
	}
	if resp.Reason != "" {
		return "default-branch graph not recorded: " + resp.Reason
	}
	return "default-branch graph not recorded"
}

func recordMarkdown(resp *v1.RecordGraphResponse) string {
	return "### Default-branch graph\n\n" + recordLine(resp) + "\n"
}

func shortRef(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func (a *app) registerResolve(ctx context.Context, gh *Context, base string) (*client.Client, *v1.CreateRunResponse, string, error) {
	if a.server == "" {
		return nil, nil, "no server is configured", nil
	}
	if !gh.CI {
		return nil, nil, "outside GitHub Actions the server takes graphs only from pull request resolve jobs", nil
	}
	cl, err := a.newClient(gh, false)
	if err != nil {
		return nil, nil, "", failed("%w", err)
	}
	ref, err := a.baseRef(ctx, gh, base)
	if err != nil {
		return nil, nil, "", failed("%w", err)
	}
	run, err := cl.CreateRun(ctx, v1.CreateRunRequest{
		Repo: gh.Repository, SHA: gh.SHA, BaseSHA: a.baseSHA(ctx, ref), PRNumber: gh.PRNumber, Mode: v1.ModePlan,
		Trigger: gh.Trigger(), WorkflowRunID: gh.RunID, Attempt: gh.RunAttempt, RunID: gh.DispatchRunID,
		SupportsStackPreview: true,
	})
	switch {
	case err == nil:
		return cl, run, "", nil
	case isUnreachable(err):
		return nil, nil, "the server is unreachable: " + err.Error(), nil
	}
	return nil, nil, "", fmt.Errorf("resolving with the server: %w", err)
}

func (a *app) verifyPreviewCheckout(ctx context.Context, head string, scope *v1.StackPreview) error {
	checkedOut, err := revParse(ctx, a.root, "HEAD")
	if err != nil || !hexSHA.MatchString(head) || checkedOut != head {
		return failed("check out pull request head %s before planning this GitHub stack", head)
	}
	if scope.TreeSHA != "" {
		tree, err := gitOutput(ctx, a.root, "rev-parse", "--verify", "HEAD^{tree}")
		if err != nil || tree != scope.TreeSHA {
			return failed("the checkout tree does not match GitHub's verified tree for %s", shortRef(head))
		}
	}
	for _, base := range []string{scope.BaseSHA, scope.ParentSHA} {
		if !hexSHA.MatchString(base) {
			return failed("GitHub did not provide a valid stack base or parent commit; rerun the full plan workflow")
		}
		if _, err := gitOutput(ctx, a.root, "merge-base", "--is-ancestor", base, head); err != nil {
			return failed("stack base or parent %s is missing from the pull request head's ancestry; fetch full history, rebase the GitHub stack, and rerun the plan", shortRef(base))
		}
	}
	parent := scope.BaseSHA
	for _, layer := range scope.Layers {
		if !hexSHA.MatchString(layer.HeadSHA) || layer.HeadSHA == parent {
			return failed("GitHub did not provide a valid head for stacked pull request #%d", layer.Number)
		}
		if _, err := gitOutput(ctx, a.root, "merge-base", "--is-ancestor", parent, layer.HeadSHA); err != nil {
			return failed("the head of stacked pull request #%d does not descend from %s; rebase the GitHub stack and rerun the plan", layer.Number, shortRef(parent))
		}
		parent = layer.HeadSHA
	}
	if len(scope.Layers) > 0 && parent != head {
		return failed("the verified GitHub stack does not end at the checked out head")
	}
	return nil
}

func (a *app) resolveLocally(sr *scanResult, requested []string) (*v1.ResolveResponse, error) {
	resp, err := resolveLocal(sr.graph, sr.changed, sr.cfg, requested)
	if resp != nil && len(resp.Cycles) > 0 {
		resp.Unconfirmed = true
		return resp, nil
	}
	if err != nil {
		return nil, failed("resolving locally: %w", err)
	}
	resp.RunID = ""
	resp.Unconfirmed = true
	return resp, nil
}

func (a *app) cycleError(resp *v1.ResolveResponse) error {
	lines := cycleLines(resp.Cycles)
	if a.format == formatJSON {
		_ = a.writeJSON(resp)
	} else {
		for _, l := range lines {
			_, _ = fmt.Fprintln(a.stdout, l)
		}
	}
	return &ExitError{Code: ExitFailure, Err: fmt.Errorf("dependency cycle: %s", strings.Join(lines, "; "))}
}

func (a *app) resolveFork(ctx context.Context, gh *Context) error {
	a.annotate("notice", forkNote)
	a.log.Info("fork pull request; nothing is planned")
	a.neutralCheck(ctx, gh, "stackorder/resolve", "Fork pull request: not planned", forkNote)
	resp := &v1.ResolveResponse{
		Affected:    []v1.AffectedStack{},
		Waves:       [][]string{},
		Matrix:      v1.Matrix{Include: []v1.MatrixEntry{}},
		Unconfirmed: true,
	}
	if err := a.resolveOutputs(resp); err != nil {
		return err
	}
	a.stepSummary(resolveMarkdown(resp, forkNote))
	return a.printResolve(resp)
}

func (a *app) resolveOutputs(resp *v1.ResolveResponse) error {
	matrix := resp.Matrix
	if matrix.Include == nil {
		matrix.Include = []v1.MatrixEntry{}
	}
	waves := resp.Waves
	if waves == nil {
		waves = [][]string{}
	}
	keys := make([]string, 0, len(resp.Affected))
	for _, s := range sortedAffected(resp) {
		keys = append(keys, s.Key)
	}
	return a.setOutputs(
		output{"run-id", resp.RunID},
		output{"matrix", jsonLine(matrix)},
		output{"waves", jsonLine(waves)},
		output{"affected", jsonLine(keys)},
		output{"count", strconv.Itoa(len(resp.Affected))},
		output{"unconfirmed", strconv.FormatBool(resp.Unconfirmed)},
	)
}

func (a *app) printResolve(resp *v1.ResolveResponse) error {
	if a.format == formatJSON {
		return a.writeJSON(resp)
	}
	if resp.RunID != "" {
		if _, err := fmt.Fprintf(a.stdout, "run %s\n", resp.RunID); err != nil {
			return err
		}
	}
	return writeAffectedTable(a.stdout, resp)
}

func resolveTitle(resp *v1.ResolveResponse) string {
	switch n := len(resp.Affected); n {
	case 0:
		return "Unconfirmed: no stacks affected"
	case 1:
		return "Unconfirmed: 1 stack affected"
	default:
		return fmt.Sprintf("Unconfirmed: %d stacks affected", n)
	}
}

func normalizeKeys(keys []string) []string {
	var out []string
	for _, k := range keys {
		if k = config.NormalizePath(strings.TrimSpace(k)); k != "" && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}
