package runs

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"

	v1 "github.com/stackorder/stackorder/api/v1"
	"github.com/stackorder/stackorder/internal/config"
	"github.com/stackorder/stackorder/internal/gh"
	"github.com/stackorder/stackorder/internal/principal"
	"github.com/stackorder/stackorder/internal/report"
	"github.com/stackorder/stackorder/internal/store"
)

const (
	stackPreviewOnly = "GitHub stacked pull requests are preview-only. To deploy this layer, detach it, target the repository default branch with the intended changes, and run a fresh standalone plan."
	stackReplan      = "The pull request plan scope changed. Rerun the full Stackorder plan workflow in GitHub Actions."
	stackRetarget    = "Retarget this detached pull request to the repository default branch and rerun the full Stackorder plan workflow."
)

func previewScope(pull *gh.PullRequest) *v1.StackPreview {
	if pull.Stack == nil {
		return nil
	}
	return &v1.StackPreview{Number: pull.Stack.Number, Position: pull.Stack.Position,
		BaseRef: pull.Stack.Base.Ref, BaseSHA: pull.Stack.Base.SHA, ParentSHA: pull.BaseSHA}
}

func samePreview(a, b *v1.StackPreview) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Number == b.Number && a.Position == b.Position && a.BaseRef == b.BaseRef &&
		a.BaseSHA == b.BaseSHA && a.ParentSHA == b.ParentSHA && a.TreeSHA == b.TreeSHA && slices.Equal(a.Layers, b.Layers)
}

func sameGeneration(a, b *uuid.UUID) bool {
	return a == b || a != nil && b != nil && *a == *b
}

func prScope(ctx context.Context, db *store.Store, repoID int64, pr int) (store.PRPlanScope, error) {
	scope, err := db.PRPlanScope(ctx, repoID, pr)
	if errors.Is(err, store.ErrNotFound) {
		return scope, nil
	}
	return scope, err
}

func currentPlanScope(scope store.PRPlanScope, generation *uuid.UUID, head string) bool {
	if scope.Generation == uuid.Nil {
		return generation == nil
	}
	return scope.HeadSHA == head && sameGeneration(&scope.Generation, generation)
}

func scopeMatches(scope store.PRPlanScope, pull *gh.PullRequest, preview *v1.StackPreview) bool {
	if scope.Generation == uuid.Nil {
		return pull.Stack == nil
	}
	return scope.HeadSHA == pull.HeadSHA && scope.ParentRef == pull.BaseRef &&
		scope.ParentSHA == pull.BaseSHA && samePreview(scope.Preview, preview)
}

// liveStackPreview reads the exact prefix only when the default branch opts in.
func liveStackPreview(ctx context.Context, c *gh.Client, repo store.Repo, pull *gh.PullRequest) (*v1.StackPreview, error) {
	preview := previewScope(pull)
	if preview == nil || !repoConfig(repo).Apply.StackedPRs || preview.BaseRef != repo.DefaultBranch || preview.BaseSHA == "" {
		return preview, nil
	}
	head, err := c.GetRef(ctx, repo.FullName, "heads/"+repo.DefaultBranch)
	if err != nil || head != preview.BaseSHA {
		return preview, err
	}
	body, err := c.GetContents(ctx, repo.FullName, config.RootFile, head)
	if errors.Is(err, gh.ErrNotFound) {
		return preview, nil
	}
	if err != nil {
		return nil, err
	}
	cfg, err := config.Parse(body)
	if err != nil || !cfg.Apply.StackedPRs {
		return preview, nil
	}
	stack, err := c.GetPullStack(ctx, repo.FullName, preview.Number)
	if err != nil {
		return nil, err
	}
	prefix := stack.OpenPrefix(pull)
	if len(prefix) == 0 {
		return preview, nil
	}
	commit, err := c.GetGitCommit(ctx, repo.FullName, pull.HeadSHA)
	if err != nil {
		return nil, err
	}
	if commit.SHA == pull.HeadSHA && commit.Tree.SHA != "" {
		preview.TreeSHA = commit.Tree.SHA
		for _, layer := range prefix {
			preview.Layers = append(preview.Layers, v1.StackLayer{Number: layer.Number, HeadSHA: layer.HeadSHA})
		}
	}
	return preview, nil
}

