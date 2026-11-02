-- lau6-cr 项目访问令牌（MySQL 方言）
--
-- 语义与 sqlite/0007_project_tokens.sql 一致，见那里的注释。
--
-- ⚠️ VARCHAR(191) 的上限来自 utf8mb4 索引键长度限制（MySQL 5.7/8.0 同此限制）：
--    散列是 64 位十六进制，191 足够。
--
-- ⚠️ expires_at 可空：NULL = **永久有效**。
--
-- ⚠️ 这里**不写 role 的 CHECK 约束**：MySQL 5.7 与早期 8.0 不执行 CHECK，
--    写了也是装饰。应用层（service.ProjectTokenService.Create）是权威闸门。
CREATE TABLE IF NOT EXISTS project_tokens (
  id           BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
  project_id   BIGINT       NOT NULL,
  name         VARCHAR(128) NOT NULL,
  role         VARCHAR(32)  NOT NULL,
  token_hash   VARCHAR(191) NOT NULL,
  token_prefix VARCHAR(32)  NOT NULL,
  created_by   BIGINT       NULL,
  created_at   BIGINT       NOT NULL,
  expires_at   BIGINT       NULL,
  last_used_at BIGINT       NULL,
  UNIQUE KEY idx_project_tokens_hash (token_hash),
  KEY idx_project_tokens_project (project_id),
  CONSTRAINT fk_project_tokens_project FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE,
  CONSTRAINT fk_project_tokens_creator FOREIGN KEY (created_by) REFERENCES users(id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- 清理历史前缀的个人令牌（02-auth.md §2.6.4）。
-- 用 SUBSTR(...) = 'cr_pat_' 而不是 LIKE：`_` 在 LIKE 里是单字符通配符，
-- 转义写法在各方言里还有反斜杠字面量差异，等值比较没有这些坑。
-- （SUBSTR 三方言都有且都是 1 起始；SQLite 根本没有 LEFT 函数。）
DELETE FROM personal_tokens WHERE SUBSTR(token_prefix, 1, 7) = 'cr_pat_';
