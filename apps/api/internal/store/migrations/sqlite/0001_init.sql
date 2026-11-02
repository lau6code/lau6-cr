-- lau6-cr 初始表结构（SQLite 方言）
-- 约定：时间戳统一存 unix 秒（BIGINT / INTEGER），布尔存 0/1 整数。

CREATE TABLE IF NOT EXISTS users (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  username        TEXT    NOT NULL UNIQUE,
  password_hash   TEXT    NOT NULL,
  source          TEXT    NOT NULL DEFAULT 'local' CHECK(source IN ('local','ldap')),
  display_name    TEXT    NOT NULL DEFAULT '',
  is_system_admin INTEGER NOT NULL DEFAULT 0 CHECK(is_system_admin IN (0,1)),
  disabled        INTEGER NOT NULL DEFAULT 0 CHECK(disabled IN (0,1)),
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_users_admin ON users(is_system_admin, disabled);

CREATE TABLE IF NOT EXISTS sessions (
  id           TEXT    PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  client_ip    TEXT,
  user_agent   TEXT
);
CREATE INDEX IF NOT EXISTS idx_sessions_user    ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS projects (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  name         TEXT    NOT NULL UNIQUE,
  display_name TEXT    NOT NULL DEFAULT '',
  description  TEXT    NOT NULL DEFAULT '',
  visibility   TEXT    NOT NULL DEFAULT 'private' CHECK(visibility IN ('public','private')),
  created_by   INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS project_members (
  project_id     INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  user_id        INTEGER NOT NULL REFERENCES users(id)    ON DELETE CASCADE,
  role           TEXT    NOT NULL CHECK(role IN ('admin','developer','reader')),
  repo_patterns  TEXT    NOT NULL DEFAULT '[]',
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL,
  PRIMARY KEY (project_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_project_members_user ON project_members(user_id);

CREATE TABLE IF NOT EXISTS repositories (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id      INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  name            TEXT    NOT NULL,
  visibility      TEXT    NOT NULL DEFAULT 'private' CHECK(visibility IN ('public','private')),
  description     TEXT    NOT NULL DEFAULT '',
  created_digest  TEXT,
  latest_digest   TEXT,
  pushed_at       INTEGER,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  UNIQUE (project_id, name)
);
CREATE INDEX IF NOT EXISTS idx_repositories_project    ON repositories(project_id);
CREATE INDEX IF NOT EXISTS idx_repositories_visibility ON repositories(project_id, visibility);

CREATE TABLE IF NOT EXISTS repository_grants (
  repository_id INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  user_id       INTEGER NOT NULL REFERENCES users(id)       ON DELETE CASCADE,
  granted_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at    INTEGER NOT NULL,
  PRIMARY KEY (repository_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_repository_grants_user ON repository_grants(user_id);

CREATE TABLE IF NOT EXISTS repo_meta (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  repository_id  INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  latest_tag     TEXT,
  tag_count      INTEGER NOT NULL DEFAULT 0,
  size           INTEGER NOT NULL DEFAULT 0,
  created        TEXT,
  platforms      TEXT,
  pull_count     INTEGER NOT NULL DEFAULT 0,
  last_pulled_at INTEGER,
  refreshed_at   INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_repo_meta_repository ON repo_meta(repository_id);
CREATE INDEX IF NOT EXISTS idx_repo_meta_size       ON repo_meta(size DESC);
CREATE INDEX IF NOT EXISTS idx_repo_meta_created    ON repo_meta(created DESC);
CREATE INDEX IF NOT EXISTS idx_repo_meta_tag_count  ON repo_meta(tag_count DESC);
CREATE INDEX IF NOT EXISTS idx_repo_meta_refreshed  ON repo_meta(refreshed_at);

CREATE TABLE IF NOT EXISTS system_settings (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  key        TEXT    NOT NULL UNIQUE CHECK(length(key) > 0),
  value      TEXT    NOT NULL,
  version    INTEGER NOT NULL DEFAULT 1 CHECK(version > 0),
  updated_at INTEGER NOT NULL,
  updated_by TEXT    NOT NULL DEFAULT 'system'
);

CREATE TABLE IF NOT EXISTS audit_logs (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  ts         INTEGER NOT NULL,
  actor      TEXT    NOT NULL,
  action     TEXT    NOT NULL,
  target     TEXT,
  scope      TEXT,
  client_ip  TEXT,
  success    INTEGER NOT NULL DEFAULT 1,
  detail     TEXT
);
CREATE INDEX IF NOT EXISTS idx_audit_ts     ON audit_logs(ts DESC);
CREATE INDEX IF NOT EXISTS idx_audit_actor  ON audit_logs(actor);
CREATE INDEX IF NOT EXISTS idx_audit_action ON audit_logs(action);

CREATE TABLE IF NOT EXISTS traffic_stats_hourly (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  repository_id  INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  bucket_at      INTEGER NOT NULL,
  pulls          INTEGER NOT NULL DEFAULT 0,
  pushes         INTEGER NOT NULL DEFAULT 0,
  deletes        INTEGER NOT NULL DEFAULT 0,
  bytes_pulled   INTEGER NOT NULL DEFAULT 0,
  bytes_pushed   INTEGER NOT NULL DEFAULT 0,
  updated_at     INTEGER NOT NULL,
  UNIQUE (repository_id, bucket_at)
);
CREATE INDEX IF NOT EXISTS idx_traffic_hourly_bucket ON traffic_stats_hourly(bucket_at);

CREATE TABLE IF NOT EXISTS traffic_stats_daily (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  repository_id  INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  stat_date      TEXT    NOT NULL,
  pulls          INTEGER NOT NULL DEFAULT 0,
  pushes         INTEGER NOT NULL DEFAULT 0,
  deletes        INTEGER NOT NULL DEFAULT 0,
  bytes_pulled   INTEGER NOT NULL DEFAULT 0,
  bytes_pushed   INTEGER NOT NULL DEFAULT 0,
  updated_at     INTEGER NOT NULL,
  UNIQUE (repository_id, stat_date)
);
CREATE INDEX IF NOT EXISTS idx_traffic_daily_date ON traffic_stats_daily(stat_date);
