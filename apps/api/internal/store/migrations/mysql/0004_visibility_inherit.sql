-- lau6-cr 仓库可见性跟随项目（MySQL 方言）
--
-- 语义与 sqlite/0004_visibility_inherit.sql 一致，见那里的注释。
--
-- ⚠️ 默认值 1 = "跟随项目"：既有行全部变成继承。
--    旧行里的 private 是**自动建仓时写死的**，不是谁有意收紧的。
ALTER TABLE repositories ADD COLUMN inherit_visibility BIGINT NOT NULL DEFAULT 1;
