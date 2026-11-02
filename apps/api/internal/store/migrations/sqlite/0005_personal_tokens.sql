-- lau6-cr 个人访问令牌（PAT）（SQLite 方言）
--
-- 背景：用户需要一枚**自己管理**的长期凭据，用来在 CI / 脚本里推拉
-- 需要权限的镜像（02-auth.md §2.7）。参考实现 apc 的 `personal_token` 表。
--
-- 设计要点：
--   - **只存散列**。原始令牌只在创建时返回一次，之后无法再取回——
--     数据库泄露也拿不到可直接使用的凭据。
--   - token_hash 唯一：按散列查找是 O(1)，也是认证热路径。
--   - last_used_at 记录最近使用时间，运维据此判断哪枚该清理。
--
-- ⚠️ expires_at **可空**：NULL = **永久有效**（用户明确要求）。
--    "永久"是用户显式选的，不是默认值——创建时必须表态
--    （never_expires / expires_at / 都不给→系统默认，见 02-auth.md §2.7）。
--
-- ⚠️ 令牌本身**不携带 scope**：它是一把"身份钥匙"，
--    能推拉哪些镜像由权限引擎按用户的**当前**角色实时判定。
--    角色改了，令牌的下一次使用立刻按新权限算——不有过期权限窗口。
CREATE TABLE IF NOT EXISTS personal_tokens (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT    NOT NULL,
  token_hash   TEXT    NOT NULL,
  token_prefix TEXT    NOT NULL,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER,
  last_used_at INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_personal_tokens_hash ON personal_tokens(token_hash);
CREATE INDEX IF NOT EXISTS idx_personal_tokens_user ON personal_tokens(user_id);
