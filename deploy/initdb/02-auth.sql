-- TanGIS API Key 表（PRD F-06 最小鉴权）
-- 与 server/internal/auth/auth.go 中的 apiKeysDDL 保持一致（服务启动时也会
-- CREATE TABLE IF NOT EXISTS，双保险；此脚本供 initdb 首次初始化）。
--
-- key_hash 为原始 key 的 SHA-256 hex（原始 key 仅在签发时对用户展示一次）。
-- role 取值 'admin' | 'tenant'：admin 可执行发布审批等管理动作。
CREATE TABLE IF NOT EXISTS api_keys (
    id        text PRIMARY KEY,
    key_hash  text NOT NULL UNIQUE,
    name      text NOT NULL,
    tenant_id text NOT NULL,
    role      text NOT NULL DEFAULT 'tenant',
    created_at timestamptz NOT NULL DEFAULT now()
);

-- 内置开发默认 key：tangis-dev-key（sha256 见 auth.seedAdminKeyHash）。
-- 仅供本地联调开箱可用；生产部署应替换为正式 key 并删除此行。
INSERT INTO api_keys (id, key_hash, name, tenant_id, role)
VALUES ('dev-default', '303b67a6146724ec8813d4edc1519352f36c02e93e72e6df2a06b45a0e6d6360',
        'local development default key', 'default', 'admin')
ON CONFLICT (id) DO NOTHING;
