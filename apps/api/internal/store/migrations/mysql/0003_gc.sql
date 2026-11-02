-- lau6-cr blob 回收执行历史（MySQL 方言）
--
-- MySQL 特有两点（沿用 0001/0002 的约定）：
--  1) TEXT/BLOB 列**不能带 DEFAULT**，因此带默认值的列都用 VARCHAR。
--  2) MySQL 8.0 不支持 CREATE INDEX IF NOT EXISTS，索引全部内联在 CREATE TABLE 里。

CREATE TABLE IF NOT EXISTS gc_runs (
  id                  BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `trigger`            VARCHAR(16)  NOT NULL CHECK(`trigger` IN ('manual','after_cleanup','schedule')),
  dry_run             BIGINT       NOT NULL DEFAULT 0 CHECK(dry_run IN (0,1)),
  status              VARCHAR(16)  NOT NULL CHECK(status IN ('running','success','failed')),
  storage_root        VARCHAR(512) NOT NULL DEFAULT '',
  repositories        BIGINT       NOT NULL DEFAULT 0,
  manifests_found     BIGINT       NOT NULL DEFAULT 0,
  blobs_marked        BIGINT       NOT NULL DEFAULT 0,
  blobs_scanned       BIGINT       NOT NULL DEFAULT 0,
  blobs_deleted       BIGINT       NOT NULL DEFAULT 0,
  bytes_reclaimed     BIGINT       NOT NULL DEFAULT 0,
  layer_links_deleted BIGINT       NOT NULL DEFAULT 0,
  untagged_deleted    BIGINT       NOT NULL DEFAULT 0,
  error               VARCHAR(2048) NULL,
  started_at          BIGINT       NOT NULL,
  finished_at         BIGINT       NULL,
  KEY idx_gc_runs_started (started_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;
