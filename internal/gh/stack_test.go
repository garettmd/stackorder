package gh

import (
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestGetPullStackAndGitTree(t *testing.T) {
	client, err := NewTokenClient(Config{HTTPClient: &http.Client{Transport: stackTestTransport(t)}}, "installation-token")
	if err != nil {
		t.Fatal(err)
	}
	stack, err := client.GetPullStack(t.Context(), "acme/infra", 12)
	if err != nil {
		t.Fatal(err)
	}
	if stack.Number != 12 || len(stack.Pulls) != 1 || stack.Pulls[0].HeadSHA != "head" || stack.Pulls[0].BaseSHA != "base" {
		t.Fatalf("unexpected stack: %+v", stack)
	}
	commit, err := client.GetGitCommit(t.Context(), "acme/infra", stack.Pulls[0].HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if commit.SHA != "head" || commit.Tree.SHA != "tree" || !slices.Equal(commit.Parents, []GitObject{{SHA: "base"}}) {
		t.Fatalf("unexpected commit: %+v", commit)
	}
}

type stackRoundTripFunc func(*http.Request) (*http.Response, error)

func (f stackRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func stackTestTransport(t *testing.T) stackRoundTripFunc {
	t.Helper()
	return func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/repos/acme/infra/stacks/12":
			if got := r.Header.Get("X-GitHub-Api-Version"); got != "2026-03-10" {
				t.Errorf("stack API version = %q", got)
			}
			body = `{"number":12,"base":{"ref":"main"},"open":true,"pull_requests":[{"number":101,"state":"open","head":{"ref":"feature","sha":"head"},"base":{"ref":"main","sha":"base"}}]}`
		case "/repos/acme/infra/git/commits/head":
			body = `{"sha":"head","tree":{"sha":"tree"},"parents":[{"sha":"base"}]}`
		default:
			t.Fatalf("unexpected request to %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}
}

func TestPullStackOpenPrefix(t *testing.T) {
	repo := &Repository{FullName: "acme/infra"}
	branch := PullBranch{Repo: repo}
	layers := []PullRequest{
		{Number: 101, State: IssueOpen, Head: branch, Base: branch, HeadRef: "first", HeadSHA: "first-sha", BaseRef: "main", BaseSHA: "trunk-sha"},
		{Number: 102, State: IssueOpen, Head: branch, Base: branch, HeadRef: "second", HeadSHA: "second-sha", BaseRef: "first", BaseSHA: "first-sha"},
		{Number: 103, State: IssueOpen, Head: branch, Base: branch, HeadRef: "third", HeadSHA: "third-sha", BaseRef: "second", BaseSHA: "second-sha"},
	}
	selected := layers[1]
	selected.Stack = &PullRequestStack{Number: 12, Position: 2, Base: BranchRef{Ref: "main", SHA: "trunk-sha"}}
	for _, tc := range []struct {
		name   string
		change func(*PullStack)
		want   []int
	}{
		{name: "middle layer", want: []int{101, 102}},
		{name: "upper layer changed", change: func(s *PullStack) { s.Pulls[2].HeadSHA = "new-third" }, want: []int{101, 102}},
		{name: "lower layer moved", change: func(s *PullStack) { s.Pulls[0].HeadSHA = "new-first" }},
		{name: "lower layer merged", change: func(s *PullStack) { s.Pulls[0].Merged = true }},
		{name: "lower layer detached", change: func(s *PullStack) { s.Pulls = s.Pulls[1:] }},
		{name: "stack retargeted", change: func(s *PullStack) { s.Base.Ref = "release" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stack := PullStack{Number: 12, Open: true, Base: BranchRef{Ref: "main"}, Pulls: slices.Clone(layers)}
			if tc.change != nil {
				tc.change(&stack)
			}
			var got []int
			for _, pull := range stack.OpenPrefix(&selected) {
				got = append(got, pull.Number)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("prefix = %v, want %v", got, tc.want)
			}
		})
	}
}
