-- examSystem 管理端设置表 (Task: 管理后台设置栏).
--
-- 用途:
--   * admin.password  -> {"hash": "<bcrypt>", "updated_at": ...}  管理端密码 (bcrypt hash)
--   * scoring         -> {"method":"local|remote_reranker|llm", ...}  评分方式 + API 凭据
--        (凭据仅在 DB 内以明文 JSON 存储; 需接入加密存储时后续迁移替换, 现为内部系统可用)
--   * key 为主键, value 统一 jsonb, 便于无 schema 演进扩展新设置项.
CREATE TABLE app_settings (
    key        text PRIMARY KEY,
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- 显式授给运行时角色 exam_app (迁移以 exam_migrator 跑, DEFAULT PRIVILEGES
-- 仅覆盖建表者自己的表; 不授则容器内 exam_app 读不到本表, 设置栏 503).
GRANT SELECT, INSERT, UPDATE, DELETE ON app_settings TO exam_app;
