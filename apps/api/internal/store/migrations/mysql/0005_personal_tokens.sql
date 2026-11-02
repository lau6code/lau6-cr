-- lau6-cr 个人访问令牌（PAT）（MySQL 方言）
--
-- 语义与 sqlite/0005_personal_tokens.sql 一致，见那里的注释。
--
-- ⚠️ VARCHAR(191) 的上限来自 utf8mb4 索引键长度限制（MySQL 5.7/8.0 同此限制）：
--    散列是 64 位十六进制，191 足够。
--
-- ⚠️ expires_at 可空：NULL = **永久有效**。
CREATE TABLE IF NOT EXISTS personal_tokens (
  id           BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
  user_id      BIGINT       NOT NULL,
  name         VARCHAR(128) NOT NULL,
  token_hash   VARCHAR(191) NOT NULL,
  token_prefix VARCHAR(32)  NOT NULL,
  created_at   BIGINT       NOT NULL,
  expires_at   BIGINT       NULL,
  last_used_at BIGINT       NULL,
  UNIQUE KEY idx_personal_tokens_hash (token_hash),
  KEY idx_personal_tokens_user (user_id),
  CONSTRAINT fk_personal_tokens_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;
