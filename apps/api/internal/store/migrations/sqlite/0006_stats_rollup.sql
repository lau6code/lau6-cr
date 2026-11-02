-- lau6-cr 流量统计：匿名客户端 IP 归集（SQLite 方言）
--
-- 背景：docs/design/10-traffic-stats.md §6 —— 匿名请求的 IP **默认不记**，
-- 只有 stats.record_ip=true 时才会写这张表（ADR 14：IP 归集有隐私后果）。
--
-- 设计要点：
--   - **只存截断+哈希后的值**：IPv4 截到 /24、IPv6 截到 /48，再取 sha256。
--     库里没有明文 IP，定位不到具体个人，但还能区分"同一个网段"。
--   - 粒度是 (仓库, 小时桶, ip_hash)，**累加命中次数**：
--     同一网段在一小时里拉了 100 次 → hits=100，而不是 100 行。
--   - bucket_at 与 traffic_stats_hourly 同义（unix 秒，整小时起点），
--     保留期清理按同一口径走。
--   - 仓库删了，IP 记录跟着删（ON DELETE CASCADE）。
--
-- ⚠️ traffic_stats_daily **不在这里建**：它已经在 0001_init.sql 里建好了
--    （三方言都有），这里重复建会造成两套说法。
CREATE TABLE IF NOT EXISTS traffic_stats_ip (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  repository_id INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  bucket_at     INTEGER NOT NULL,
  ip_hash       TEXT    NOT NULL,
  hits          INTEGER NOT NULL DEFAULT 0,
  updated_at    INTEGER NOT NULL,
  UNIQUE (repository_id, bucket_at, ip_hash)
);
CREATE INDEX IF NOT EXISTS idx_traffic_ip_bucket ON traffic_stats_ip(bucket_at);
