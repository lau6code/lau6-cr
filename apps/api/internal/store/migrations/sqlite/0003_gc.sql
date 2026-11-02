-- lau6-cr blob 回收执行历史（SQLite 方言）
--
-- 与 cleanup_runs 分开存：两者是**两件事**——
--   cleanup_runs 记的是"删了哪些 tag/manifest"（第 ① 层）
--   gc_runs     记的是"回收了多少 blob 与磁盘"（第 ② 层）
--
-- 约定：时间戳统一存 unix 秒（INTEGER），布尔存 0/1 整数。

-- ⚠️ status 只有 running / success / failed 三态：
--    "部分成功"不允许伪装成 success——扫盘失败就是失败。
CREATE TABLE IF NOT EXISTS gc_runs (
  id                  INTEGER PRIMARY KEY AUTOINCREMENT,
  trigger             TEXT    NOT NULL CHECK(trigger IN ('manual','after_cleanup','schedule')),
  dry_run             INTEGER NOT NULL DEFAULT 0 CHECK(dry_run IN (0,1)),
  status              TEXT    NOT NULL CHECK(status IN ('running','success','failed')),
  storage_root        TEXT    NOT NULL DEFAULT '',
  repositories        INTEGER NOT NULL DEFAULT 0,
  manifests_found     INTEGER NOT NULL DEFAULT 0,
  blobs_marked        INTEGER NOT NULL DEFAULT 0,
  blobs_scanned       INTEGER NOT NULL DEFAULT 0,
  blobs_deleted       INTEGER NOT NULL DEFAULT 0,
  bytes_reclaimed     INTEGER NOT NULL DEFAULT 0,
  layer_links_deleted INTEGER NOT NULL DEFAULT 0,
  untagged_deleted    INTEGER NOT NULL DEFAULT 0,
  error               TEXT,
  started_at          INTEGER NOT NULL,
  finished_at         INTEGER
);
CREATE INDEX IF NOT EXISTS idx_gc_runs_started ON gc_runs(started_at DESC);
