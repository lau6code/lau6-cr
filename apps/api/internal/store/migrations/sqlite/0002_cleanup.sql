-- lau6-cr 镜像清理策略（SQLite 方言）
--
-- 三张表：
--   retention_policies —— 保留策略（全局默认 + 项目级覆盖）
--   retention_tags     —— tag 级快照（推送/拉取时间），保留策略求值的依据
--   cleanup_runs       —— 清理执行历史
--
-- 约定：时间戳统一存 unix 秒（INTEGER），布尔存 0/1 整数。

-- ⚠️ scope_key 而不是直接对 project_id 建唯一索引：
--    project_id 可空，而 SQLite/PostgreSQL 的唯一索引**不认为多个 NULL 冲突**，
--    直接建唯一索引会导致"全局默认策略"被插进去好几行。
--    scope_key 是显式的作用域标识：'' = 全局默认，'p:<id>' = 项目级。
CREATE TABLE IF NOT EXISTS retention_policies (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  scope_key    TEXT    NOT NULL UNIQUE,
  project_id   INTEGER REFERENCES projects(id) ON DELETE CASCADE,
  enabled      INTEGER NOT NULL DEFAULT 0 CHECK(enabled IN (0,1)),
  algorithm    TEXT    NOT NULL DEFAULT 'or' CHECK(algorithm IN ('or')),
  rules        TEXT    NOT NULL DEFAULT '[]',
  tag_selector TEXT    NOT NULL DEFAULT '**',
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_retention_policies_project ON retention_policies(project_id);

-- tag 级快照。
--
-- ⚠️ pushed_at 可空：没有推送时间意味着"这个 tag 是什么时候来的不知道"。
--    保留策略遇到它一律保留（见 03-registry.md §5.2.4 ②），
--    绝不能把 NULL 当成 0（= 1970 年）然后当成"很老"删掉。
CREATE TABLE IF NOT EXISTS retention_tags (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  repository_id INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  tag           TEXT    NOT NULL,
  digest        TEXT    NOT NULL,
  size          INTEGER NOT NULL DEFAULT 0,
  pushed_at     INTEGER,
  pulled_at     INTEGER,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  UNIQUE (repository_id, tag)
);
CREATE INDEX IF NOT EXISTS idx_retention_tags_repo ON retention_tags(repository_id);
CREATE INDEX IF NOT EXISTS idx_retention_tags_digest ON retention_tags(digest);

-- 清理执行历史。
--
-- ⚠️ status 只有 running / success / failed 三态：
--    "部分成功"不允许伪装成 success——有任何一个删除失败就是 failed。
CREATE TABLE IF NOT EXISTS cleanup_runs (
  id                   INTEGER PRIMARY KEY AUTOINCREMENT,
  trigger              TEXT    NOT NULL CHECK(trigger IN ('schedule','manual','push')),
  scope                TEXT    NOT NULL DEFAULT 'all',
  project_name         TEXT    NOT NULL DEFAULT '',
  dry_run              INTEGER NOT NULL DEFAULT 0 CHECK(dry_run IN (0,1)),
  status               TEXT    NOT NULL CHECK(status IN ('running','success','failed')),
  tags_deleted         INTEGER NOT NULL DEFAULT 0,
  manifests_deleted    INTEGER NOT NULL DEFAULT 0,
  tags_kept            INTEGER NOT NULL DEFAULT 0,
  skipped_unknown_time INTEGER NOT NULL DEFAULT 0,
  skipped_shared_digest INTEGER NOT NULL DEFAULT 0,
  failed_count         INTEGER NOT NULL DEFAULT 0,
  error                TEXT,
  detail               TEXT,
  started_at           INTEGER NOT NULL,
  finished_at          INTEGER
);
CREATE INDEX IF NOT EXISTS idx_cleanup_runs_started ON cleanup_runs(started_at DESC);
