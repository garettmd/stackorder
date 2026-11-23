//go:build integration

package runs_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/stackorder/stackorder/api/v1"
	"github.com/stackorder/stackorder/internal/gh"
	"github.com/stackorder/stackorder/internal/principal"
	"github.com/stackorder/stackorder/internal/report"
)

func TestOptedInStackPreviewRecordsMiddleLayer(t *testing.T) {
	cfg := baseConfig()
	cfg.Apply.Mode, cfg.Apply.StackedPRs = v1.ApplyOnMerge, true
	e := newEnv(t, cfg)
	e.gh.SetContents(repoName, mainSHA, "stackorder.yaml", []byte("version: 1\napply:\n  mode: on_merge\n  stacked_prs: true\n"))
	branch := gh.PullBranch{Repo: &gh.Repository{FullName: repoName}}
	trunk := gh.BranchRef{Ref: "main", SHA: mainSHA}
	pulls := []gh.PullRequest{
		{Number: 6, State: gh.IssueOpen, Head: branch, Base: branch, HeadRef: "lower", HeadSHA: baseSHA, BaseRef: "main", BaseSHA: mainSHA},
		{Number: 7, State: gh.IssueOpen, Head: branch, Base: branch, HeadRef: "middle", HeadSHA: headSHA, BaseRef: "lower", BaseSHA: baseSHA,
			Stack: &gh.PullRequestStack{Number: 12, Position: 2, Base: trunk}},
		{Number: 8, State: gh.IssueOpen, Head: branch, Base: branch, HeadRef: "upper", HeadSHA: newHeadSHA, BaseRef: "middle", BaseSHA: headSHA},
	}
	for _, pull := range pulls {
		e.gh.SetPull(repoName, pull)
	}
	e.gh.SetPullStack(repoName, gh.PullStack{Number: 12, Base: trunk, Open: true, Pulls: pulls})
	e.gh.SetGitCommit(repoName, gh.GitCommit{SHA: headSHA, Tree: gh.GitObject{SHA: mergeSHA}})

	job := e.planJob(7)
	created, err := e.svc.CreateRun(e.ctx, job.p, v1.CreateRunRequest{
		Repo: repoName, PRNumber: 7, SHA: headSHA, SupportsStackPreview: true,
	})
	require.NoError(t, err)
	scope, err := e.st.PRPlanScope(e.ctx, repoID, 7)
	require.NoError(t, err)
	require.NotNil(t, scope.Preview)
	assert.Equal(t, []v1.StackLayer{{Number: 6, HeadSHA: baseSHA}, {Number: 7, HeadSHA: headSHA}}, scope.Preview.Layers)
	assert.Equal(t, mergeSHA, scope.Preview.TreeSHA)
	require.NotNil(t, created.Run)
	assert.Equal(t, scope.Preview, created.Run.StackPreview)

	upload := v1.GraphUploadRequest{Graph: testGraph(headSHA), BaseSHA: mainSHA, ChangedPaths: []string{vpc + "/main.tf"}}
	_, err = e.svc.UploadGraph(e.ctx, job.p, created.RunID, upload)
	require.ErrorContains(t, err, "verified GitHub tree")
	upload.CheckoutTree = mergeSHA
	_, err = e.svc.UploadGraph(e.ctx, job.p, created.RunID, upload)
	require.NoError(t, err)

	e.gh.SetRef(repoName, "heads/main", newHeadSHA)
	_, err = e.svc.UploadGraph(e.ctx, job.p, created.RunID, upload)
	require.ErrorIs(t, err, principal.ErrSuperseded)
	assert.Empty(t, e.gh.Dispatches())
}

