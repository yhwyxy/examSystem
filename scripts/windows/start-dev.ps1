# start-dev.ps1: Windows 本地开发环境启动
# 启动 Go API 服务（可选启动评分 Worker）
[CmdletBinding()]
param(
    [string]$ConfigFile = "config.dev.yaml",
    [switch]$StartWorker,
    [switch]$Background
)
$ErrorActionPreference = 'Stop'

# 1) 检查数据库连接
Write-Host "==> 检查数据库连接..."
$env:PGPASSWORD = "exam_app_dev"
$pgReady = & pg_isready -h 127.0.0.1 -p 5432 -U exam_app 2>&1
if ($LASTEXITCODE -ne 0) {
    Write-Error "数据库未就绪，请先运行 setup-dev.ps1"
    exit 1
}
Write-Host "==> 数据库连接正常"

# 2) 启动 Go API
Write-Host "==> 启动 Go API 服务..."
if ($Background) {
    # 后台启动
    $apiProcess = Start-Process -FilePath "go" `
        -ArgumentList "run", "cmd/exam-server/main.go", "serve", "--config", $ConfigFile `
        -PassThru -NoNewWindow
    Write-Host "==> API 后台启动，PID: $($apiProcess.Id)"
    
    # 等待健康检查
    Write-Host "==> 等待 API 健康检查..."
    $deadline = (Get-Date).AddSeconds(30)
    $healthy = $false
    while ((Get-Date) -lt $deadline) {
        try {
            $resp = Invoke-RestMethod -Uri "http://127.0.0.1:8000/api/health" -TimeoutSec 2
            if ($resp.status -eq "ok" -or $resp.ok -eq $true) {
                $healthy = $true
                Write-Host "==> API 健康检查通过"
                break
            }
        } catch {
            Start-Sleep -Milliseconds 500
        }
    }
    if (-not $healthy) {
        Write-Warning "API 健康检查超时，请手动检查"
    }
} else {
    # 前台启动（Ctrl+C 停止）
    & go run cmd/exam-server/main.go serve --config $ConfigFile
}

# 3) 可选：启动评分 Worker
if ($StartWorker) {
    Write-Host "==> 启动评分 Worker..."
    $env:DATABASE_URL = "postgres://exam_app:exam_app_dev@127.0.0.1:5432/exam_system"
    if ($Background) {
        $workerProcess = Start-Process -FilePath "python" `
            -ArgumentList "-m", "scoring_worker" `
            -PassThru -NoNewWindow
        Write-Host "==> Worker 后台启动，PID: $($workerProcess.Id)"
    } else {
        & python -m scoring_worker
    }
}
