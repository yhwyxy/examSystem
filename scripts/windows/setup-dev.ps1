# setup-dev.ps1: Windows 本地开发环境初始化
# 首次使用时运行一次，创建数据库用户、库和表结构
[CmdletBinding()]
param(
    [string]$PgHost = "127.0.0.1",
    [int]$PgPort = 5432,
    [string]$SuperUser = "postgres",
    [string]$SuperPassword = "",
    [string]$ConfigFile = "config.dev.yaml"
)
$ErrorActionPreference = 'Stop'

# 1) 检查 PostgreSQL 是否可用
Write-Host "==> 检查 PostgreSQL 连接..."
$env:PGPASSWORD = $SuperPassword
$pgReady = & pg_isready -h $PgHost -p $PgPort -U $SuperUser 2>&1
if ($LASTEXITCODE -ne 0) {
    Write-Error "PostgreSQL 未运行或连接失败: $pgReady"
    Write-Host "请确保 PostgreSQL 已启动，或检查端口和凭据"
    exit 1
}
Write-Host "==> PostgreSQL 连接正常"

# 2) 执行 pg-bootstrap.sql（创建用户和数据库）
Write-Host "==> 初始化用户和数据库..."
$bootstrapSql = Join-Path $PSScriptRoot "..\pg-bootstrap.sql"
if (Test-Path $bootstrapSql) {
    & psql -h $PgHost -p $PgPort -U $SuperUser -d postgres -f $bootstrapSql
    if ($LASTEXITCODE -ne 0) {
        Write-Error "pg-bootstrap.sql 执行失败"
        exit 1
    }
    Write-Host "==> 用户和数据库创建完成"
} else {
    Write-Warning "pg-bootstrap.sql 未找到，跳过用户初始化"
}

# 3) 运行 Go 迁移（建表）
Write-Host "==> 运行数据库迁移..."
$env:PGPASSWORD = "exam_migrator_dev"
& go run cmd/exam-server/main.go migrate --config $ConfigFile
if ($LASTEXITCODE -ne 0) {
    Write-Error "数据库迁移失败"
    exit 1
}
Write-Host "==> 迁移完成"

# 4) 创建数据目录
$dataDirs = @("data/papers", "data/exam_runs", "data/snapshots")
foreach ($dir in $dataDirs) {
    if (-not (Test-Path $dir)) {
        New-Item -ItemType Directory -Path $dir -Force | Out-Null
        Write-Host "==> 创建目录: $dir"
    }
}

Write-Host ""
Write-Host "==> 开发环境初始化完成！"
Write-Host ""
Write-Host "后续步骤："
Write-Host "  1. 编辑 config.dev.yaml 填入数据库密码（如需修改）"
Write-Host "  2. 运行 .\scripts\windows\start-dev.ps1 启动服务"
