-- lau6-cr 初始表结构（PostgreSQL 方言）
-- 约定：时间戳统一存 unix 秒（BIGINT / INTEGER），布尔存 0/1 整数。

CREATE TABLE IF NOT EXISTS users (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  username        TEXT    NOT NULL UNIQUE,
  password_hash   TEXT    NOT NULL,
  source          TEXT    NOT NULL DEFAULT 'local' CHECK(source IN ('local','ldap')),
  display_name    TEXT    NOT NULL DEFAULT '',
  is_system_admin BIGINT NOT NULL DEFAULT 0 CHECK(is_system_admin IN (0,1)),
  disabled        BIGINT NOT NULL DEFAULT 0 CHECK(disabled IN (0,1)),
  created_at      BIGINT NOT NULL,
  updated_at      BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_users_admin ON users(is_system_admin, disabled);

CREATE TABLE IF NOT EXISTS sessions (
  id           TEXT    PRIMARY KEY,
  user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at   BIGINT NOT NULL,
  expires_at   BIGINT NOT NULL,
  last_seen_at BIGINT NOT NULL,
  client_ip    TEXT,
  user_agent   TEXT
);
CREATE INDEX IF NOT EXISTS idx_sessions_user    ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS projects (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name         TEXT    NOT NULL UNIQUE,
  display_name TEXT    NOT NULL DEFAULT '',
  description  TEXT    NOT NULL DEFAULT '',
  visibility   TEXT    NOT NULL DEFAULT 'private' CHECK(visibility IN ('public','private')),
  created_by   BIGINT REFERENCES users(id) ON DELETE SET NULL,
  created_at   BIGINT NOT NULL,
  updated_at   BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS project_members (
  project_id     BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  user_id        BIGINT NOT NULL REFERENCES users(id)    ON DELETE CASCADE,
  role           TEXT    NOT NULL CHECK(role IN ('admin','developer','reader')),
  repo_patterns  TEXT    NOT NULL DEFAULT '[]',
  created_at     BIGINT NOT NULL,
  updated_at     BIGINT NOT NULL,
  PRIMARY KEY (project_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_project_members_user ON project_members(user_id);

CREATE TABLE IF NOT EXISTS repositories (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  project_id      BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  name            TEXT    NOT NULL,
  visibility      TEXT    NOT NULL DEFAULT 'private' CHECK(visibility IN ('public','private')),
  description     TEXT    NOT NULL DEFAULT '',
  created_digest  TEXT,
  latest_digest   TEXT,
  pushed_at       BIGINT,
  created_at      BIGINT NOT NULL,
  updated_at      BIGINT NOT NULL,
  UNIQUE (project_id, name)
);
CREATE INDEX IF NOT EXISTS idx_repositories_project    ON repositories(project_id);
CREATE INDEX IF NOT EXISTS idx_repositories_visibility ON repositories(project_id, visibility);

CREATE TABLE IF NOT EXISTS repository_grants (
  repository_id BIGINT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  user_id       BIGINT NOT NULL REFERENCES users(id)       ON DELETE CASCADE,
  granted_by    BIGINT REFERENCES users(id) ON DELETE SET NULL,
  created_at    BIGINT NOT NULL,
  PRIMARY KEY (repository_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_repository_grants_user ON repository_grants(user_id);

CREATE TABLE IF NOT EXISTS repo_meta (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  repository_id  BIGINT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  latest_tag     TEXT,
  tag_count      BIGINT NOT NULL DEFAULT 0,
  size           BIGINT NOT NULL DEFAULT 0,
  created        TEXT,
  platforms      TEXT,
  pull_count     BIGINT NOT NULL DEFAULT 0,
  last_pulled_at BIGINT,
  refreshed_at   BIGINT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_repo_meta_repository ON repo_meta(repository_id);
CREATE INDEX IF NOT EXISTS idx_repo_meta_size       ON repo_meta(size DESC);
CREATE INDEX IF NOT EXISTS idx_repo_meta_created    ON repo_meta(created DESC);
CREATE INDEX IF NOT EXISTS idx_repo_meta_tag_count  ON repo_meta(tag_count DESC);
CREATE INDEX IF NOT EXISTS idx_repo_meta_refreshed  ON repo_meta(refreshed_at);

CREATE TABLE IF NOT EXISTS system_settings (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  key        TEXT    NOT NULL UNIQUE CHECK(length(key) > 0),
  value      TEXT    NOT NULL,
  version    BIGINT NOT NULL DEFAULT 1 CHECK(version > 0),
  updated_at BIGINT NOT NULL,
  updated_by TEXT    NOT NULL DEFAULT 'system'
);

CREATE TABLE IF NOT EXISTS audit_logs (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  ts         BIGINT NOT NULL,
  actor      TEXT    NOT NULL,
  action     TEXT    NOT NULL,
  target     TEXT,
  scope      TEXT,
  client_ip  TEXT,
  success    BIGINT NOT NULL DEFAULT 1,
  detail     TEXT
);
CREATE INDEX IF NOT EXISTS idx_audit_ts     ON audit_logs(ts DESC);
CREATE INDEX IF NOT EXISTS idx_audit_actor  ON audit_logs(actor);
CREATE INDEX IF NOT EXISTS idx_audit_action ON audit_logs(action);

CREATE TABLE IF NOT EXISTS traffic_stats_hourly (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  repository_id  BIGINT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  bucket_at      BIGINT NOT NULL,
  pulls          BIGINT NOT NULL DEFAULT 0,
  pushes         BIGINT NOT NULL DEFAULT 0,
  deletes        BIGINT NOT NULL DEFAULT 0,
  bytes_pulled   BIGINT NOT NULL DEFAULT 0,
  bytes_pushed   BIGINT NOT NULL DEFAULT 0,
  updated_at     BIGINT NOT NULL,
  UNIQUE (repository_id, bucket_at)
);
CREATE INDEX IF NOT EXISTS idx_traffic_hourly_bucket ON traffic_stats_hourly(bucket_at);

CREATE TABLE IF NOT EXISTS traffic_stats_daily (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  repository_id  BIGINT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  stat_date      TEXT    NOT NULL,
  pulls          BIGINT NOT NULL DEFAULT 0,
  pushes         BIGINT NOT NULL DEFAULT 0,
  deletes        BIGINT NOT NULL DEFAULT 0,
  bytes_pulled   BIGINT NOT NULL DEFAULT 0,
  bytes_pushed   BIGINT NOT NULL DEFAULT 0,
  updated_at     BIGINT NOT NULL,
  UNIQUE (repository_id, stat_date)
);
CREATE INDEX IF NOT EXISTS idx_traffic_daily_date ON traffic_stats_daily(stat_date);
