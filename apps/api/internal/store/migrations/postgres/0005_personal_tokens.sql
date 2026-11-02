-- lau6-cr 个人访问令牌（PAT）（PostgreSQL 方言）
--
-- 语义与 sqlite/0005_personal_tokens.sql 一致，见那里的注释。
--
-- ⚠️ expires_at 可空：NULL = **永久有效**。
CREATE TABLE IF NOT EXISTS personal_tokens (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT   NOT NULL,
  token_hash   TEXT   NOT NULL,
  token_prefix TEXT   NOT NULL,
  created_at   BIGINT NOT NULL,
  expires_at   BIGINT,
  last_used_at BIGINT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_personal_tokens_hash ON personal_tokens(token_hash);
CREATE INDEX IF NOT EXISTS idx_personal_tokens_user ON personal_tokens(user_id);
