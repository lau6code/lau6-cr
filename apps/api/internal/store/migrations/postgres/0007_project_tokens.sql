-- lau6-cr 项目访问令牌（PostgreSQL 方言）
--
-- 语义与 sqlite/0007_project_tokens.sql 一致，见那里的注释。
--
-- ⚠️ expires_at 可空：NULL = **永久有效**。
CREATE TABLE IF NOT EXISTS project_tokens (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  project_id   BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  name         TEXT   NOT NULL,
  role         TEXT   NOT NULL
                 CHECK(role IN ('admin', 'developer', 'reader')),
  token_hash   TEXT   NOT NULL,
  token_prefix TEXT   NOT NULL,
  created_by   BIGINT REFERENCES users(id) ON DELETE SET NULL,
  created_at   BIGINT NOT NULL,
  expires_at   BIGINT,
  last_used_at BIGINT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_project_tokens_hash    ON project_tokens(token_hash);
CREATE INDEX        IF NOT EXISTS idx_project_tokens_project ON project_tokens(project_id);

-- 清理历史前缀的个人令牌（02-auth.md §2.6.4）。
-- 用 SUBSTR(...) = 'cr_pat_' 而不是 LIKE：`_` 在 LIKE 里是单字符通配符，
-- 转义写法在各方言里还有反斜杠字面量差异，等值比较没有这些坑。
-- （SUBSTR 三方言都有且都是 1 起始；SQLite 根本没有 LEFT 函数。）
DELETE FROM personal_tokens WHERE SUBSTR(token_prefix, 1, 7) = 'cr_pat_';
