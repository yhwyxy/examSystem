-- scripts/pg-bootstrap.sql
--
-- 一键建立 examSystem 的 PG 用户与库. 仅供 superuser / container entrypoint.
--
-- 本机 EDB PostgreSQL 跑法 (用你实际的 superuser 密码):
--   PGPASSWORD=<your-super-password> psql -h 127.0.0.1 -p 5432 \
--       -U postgres -d postgres -f scripts/pg-bootstrap.sql
--
-- Docker (docker-compose.postgres.yml 已自动挂此文件):
--   容器启动 entrypoint 自动执行; POSTGRES_USER=exam_super 时, exam_super 即
--   superuser, 会通过此文件进一步建 exam_app / exam_migrator + 库已存在.
--
-- 重入安全: 任一 role 已存在则跳过; 库已存在会以 NOTICE 告知, 不中断后续语句.

-- 1. 角色: exam_app (应用运行态; DML 无 DDL) + exam_migrator (有 DDL)
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'exam_app') THEN
        CREATE ROLE exam_app LOGIN PASSWORD 'exam_app_dev';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'exam_migrator') THEN
        CREATE ROLE exam_migrator LOGIN PASSWORD 'exam_migrator_dev';
    END IF;
END $$;

-- 2. 库: exam_system (允许已存在, 用 SELECT 配合 \gexec 绕过 CREATE DATABASE 在事务中的限制)
SELECT 'CREATE DATABASE exam_system OWNER exam_app ENCODING ''UTF8'''
    WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'exam_system')\gexec

\connect exam_system

-- 3. 赋 exam_app (全表 DML + schema 用, 无 DDL)
GRANT CONNECT ON DATABASE exam_system TO exam_app;
GRANT USAGE ON SCHEMA public TO exam_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO exam_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO exam_app;

-- 4. 赋 exam_migrator (有 DDL + DML, 用于 Task 2+ 跑 schema_migrations)
GRANT CONNECT ON DATABASE exam_system TO exam_migrator;
GRANT ALL ON SCHEMA public TO exam_migrator;
-- CREATE 权限: 让 testutil 建临时 schema (Task 2 测试隔离用)
GRANT CREATE ON DATABASE exam_system TO exam_migrator;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT ALL ON TABLES TO exam_migrator;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT ALL ON SEQUENCES TO exam_migrator;

-- 5. 提示
\echo 'examSystem PostgreSQL bootstrap OK:'
\echo '  exam_app:       exam_app_dev         (DML only)'
\echo '  exam_migrator:  exam_migrator_dev    (DDL + DML)'
\echo '  database:       exam_system'
