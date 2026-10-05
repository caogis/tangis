-- TanGIS 任务表（PRD F-04 任务持久化）
-- 与 server/internal/task/pg.go 中的 schemaDDL 保持一致（服务启动时也会
-- CREATE TABLE IF NOT EXISTS + ALTER ADD COLUMN IF NOT EXISTS，双保险；
-- 此脚本供 initdb 首次初始化）。
CREATE TABLE IF NOT EXISTS tasks (
    id            text PRIMARY KEY,
    type          text NOT NULL,
    source        text NOT NULL,
    output        text NOT NULL,
    status        text NOT NULL,
    manifest_path text,
    error         text,
    minio_prefix  text,
    -- F-04：参数版本化（随任务下发内核）、分块进度、失败重试计数
    params        jsonb,
    progress_done int  NOT NULL DEFAULT 0,
    progress_total int NOT NULL DEFAULT 0,
    attempts      int  NOT NULL DEFAULT 0,
    -- F-06：多租户隔离
    tenant_id     text NOT NULL DEFAULT '',
    -- F-21：发布审批（审批通过才进入 /api/v1/services 与分发路径）
    approved      boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- 已有库平滑升级
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS tenant_id text NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS params jsonb;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS progress_done int NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS progress_total int NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS attempts int NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS approved boolean NOT NULL DEFAULT false;

-- 服务发布列表按 status 过滤（SUCCEEDED）、任务列表按创建时间倒序、租户隔离查询
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks (status);
CREATE INDEX IF NOT EXISTS idx_tasks_created_at ON tasks (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_tenant ON tasks (tenant_id);

-- M2-F09c：编辑任务（parent 链查询、内核 ops.json 留痕摘要）
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS parent_task_id text NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS ops_summary jsonb;
CREATE INDEX IF NOT EXISTS idx_tasks_parent ON tasks (parent_task_id);
