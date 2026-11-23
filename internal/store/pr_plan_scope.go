package store

import (
	"context"

	"github.com/google/uuid"

	v1 "github.com/stackorder/stackorder/api/v1"
)

type PRPlanScope struct {
	Generation uuid.UUID        `db:"generation"`
	HeadSHA    string           `db:"head_sha"`
	ParentRef  string           `db:"parent_ref"`
	ParentSHA  string           `db:"parent_sha"`
	Preview    *v1.StackPreview `db:"preview"`
	CheckID    int64            `db:"check_id"`
}

func (s *Store) PRPlanScope(ctx context.Context, repoID int64, pr int) (PRPlanScope, error) {
	scope, err := queryOne[PRPlanScope](ctx, s.db, `
		SELECT generation, head_sha, parent_ref, parent_sha, preview, check_id
		FROM pr_plan_scopes WHERE repo_id = $1 AND pr_number = $2`, repoID, pr)
	return scope, wrap("get pull request plan scope", err)
}

func (s *Store) SetPRPlanScope(ctx context.Context, repoID int64, pr int, scope PRPlanScope) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO pr_plan_scopes (repo_id, pr_number, generation, head_sha, parent_ref, parent_sha, preview, check_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (repo_id, pr_number) DO UPDATE
		SET generation = excluded.generation, head_sha = excluded.head_sha, parent_ref = excluded.parent_ref,
		    parent_sha = excluded.parent_sha, preview = excluded.preview, check_id = excluded.check_id`,
		repoID, pr, scope.Generation, scope.HeadSHA, scope.ParentRef, scope.ParentSHA, scope.Preview, scope.CheckID)
	return wrap("set pull request plan scope", err)
}
