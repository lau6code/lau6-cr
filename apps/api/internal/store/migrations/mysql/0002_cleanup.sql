-- lau6-cr 镜像清理策略（MySQL 方言）
--
-- MySQL 特有两点（沿用 0001 的约定）：
--  1) TEXT/BLOB 列**不能带 DEFAULT**，因此带默认值的列都用 VARCHAR。
--  2) MySQL 8.0 不支持 CREATE INDEX IF NOT EXISTS，索引全部内联在 CREATE TABLE 里。

CREATE TABLE IF NOT EXISTS retention_policies (
  id           BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
  scope_key    VARCHAR(64)  NOT NULL,
  project_id   BIGINT       NULL,
  enabled      BIGINT       NOT NULL DEFAULT 0 CHECK(enabled IN (0,1)),
  algorithm    VARCHAR(16)  NOT NULL DEFAULT 'or' CHECK(algorithm IN ('or')),
  rules        VARCHAR(4096) NOT NULL DEFAULT '[]',
  tag_selector VARCHAR(255)  NOT NULL DEFAULT '**',
  created_at   BIGINT       NOT NULL,
  updated_at   BIGINT       NOT NULL,
  UNIQUE KEY uk_retention_policies_scope (scope_key),
  KEY idx_retention_policies_project (project_id),
  CONSTRAINT fk_retention_policy_project FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE IF NOT EXISTS retention_tags (
  id            BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
  repository_id BIGINT       NOT NULL,
  tag           VARCHAR(255) NOT NULL,
  digest        VARCHAR(255) NOT NULL,
  size          BIGINT       NOT NULL DEFAULT 0,
  pushed_at     BIGINT       NULL,
  pulled_at     BIGINT       NULL,
  created_at    BIGINT       NOT NULL,
  updated_at    BIGINT       NOT NULL,
  UNIQUE KEY uk_retention_tags_repo_tag (repository_id, tag),
  KEY idx_retention_tags_digest (digest),
  CONSTRAINT fk_retention_tags_repo FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE IF NOT EXISTS cleanup_runs (
  id                   BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `trigger`            VARCHAR(16)  NOT NULL CHECK(`trigger` IN ('schedule','manual','push')),
  scope                VARCHAR(64)  NOT NULL DEFAULT 'all',
  project_name         VARCHAR(191) NOT NULL DEFAULT '',
  dry_run              BIGINT       NOT NULL DEFAULT 0 CHECK(dry_run IN (0,1)),
  status               VARCHAR(16)  NOT NULL CHECK(status IN ('running','success','failed')),
  tags_deleted         BIGINT       NOT NULL DEFAULT 0,
  manifests_deleted    BIGINT       NOT NULL DEFAULT 0,
  tags_kept            BIGINT       NOT NULL DEFAULT 0,
  skipped_unknown_time BIGINT       NOT NULL DEFAULT 0,
  skipped_shared_digest BIGINT      NOT NULL DEFAULT 0,
  failed_count         BIGINT       NOT NULL DEFAULT 0,
  error                VARCHAR(2048) NULL,
  detail               TEXT         NULL,
  started_at           BIGINT       NOT NULL,
  finished_at          BIGINT       NULL,
  KEY idx_cleanup_runs_started (started_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;