func TestStackPreviewReplacesSameHeadPlanAndBlocksApply(t *testing.T) {
	e := newEnv(t, baseConfig())
	old := e.planned(7, headSHA)
	check := e.check(report.CheckPlan)
	require.Equal(t, gh.ConclusionSuccess, check.Conclusion)

	previous := e.planJob(7)
	previous.p.Claims.RunAttempt = "2"
	pending, err := e.svc.CreateRun(e.ctx, previous.p, v1.CreateRunRequest{Repo: repoName, SHA: headSHA, PRNumber: 7})
	require.NoError(t, err)
	_, err = e.svc.UploadGraph(e.ctx, previous.p, pending.RunID, v1.GraphUploadRequest{
		Graph: testGraph(headSHA), ChangedPaths: []string{vpc + "/main.tf"},
	})
	require.NoError(t, err)
	check = e.check(report.CheckPlan)

	pr := gh.PullRequest{Number: 7, HeadSHA: headSHA, BaseSHA: baseSHA, BaseRef: "lower",
		State: gh.IssueOpen, User: gh.User{Login: author}, Mergeable: ptr(true),
		Stack: &gh.PullRequestStack{Number: 12, Position: 2, Base: gh.BranchRef{Ref: "main", SHA: mainSHA}}}
	attached := e.gh.PullRequestEvent("stacked", repoName, pr)
	require.NoError(t, e.svc.HandlePullRequest(e.ctx, attached))
	blocked := e.check(report.CheckPlan)
	assert.Equal(t, check.ID, blocked.ID)
	assert.Equal(t, gh.ConclusionActionRequired, blocked.Conclusion)
	assert.Contains(t, e.sticky(7), "Cumulative preview")
	_, err = e.svc.RecordResult(e.ctx, previous.p, pending.RunID, vpc, planResult(vpc, headSHA, 1))
	require.NoError(t, err)
	assert.Equal(t, gh.ConclusionActionRequired, e.check(report.CheckPlan).Conclusion)
	e.comment(7, applier, "stackorder apply "+vpc)
	assert.Contains(t, e.lastComment(7), "preview-only")
	assert.Empty(t, e.locks())
	assert.Empty(t, e.gh.Dispatches())
	_, err = e.svc.Rerun(e.ctx, applier, old)
	require.ErrorIs(t, err, principal.ErrSuperseded)

	job := e.planJob(7)
	req := v1.CreateRunRequest{Repo: repoName, SHA: headSHA, PRNumber: 7, Mode: v1.ModePlan, BaseSHA: baseSHA}
	_, err = e.svc.CreateRun(e.ctx, job.p, req)
	require.ErrorContains(t, err, "upgrade the Stackorder CLI")
	req.SupportsStackPreview = true
	preview, err := e.svc.CreateRun(e.ctx, job.p, req)
	require.NoError(t, err)
	assert.Equal(t, mainSHA, preview.VerifiedBaseSHA)

	_, err = e.svc.UploadGraph(e.ctx, job.p, preview.RunID, v1.GraphUploadRequest{Graph: testGraph(headSHA), BaseSHA: baseSHA})
	require.ErrorContains(t, err, "verified plan base")
	upload := v1.GraphUploadRequest{Graph: testGraph(headSHA), BaseSHA: mainSHA, ChangedPaths: []string{vpc + "/main.tf", "stacks/dev/other/main.tf"}}
	resolved, err := e.svc.UploadGraph(e.ctx, job.p, preview.RunID, upload)
	require.NoError(t, err)
	for _, affected := range resolved.Affected {
		_, err := e.svc.RecordResult(e.ctx, job.p, preview.RunID, affected.Key, planResult(affected.Key, headSHA, 1))
		require.NoError(t, err)
	}
	assert.Equal(t, gh.ConclusionActionRequired, e.check(report.CheckPlan).Conclusion)
	assert.Equal(t, gh.ConclusionSuccess, e.check(report.StackCheckName(report.CheckPlan, vpc)).Conclusion)

	pr.Stack, pr.BaseRef, pr.BaseSHA = nil, "main", mainSHA
	require.NoError(t, e.svc.HandlePullRequest(e.ctx, e.gh.PullRequestEvent("edited", repoName, pr)))
	_, err = e.svc.Rerun(e.ctx, applier, preview.RunID)
	require.ErrorIs(t, err, principal.ErrSuperseded)
	e.comment(7, applier, "stackorder apply")
	assert.Empty(t, e.locks())

	fresh, err := e.svc.CreateRun(e.ctx, job.p, req)
	require.NoError(t, err)
	assert.NotEqual(t, preview.RunID, fresh.RunID)
	// A redelivered membership event uses current GitHub metadata.
	require.NoError(t, e.svc.HandlePullRequest(e.ctx, attached))
	resolved, err = e.svc.UploadGraph(e.ctx, job.p, fresh.RunID, upload)
	require.NoError(t, err)
	for _, affected := range resolved.Affected {
		_, err := e.svc.RecordResult(e.ctx, job.p, fresh.RunID, affected.Key, planResult(affected.Key, headSHA, 1))
		require.NoError(t, err)
	}
	assert.Equal(t, gh.ConclusionSuccess, e.check(report.CheckPlan).Conclusion)
}

