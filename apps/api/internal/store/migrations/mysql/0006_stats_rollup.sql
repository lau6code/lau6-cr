-- lau6-cr 流量统计：匿名客户端 IP 归集（MySQL 方言）
--
-- 语义与 sqlite/0006_stats_rollup.sql 一致，见那里的注释。
--
-- ⚠️ 主键写法照 0001_init.sql 里的 traffic_stats_hourly：
--    MySQL 用 BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY + 表级 ENGINE=InnoDB。
--
-- ⚠️ ip_hash 用 VARCHAR(64)：sha256 的十六进制正好 64 字符。
--    唯一键 (repository_id, bucket_at, ip_hash) 的索引长度
--    = 8 + 8 + 64*4 = 272 字节，在 InnoDB 的 3072 字节上限内。
--
-- ⚠️ traffic_stats_daily **不在这里建**：它已经在 0001_init.sql 里建好了。
CREATE TABLE IF NOT EXISTS traffic_stats_ip (
  id            BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
  repository_id BIGINT       NOT NULL,
  bucket_at     BIGINT       NOT NULL,
  ip_hash       VARCHAR(64)  NOT NULL,
  hits          BIGINT       NOT NULL DEFAULT 0,
  updated_at    BIGINT       NOT NULL,
  UNIQUE KEY uk_traffic_ip (repository_id, bucket_at, ip_hash),
  KEY idx_traffic_ip_bucket (bucket_at),
  CONSTRAINT fk_traffic_ip_repo FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;
