-- lau6-cr 项目访问令牌（SQLite 方言）
--
-- 背景：项目管理员需要一枚**项目身份**的长期凭据，
-- 交给 CI / 流水线推拉镜像，不必占用某个人的个人账号（02-auth.md §2.6.2）。
--
-- 设计要点：
--   - **只存散列**。原始令牌只在创建时返回一次，之后无法再取回——
--     数据库泄露也拿不到可直接使用的凭据。
--   - token_hash 唯一：按散列查找是认证热路径。
--   - role 创建时选定、**此后不变**：角色即权限。
--
-- ⚠️ expires_at **可空**：NULL = **永久有效**（用户显式选的，不是默认值）。
--
-- ⚠️ role 的取值校验**不在这里兜底**：CHECK 约束在 MySQL 上可能不被执行，
--    应用层（service.ProjectTokenService.Create）是权威闸门。
CREATE TABLE IF NOT EXISTS project_tokens (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id   INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  name         TEXT    NOT NULL,
  role         TEXT    NOT NULL
                  CHECK(role IN ('admin', 'developer', 'reader')),
  token_hash   TEXT    NOT NULL,
  token_prefix TEXT    NOT NULL,
  -- 创建人只用于审计；令牌本身不代表这个人
  created_by   INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER,
  last_used_at INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_project_tokens_hash    ON project_tokens(token_hash);
CREATE INDEX        IF NOT EXISTS idx_project_tokens_project ON project_tokens(project_id);

-- 清理历史前缀的个人令牌（02-auth.md §2.6.4）。
-- 个人令牌前缀已从 cr_pat_ 改为 lau6_u_，认证路径不再认旧前缀。
-- 散列是原子的前像，改前缀必然改散列，无法还原——只能删，不能改。
--
-- 用 SUBSTR(...) = 'cr_pat_' 而不是 LIKE：`_` 在 LIKE 里是单字符通配符，
-- 转义写法在三方言里还有反斜杠字面量差异，等值比较没有这些坑。
-- （SUBSTR 三方言都有且都是 1 起始；SQLite 根本没有 LEFT 函数。）
DELETE FROM personal_tokens WHERE SUBSTR(token_prefix, 1, 7) = 'cr_pat_';
