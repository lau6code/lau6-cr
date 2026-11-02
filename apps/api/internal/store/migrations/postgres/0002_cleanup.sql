-- lau6-cr 镜像清理策略（PostgreSQL 方言）
--
-- 与 SQLite 版结构一致，见 sqlite/0002_cleanup.sql 的注释。

CREATE TABLE IF NOT EXISTS retention_policies (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  scope_key    TEXT    NOT NULL UNIQUE,
  project_id   BIGINT REFERENCES projects(id) ON DELETE CASCADE,
  enabled      BIGINT NOT NULL DEFAULT 0 CHECK(enabled IN (0,1)),
  algorithm    TEXT    NOT NULL DEFAULT 'or' CHECK(algorithm IN ('or')),
  rules        TEXT    NOT NULL DEFAULT '[]',
  tag_selector TEXT    NOT NULL DEFAULT '**',
  created_at   BIGINT NOT NULL,
  updated_at   BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_retention_policies_project ON retention_policies(project_id);

CREATE TABLE IF NOT EXISTS retention_tags (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  repository_id BIGINT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  tag           TEXT    NOT NULL,
  digest        TEXT    NOT NULL,
  size          BIGINT NOT NULL DEFAULT 0,
  pushed_at     BIGINT,
  pulled_at     BIGINT,
  created_at    BIGINT NOT NULL,
  updated_at    BIGINT NOT NULL,
  UNIQUE (repository_id, tag)
);
CREATE INDEX IF NOT EXISTS idx_retention_tags_repo   ON retention_tags(repository_id);
CREATE INDEX IF NOT EXISTS idx_retention_tags_digest ON retention_tags(digest);

CREATE TABLE IF NOT EXISTS cleanup_runs (
  id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  trigger              TEXT   NOT NULL CHECK(trigger IN ('schedule','manual','push')),
  scope                TEXT   NOT NULL DEFAULT 'all',
  project_name         TEXT   NOT NULL DEFAULT '',
  dry_run              BIGINT NOT NULL DEFAULT 0 CHECK(dry_run IN (0,1)),
  status               TEXT   NOT NULL CHECK(status IN ('running','success','failed')),
  tags_deleted         BIGINT NOT NULL DEFAULT 0,
  manifests_deleted    BIGINT NOT NULL DEFAULT 0,
  tags_kept            BIGINT NOT NULL DEFAULT 0,
  skipped_unknown_time BIGINT NOT NULL DEFAULT 0,
  skipped_shared_digest BIGINT NOT NULL DEFAULT 0,
  failed_count         BIGINT NOT NULL DEFAULT 0,
  error                TEXT,
  detail               TEXT,
  started_at           BIGINT NOT NULL,
  finished_at          BIGINT
);
CREATE INDEX IF NOT EXISTS idx_cleanup_runs_started ON cleanup_runs(started_at DESC);