// syncPRScope runs under the PR's reporting lock so webhook, registration
// and rendering cannot publish checks for different scopes out of order.
func (s *Service) syncPRScope(ctx context.Context, tx *store.Store, c *gh.Client, repo store.Repo, pull *gh.PullRequest) (store.PRPlanScope, bool, error) {
	scope, err := prScope(ctx, tx, repo.ID, pull.Number)
	if err != nil {
		return scope, false, err
	}
	preview, err := liveStackPreview(ctx, c, repo, pull)
	if err != nil || scopeMatches(scope, pull, preview) {
		return scope, false, err
	}
	if scope.HeadSHA != pull.HeadSHA {
		scope.CheckID = 0
	}
	scope.Generation = uuid.New()
	scope.HeadSHA, scope.ParentRef, scope.ParentSHA = pull.HeadSHA, pull.BaseRef, pull.BaseSHA
	scope.Preview = preview
	// Adopt the existing rollup on the same commit when a PR joins a stack.
	if scope.CheckID == 0 {
		rows, _, err := tx.ListRuns(ctx, store.RunFilter{RepoID: repo.ID, PRNumber: pull.Number, SHA: pull.HeadSHA, Mode: v1.ModePlan})
		if err != nil {
			return scope, false, err
		}
		for _, row := range rows {
			if scope.CheckID = row.CheckRuns[report.CheckPlan]; scope.CheckID > 0 {
				break
			}
		}
	}
	return scope, true, tx.SetPRPlanScope(ctx, repo.ID, pull.Number, scope)
}

func scopeRollup(check report.CheckOutput, preview *v1.StackPreview) report.CheckOutput {
	if preview == nil {
		if check.Conclusion == "" || check.Conclusion == report.ConclusionNeutral {
			check.Title = "Fresh plan required"
			check.Status, check.Conclusion = report.StatusCompleted, report.ConclusionActionRequired
		}
		return check
	}
	check.Summary = report.StackPreviewNotice(preview) + "\n\n" + stackPreviewOnly + "\n\n" + check.Summary
	if check.Conclusion != report.ConclusionFailure {
		check.Title = "GitHub stack preview · action required"
		check.Status, check.Conclusion = report.StatusCompleted, report.ConclusionActionRequired
	}
	return check
}

func (s *Service) writeScopeCheck(ctx context.Context, w *checkWriter, pr int, scope store.PRPlanScope, run store.Run, out report.CheckOutput) error {
	if scope.Preview == nil && scope.ParentRef != w.repo.DefaultBranch {
		out.Summary = stackRetarget + "\n\n" + out.Summary
		if out.Conclusion != report.ConclusionFailure {
			out.Title = "Retarget to the default branch"
			out.Status, out.Conclusion = report.StatusCompleted, report.ConclusionActionRequired
		}
	}
	if scope.Preview == nil && out.Conclusion == report.ConclusionSuccess && repoConfig(w.repo).Apply.Mode != v1.ApplyOnMerge {
		if err := w.blockEarlierApplyChecks(ctx, pr, scope); err != nil {
			return err
		}
	}
	p := w.params(&run, report.CheckPlan, out)
	p.HeadSHA = scope.HeadSHA
	p.ExternalID = fmt.Sprintf("stackorder-plan:%d:%d:%s", w.repo.ID, pr, scope.HeadSHA)
	if run.ID == uuid.Nil {
		p.DetailsURL = ""
	}
	if scope.CheckID != 0 {
		_, err := w.c.UpdateCheckRun(ctx, w.repo.FullName, scope.CheckID, p)
		if err == nil || !errors.Is(err, gh.ErrEditRefused) {
			return err
		}
	}
	check, err := w.c.CreateCheckRun(ctx, w.repo.FullName, p)
	if errors.Is(err, gh.ErrUnknownOutcome) {
		checks, lookupErr := w.c.ListCheckRunsForRef(ctx, w.repo.FullName, scope.HeadSHA, report.CheckPlan)
		if lookupErr != nil {
			return errors.Join(err, lookupErr)
		}
		for _, existing := range checks {
			if existing.ExternalID == p.ExternalID {
				check, err = &existing, nil
				break
			}
		}
	}
	if err != nil {
		return err
	}
	scope.CheckID = check.ID
	return w.tx.SetPRPlanScope(ctx, w.repo.ID, pr, scope)
}

// A same-head standalone plan cannot inherit an apply check from before
// the PR joined the stack.
func (w *checkWriter) blockEarlierApplyChecks(ctx context.Context, pr int, scope store.PRPlanScope) error {
	filter := store.RunFilter{RepoID: w.repo.ID, PRNumber: pr, SHA: scope.HeadSHA, Limit: 500}
	for {
		rows, next, err := w.tx.ListRuns(ctx, filter)
		if err != nil {
			return err
		}
		for _, row := range rows {
			id := row.CheckRuns[report.CheckApply]
			if id == 0 || sameGeneration(&scope.Generation, row.PlanGeneration) {
				continue
			}
			params := gh.CheckRunParams{Status: gh.CheckRunCompleted, Conclusion: gh.ConclusionActionRequired,
				CompletedAt: w.s.now(), Output: gh.CheckRunOutput{Title: "Fresh apply required", Summary: "The pull request plan scope changed."}}
			if _, err := w.c.UpdateCheckRun(ctx, w.repo.FullName, id, params); err != nil && !errors.Is(err, gh.ErrEditRefused) {
				return err
			}
		}
		if next == "" {
			return nil
		}
		filter.Cursor = next
	}
}

