#!/usr/bin/env bash
# scripts/pg-bootstrap-prod.sh - 生产 PG 最小权限角色初始化.
#
# 由 docker-compose.prod.yml 挂到 postgres 容器 /docker-entrypoint-initdb.d/,
# 首次初始化 (空卷) 时以 superuser (POSTGRES_USER) 自动执行, 幂等.
#
# 创建两个最小权限角色:
#   exam_app       应用/worker 运行态 (仅 DML)          -- EXAM_DATABASE_URL 使用
#   exam_migrator  一次性迁移专用 (DDL + DML)           -- EXAM_MIGRATOR_DATABASE_URL 使用
#
# 依赖环境变量 (来自 .env, 不入镜像): PG_APP_PASSWORD / PG_MIGRATOR_PASSWORD.
# 凭据只存在于 .env, 容器启动时注入, 不写盘/不进镜像.
set -euo pipefail

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
  -- 1. 角色 (重入安全: 已存在则跳过)
  DO \$\$
  BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'exam_app') THEN
      CREATE ROLE exam_app LOGIN PASSWORD '${PG_APP_PASSWORD:?PG_APP_PASSWORD 必填}';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'exam_migrator') THEN
      CREATE ROLE exam_migrator LOGIN PASSWORD '${PG_MIGRATOR_PASSWORD:?PG_MIGRATOR_PASSWORD 必填}';
    END IF;
  END \$\$;

  -- 2. 连接与 schema 权限 (PG15+ public 归 pg_database_owner, superuser 可授)
  GRANT CONNECT ON DATABASE "$POSTGRES_DB" TO exam_app;
  GRANT USAGE ON SCHEMA public TO exam_app;

  GRANT CONNECT ON DATABASE "$POSTGRES_DB" TO exam_migrator;
  GRANT USAGE, CREATE ON SCHEMA public TO exam_migrator;
  GRANT CREATE ON DATABASE "$POSTGRES_DB" TO exam_migrator;

  -- 3. 迁移 (exam_migrator) 新建的表/序列自动授给 exam_app (运行态只 DML)
  ALTER DEFAULT PRIVILEGES FOR ROLE exam_migrator IN SCHEMA public
      GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO exam_app;
  ALTER DEFAULT PRIVILEGES FOR ROLE exam_migrator IN SCHEMA public
      GRANT USAGE, SELECT ON SEQUENCES TO exam_app;
EOSQL

echo "pg-bootstrap-prod: 最小权限角色已就绪 (exam_app / exam_migrator)"
