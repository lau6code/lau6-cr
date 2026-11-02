-- lau6-cr 流量统计：匿名客户端 IP 归集（PostgreSQL 方言）
--
-- 语义与 sqlite/0006_stats_rollup.sql 一致，见那里的注释。
--
-- ⚠️ 主键写法照 0001_init.sql 里的 traffic_stats_hourly：
--    PostgreSQL 用 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY。
--
-- ⚠️ traffic_stats_daily **不在这里建**：它已经在 0001_init.sql 里建好了。
CREATE TABLE IF NOT EXISTS traffic_stats_ip (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  repository_id BIGINT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  bucket_at     BIGINT NOT NULL,
  ip_hash       TEXT   NOT NULL,
  hits          BIGINT NOT NULL DEFAULT 0,
  updated_at    BIGINT NOT NULL,
  UNIQUE (repository_id, bucket_at, ip_hash)
);
CREATE INDEX IF NOT EXISTS idx_traffic_ip_bucket ON traffic_stats_ip(bucket_at);