func TestStackedMergeEventsDoNotApply(t *testing.T) {
	cfg := baseConfig()
	cfg.Apply.Mode = v1.ApplyOnMerge
	e := newEnv(t, cfg)
	stack := &gh.PullRequestStack{Number: 1, Position: 1, Base: gh.BranchRef{Ref: "main", SHA: mainSHA}}
	for _, number := range []int{8, 7, 8, 7} {
		pr := gh.PullRequest{Number: number, Merged: true, State: gh.IssueClosed, HeadSHA: headSHA,
			BaseRef: "main", BaseSHA: baseSHA, MergeCommitSHA: mergeSHA, Stack: stack}
		ev := e.gh.PullRequestEvent("closed", repoName, pr)
		require.NoError(t, e.svc.HandlePullRequest(e.ctx, ev))
	}
	assert.Empty(t, e.gh.Dispatches())
	assert.Empty(t, e.locks())
	assert.Len(t, e.comments(7), 1)
	assert.Len(t, e.comments(8), 1)
}

func TestJoiningStackBlocksAlreadyDispatchedApply(t *testing.T) {
	e := newEnv(t, baseConfig())
	e.planned(7, headSHA)
	e.comment(7, applier, "stackorder apply")
	apply := e.applyRun(7)
	dispatches := e.gh.Dispatches()
	require.NotEmpty(t, dispatches)
	dispatch := dispatches[0]
	env := entries(t, dispatch)[0].Environment
	pr := gh.PullRequest{Number: 7, HeadSHA: headSHA, BaseSHA: baseSHA, BaseRef: "main",
		Stack: &gh.PullRequestStack{Number: 4, Position: 1, Base: gh.BranchRef{Ref: "main", SHA: baseSHA}}}
	// The live check also protects the dispatch before the membership webhook arrives.
	e.gh.SetPull(repoName, pr)
	_, err := e.svc.GetRunForPrincipal(e.ctx, e.dispatchJob(dispatch.RunID, env), apply.ID)
	require.ErrorIs(t, err, principal.ErrForbidden)
	assert.ErrorContains(t, err, "preview-only")
}

func TestStackPreviewWithNoChangesOrFailedPlan(t *testing.T) {
	e := newEnv(t, baseConfig())
	for _, tc := range []struct {
		pr      int
		head    string
		changed []string
		want    string
	}{
		{7, headSHA, []string{"README.md"}, gh.ConclusionActionRequired},
		{8, newHeadSHA, []string{vpc + "/main.tf"}, gh.ConclusionFailure},
	} {
		pr := gh.PullRequest{Number: tc.pr, HeadSHA: tc.head, BaseSHA: baseSHA, BaseRef: "main",
			Stack: &gh.PullRequestStack{Number: tc.pr, Position: 1, Base: gh.BranchRef{Ref: "main", SHA: baseSHA}}}
		e.gh.SetPull(repoName, pr)
		job := e.planJob(tc.pr)
		plan, err := e.svc.CreateRun(e.ctx, job.p, v1.CreateRunRequest{
			Repo: repoName, SHA: tc.head, PRNumber: tc.pr, SupportsStackPreview: true,
		})
		require.NoError(t, err)
		resolved, err := e.svc.UploadGraph(e.ctx, job.p, plan.RunID, v1.GraphUploadRequest{
			Graph: testGraph(tc.head), BaseSHA: baseSHA, ChangedPaths: tc.changed,
		})
		require.NoError(t, err)
		for _, affected := range resolved.Affected {
			res := planResult(affected.Key, tc.head, 1)
			res.Status, res.ExitCode, res.ErrorText = v1.ResultFailure, 1, "Error: failed to plan"
			_, err := e.svc.RecordResult(e.ctx, job.p, plan.RunID, affected.Key, res)
			require.NoError(t, err)
		}
		assert.Equal(t, tc.want, e.check(report.CheckPlan).Conclusion)
	}
	assert.Empty(t, e.checksNamed(report.CheckApply))
}
