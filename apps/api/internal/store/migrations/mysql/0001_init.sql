-- lau6-cr 初始表结构（MySQL 方言）
-- 约定：时间戳统一存 unix 秒（BIGINT），布尔存 0/1 整数。
--
-- MySQL 特有两点，务必注意：
--  1) TEXT/BLOB 列**不能带 DEFAULT**，因此所有带默认值的列都用 VARCHAR。
--  2) MySQL 8.0 **不支持 CREATE INDEX IF NOT EXISTS**，
--     所以索引全部内联在 CREATE TABLE 里 —— 整条迁移天然幂等，
--     部分失败后重跑也安全（MySQL 无事务性 DDL，幂等是唯一的自愈手段）。

CREATE TABLE IF NOT EXISTS users (
  id              BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
  username        VARCHAR(191) NOT NULL UNIQUE,
  password_hash   VARCHAR(255) NOT NULL,
  source          VARCHAR(16)  NOT NULL DEFAULT 'local'  CHECK(source IN ('local','ldap')),
  display_name    VARCHAR(255) NOT NULL DEFAULT '',
  is_system_admin BIGINT       NOT NULL DEFAULT 0        CHECK(is_system_admin IN (0,1)),
  disabled        BIGINT       NOT NULL DEFAULT 0        CHECK(disabled IN (0,1)),
  created_at      BIGINT       NOT NULL,
  updated_at      BIGINT       NOT NULL,
  KEY idx_users_admin (is_system_admin, disabled)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE IF NOT EXISTS sessions (
  id           VARCHAR(64)  NOT NULL PRIMARY KEY,
  user_id      BIGINT       NOT NULL,
  created_at   BIGINT       NOT NULL,
  expires_at   BIGINT       NOT NULL,
  last_seen_at BIGINT       NOT NULL,
  client_ip    VARCHAR(45)  NULL,
  user_agent   VARCHAR(512) NULL,
  KEY idx_sessions_user (user_id),
  KEY idx_sessions_expires (expires_at),
  CONSTRAINT fk_sessions_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE IF NOT EXISTS projects (
  id           BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
  name         VARCHAR(191) NOT NULL UNIQUE,
  display_name VARCHAR(255) NOT NULL DEFAULT '',
  description  VARCHAR(1024) NOT NULL DEFAULT '',
  visibility   VARCHAR(16)  NOT NULL DEFAULT 'private' CHECK(visibility IN ('public','private')),
  created_by   BIGINT       NULL,
  created_at   BIGINT       NOT NULL,
  updated_at   BIGINT       NOT NULL,
  CONSTRAINT fk_projects_creator FOREIGN KEY (created_by) REFERENCES users(id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE IF NOT EXISTS project_members (
  project_id     BIGINT        NOT NULL,
  user_id        BIGINT        NOT NULL,
  role           VARCHAR(16)   NOT NULL CHECK(role IN ('admin','developer','reader')),
  repo_patterns  VARCHAR(4096) NOT NULL DEFAULT '[]',
  created_at     BIGINT        NOT NULL,
  updated_at     BIGINT        NOT NULL,
  PRIMARY KEY (project_id, user_id),
  KEY idx_project_members_user (user_id),
  CONSTRAINT fk_members_project FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE,
  CONSTRAINT fk_members_user    FOREIGN KEY (user_id)    REFERENCES users(id)    ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE IF NOT EXISTS repositories (
  id             BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
  project_id     BIGINT       NOT NULL,
  name           VARCHAR(512) NOT NULL,
  visibility     VARCHAR(16)  NOT NULL DEFAULT 'private' CHECK(visibility IN ('public','private')),
  description    VARCHAR(1024) NOT NULL DEFAULT '',
  created_digest VARCHAR(255) NULL,
  latest_digest  VARCHAR(255) NULL,
  pushed_at      BIGINT       NULL,
  created_at     BIGINT       NOT NULL,
  updated_at     BIGINT       NOT NULL,
  UNIQUE KEY uk_repositories_project_name (project_id, name),
  KEY idx_repositories_visibility (project_id, visibility),
  CONSTRAINT fk_repos_project FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE IF NOT EXISTS repository_grants (
  repository_id BIGINT      NOT NULL,
  user_id       BIGINT      NOT NULL,
  granted_by    BIGINT      NULL,
  created_at    BIGINT      NOT NULL,
  PRIMARY KEY (repository_id, user_id),
  KEY idx_repository_grants_user (user_id),
  CONSTRAINT fk_grants_repo    FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE,
  CONSTRAINT fk_grants_user    FOREIGN KEY (user_id)       REFERENCES users(id)        ON DELETE CASCADE,
  CONSTRAINT fk_grants_granted FOREIGN KEY (granted_by)    REFERENCES users(id)        ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE IF NOT EXISTS repo_meta (
  id             BIGINT      NOT NULL AUTO_INCREMENT PRIMARY KEY,
  repository_id  BIGINT      NOT NULL,
  latest_tag     VARCHAR(255) NULL,
  tag_count      BIGINT      NOT NULL DEFAULT 0,
  size           BIGINT      NOT NULL DEFAULT 0,
  created        VARCHAR(64) NULL,
  platforms      TEXT        NULL,
  pull_count     BIGINT      NOT NULL DEFAULT 0,
  last_pulled_at BIGINT      NULL,
  refreshed_at   BIGINT      NOT NULL,
  UNIQUE KEY uk_repo_meta_repository (repository_id),
  KEY idx_repo_meta_size (size DESC),
  KEY idx_repo_meta_created (created DESC),
  KEY idx_repo_meta_tag_count (tag_count DESC),
  KEY idx_repo_meta_refreshed (refreshed_at),
  CONSTRAINT fk_repo_meta_repo FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE IF NOT EXISTS system_settings (
  id         BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `key`      VARCHAR(191) NOT NULL UNIQUE,
  value      TEXT         NOT NULL,
  version    BIGINT       NOT NULL DEFAULT 1 CHECK(version > 0),
  updated_at BIGINT       NOT NULL,
  updated_by VARCHAR(255) NOT NULL DEFAULT 'system'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE IF NOT EXISTS audit_logs (
  id         BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
  ts         BIGINT       NOT NULL,
  actor      VARCHAR(255) NOT NULL,
  action     VARCHAR(255) NOT NULL,
  target     VARCHAR(512) NULL,
  scope      VARCHAR(255) NULL,
  client_ip  VARCHAR(45)  NULL,
  success    BIGINT       NOT NULL DEFAULT 1,
  detail     TEXT         NULL,
  KEY idx_audit_ts (ts DESC),
  KEY idx_audit_actor (actor),
  KEY idx_audit_action (action)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE IF NOT EXISTS traffic_stats_hourly (
  id            BIGINT      NOT NULL AUTO_INCREMENT PRIMARY KEY,
  repository_id BIGINT      NOT NULL,
  bucket_at     BIGINT      NOT NULL,
  pulls         BIGINT      NOT NULL DEFAULT 0,
  pushes        BIGINT      NOT NULL DEFAULT 0,
  deletes       BIGINT      NOT NULL DEFAULT 0,
  bytes_pulled  BIGINT      NOT NULL DEFAULT 0,
  bytes_pushed  BIGINT      NOT NULL DEFAULT 0,
  updated_at    BIGINT      NOT NULL,
  UNIQUE KEY uk_traffic_hourly (repository_id, bucket_at),
  KEY idx_traffic_hourly_bucket (bucket_at),
  CONSTRAINT fk_traffic_hourly_repo FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE IF NOT EXISTS traffic_stats_daily (
  id            BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
  repository_id BIGINT       NOT NULL,
  stat_date     VARCHAR(10)  NOT NULL,
  pulls         BIGINT       NOT NULL DEFAULT 0,
  pushes        BIGINT       NOT NULL DEFAULT 0,
  deletes       BIGINT       NOT NULL DEFAULT 0,
  bytes_pulled  BIGINT       NOT NULL DEFAULT 0,
  bytes_pushed  BIGINT       NOT NULL DEFAULT 0,
  updated_at    BIGINT       NOT NULL,
  UNIQUE KEY uk_traffic_daily (repository_id, stat_date),
  KEY idx_traffic_daily_date (stat_date),
  CONSTRAINT fk_traffic_daily_repo FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;
