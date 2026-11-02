-- lau6-cr 口令传输模式默认值修正（PostgreSQL 方言）
--
-- 背景：口令传输模式 `password.mode` 的**内置默认值**曾是 plaintext，
-- 而设置项定义（service.settings_defs）写的是 optional。
-- 两层默认值打架的后果是：全新实例的运行期生效值是 plaintext
-- → GET /api/v1/auth/pubkey 返回 enabled:false
-- → 前端按明文提交口令（{username, password}），口令明文过网。
--
-- 判据：设置页保存一项设置时**必定写一行 DB**（settings.go 的 tx.UpsertSetting），
-- 所以 `system_settings` 里存在 `password.mode` 行 ⟺ 运维显式选过模式。
-- 没有行 ⟺ 生效值来自内置默认值（以及由它导出的 application.yaml 陈旧值），
-- 那正是本次修掉的错误默认值 ⇒ 提升为文档默认值 optional。
--
-- ⚠️ optional **仍然接受明文**（server.resolvePassword），
--    所以旧客户端不受影响，只有升级后的前端开始发密文。
--    运维显式选过 plaintext / required 的实例**不动**（有行，被 ON CONFLICT 跳过）。
--
-- value 存的是 JSON 编码后的字符串（store.UpsertSetting 用 json.Marshal），
-- 所以字面量是带双引号的 "optional"。
--
-- ON CONFLICT DO NOTHING 让这条语句幂等：重跑不会因唯一键冲突失败（迁移必须可重跑）。
INSERT INTO system_settings ("key", value, version, updated_at, updated_by)
VALUES ('password.mode', '"optional"', 1, EXTRACT(EPOCH FROM NOW())::BIGINT, 'migration-0008')
ON CONFLICT ("key") DO NOTHING;
