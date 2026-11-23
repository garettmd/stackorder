CREATE TABLE pr_plan_scopes (
    repo_id bigint NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    pr_number integer NOT NULL,
    generation uuid NOT NULL,
    head_sha text NOT NULL,
    parent_ref text NOT NULL,
    parent_sha text NOT NULL,
    preview jsonb,
    check_id bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (repo_id, pr_number)
);

ALTER TABLE runs ADD COLUMN plan_generation uuid;
ALTER TABLE runs ADD COLUMN stack_preview jsonb;
