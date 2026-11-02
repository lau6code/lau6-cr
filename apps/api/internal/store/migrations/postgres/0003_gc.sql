-- lau6-cr blob 回收执行历史（PostgreSQL 方言）
--
-- 与 SQLite 版结构一致，见 sqlite/0003_gc.sql 的注释。

CREATE TABLE IF NOT EXISTS gc_runs (
  id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  trigger             TEXT   NOT NULL CHECK(trigger IN ('manual','after_cleanup','schedule')),
  dry_run             BIGINT NOT NULL DEFAULT 0 CHECK(dry_run IN (0,1)),
  status              TEXT   NOT NULL CHECK(status IN ('running','success','failed')),
  storage_root        TEXT   NOT NULL DEFAULT '',
  repositories        BIGINT NOT NULL DEFAULT 0,
  manifests_found     BIGINT NOT NULL DEFAULT 0,
  blobs_marked        BIGINT NOT NULL DEFAULT 0,
  blobs_scanned       BIGINT NOT NULL DEFAULT 0,
  blobs_deleted       BIGINT NOT NULL DEFAULT 0,
  bytes_reclaimed     BIGINT NOT NULL DEFAULT 0,
  layer_links_deleted BIGINT NOT NULL DEFAULT 0,
  untagged_deleted    BIGINT NOT NULL DEFAULT 0,
  error               TEXT,
  started_at          BIGINT NOT NULL,
  finished_at         BIGINT
);
CREATE INDEX IF NOT EXISTS idx_gc_runs_started ON gc_runs(started_at DESC);