func (s *Service) publishScopeChange(ctx context.Context, tx *store.Store, c *gh.Client, repo store.Repo, pr int, scope store.PRPlanScope) error {
	w := &checkWriter{s: s, tx: tx, c: c, repo: repo}
	out := report.CheckOutput{Title: "Fresh plan required", Summary: stackReplan,
		Status: report.StatusCompleted, Conclusion: report.ConclusionActionRequired}
	return s.writeScopeCheck(ctx, w, pr, scope, store.Run{}, scopeRollup(out, scope.Preview))
}

func (s *Service) syncPRScopeEvent(ctx context.Context, repo store.Repo, pr int) error {
	c, err := s.client(ctx, repo)
	if err != nil {
		return err
	}
	var publishErr error
	run := store.Run{RepoID: repo.ID, PRNumber: pr}
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		if err := tx.LockKey(ctx, renderKey(run)); err != nil {
			return err
		}
		pull, err := c.GetPull(ctx, repo.FullName, pr)
		if err != nil || pull.State != gh.IssueOpen || pull.IsFork() {
			return err
		}
		scope, _, err := s.syncPRScope(ctx, tx, c, repo, pull)
		if err != nil || scope.Generation == uuid.Nil {
			return err
		}
		w := &checkWriter{s: s, tx: tx, c: c, repo: repo}
		view, err := planViewAt(ctx, tx, repo.ID, pr, scope.HeadSHA)
		if err != nil {
			return err
		}
		opts, err := s.reportOptions(ctx, tx, repo, pr)
		if err != nil {
			return err
		}
		if len(view.runs) == 0 {
			publishErr = s.publishScopeChange(ctx, tx, c, repo, pr, scope)
		} else {
			publishErr = s.writeScopeCheck(ctx, w, pr, scope, view.runs[0], scopeRollup(report.RollupCheck(view.v1(), opts), scope.Preview))
		}
		stickyErr := s.renderSticky(ctx, w, repo, pr, opts)
		publishErr = errors.Join(publishErr, stickyErr, errors.Join(w.errs...))
		return nil
	})
	return errors.Join(err, publishErr)
}

func (s *Service) checkApplyScope(ctx context.Context, c *gh.Client, repo store.Repo, run store.Run) error {
	if run.Mode != v1.ModeApply || run.PRNumber == 0 {
		return nil
	}
	pull, err := c.GetPull(ctx, repo.FullName, run.PRNumber)
	if err != nil {
		return err
	}
	if pull.Stack != nil {
		return principal.Wrap(principal.ErrForbidden, "%s", stackPreviewOnly)
	}
	scope, err := prScope(ctx, s.st, repo.ID, run.PRNumber)
	if err != nil {
		return err
	}
	if scope.Generation != uuid.Nil && pull.BaseRef != repo.DefaultBranch {
		return principal.Wrap(principal.ErrForbidden, "%s", stackRetarget)
	}
	if !applyScopeMatches(scope, pull, run) {
		return principal.Wrap(principal.ErrSuperseded, "%s", stackReplan)
	}
	return nil
}

func applyScopeMatches(scope store.PRPlanScope, pull *gh.PullRequest, run store.Run) bool {
	if scope.Generation == uuid.Nil {
		return pull.Stack == nil && run.PlanGeneration == nil
	}
	if !sameGeneration(&scope.Generation, run.PlanGeneration) {
		return false
	}
	if run.Trigger == v1.TriggerPullRequest && pull.Merged {
		// The signed merge event already checked the pre-merge base. GitHub
		// can now resolve that branch to the merge commit itself.
		return pull.Stack == nil && scope.Preview == nil && pull.HeadSHA == scope.HeadSHA
	}
	return scopeMatches(scope, pull, previewScope(pull))
}

// A native group merge can deliver several closed events. Each PR gets one
// sticky notice, and none can become an independent deployment or graph write.
func (s *Service) skipStackedMerge(ctx context.Context, repo store.Repo, pull *gh.PullRequest) (bool, error) {
	scope, err := prScope(ctx, s.st, repo.ID, pull.Number)
	if err != nil {
		return false, err
	}
	c, err := s.client(ctx, repo)
	if err != nil {
		return false, err
	}
	if pull.Stack == nil && scope.Preview == nil {
		live, err := c.GetPull(ctx, repo.FullName, pull.Number)
		if err != nil || live.Stack == nil {
			return false, err
		}
	}
	const marker = "<!-- stackorder:stacked-merge -->"
	body := marker + "\nStackorder did not start an apply for this GitHub stacked merge. Review the deployed state and any existing Stackorder locks before recovery."
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		if err := tx.LockKey(ctx, renderKey(store.Run{RepoID: repo.ID, PRNumber: pull.Number})); err != nil {
			return err
		}
		_, err := c.UpsertStickyComment(ctx, repo.FullName, pull.Number, marker, body)
		return err
	})
	return true, err
}
