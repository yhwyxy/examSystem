# start.ps1 (Task 12 Step 4): 启动 Windows staging (New-ScheduledTask, 不用 New-Service).
# 顺序: 先启 API 任务并轮询 /api/health 30s 等健康, 再启 Worker 任务.
[CmdletBinding()]
param(
    [string]$InstallRoot = ".",
    [int]$HealthTimeoutSec = 30,
    [int]$HealthIntervalMs = 500,
    [string]$ApiTaskName = "ExamSystemGoAPI",
    [string]$WorkerTaskName = "ExamSystemGoScoringWorker",
    [int]$ApiPort = 8000,
    [string]$ApiHost = "127.0.0.1"
)
$ErrorActionPreference = 'Stop'
# 1) 启动 API 任务 (ScheduledTask 需提前一次性创建, 命令见 docs/deployment-go-pg.md)
$apiTask = Get-ScheduledTask -TaskName $ApiTaskName -ErrorAction SilentlyContinue
if (-not $apiTask) {
    Write-Error "API ScheduledTask '$ApiTaskName' 不存在; 用 New-ScheduledTask 一次性创建 (见 docs/deployment-go-pg.md)"
    exit 1
}
Start-ScheduledTask -TaskName $ApiTaskName
Write-Host "==> API task '$ApiTaskName' 启动信号发出, 等 /api/health (最多 $HealthTimeoutSec s)"
# 2) 轮询 /api/health 最多 $HealthTimeoutSec 秒
$deadline = (Get-Date).AddSeconds($HealthTimeoutSec)
$healthy = $false
$healthUrl = "http://${ApiHost}:$ApiPort/api/health"
while ((Get-Date) -lt $deadline) {
    try {
        $resp = Invoke-RestMethod -Uri $healthUrl -TimeoutSec 2 -ErrorAction Stop
        if ($resp.status -eq "ok" -or $resp.ok -eq $true) {
            $healthy = $true
            Write-Host "==> API healthy: $($resp | ConvertTo-Json -Compress)"
            break
        }
    } catch {
        Start-Sleep -Milliseconds $HealthIntervalMs
    }
}
if (-not $healthy) {
    Write-Error "API 在 ${HealthTimeoutSec}s 内未健康; 中止启动 Worker"
    exit 1
}
# 3) API 健康后再启 Worker (避免 Worker 启动早于 API 时调 wrong endpoint)
$workerTask = Get-ScheduledTask -TaskName $WorkerTaskName -ErrorAction SilentlyContinue
if (-not $workerTask) {
    Write-Warning "Worker ScheduledTask '$WorkerTaskName' 不存在; API 已启动但 Worker 未启动; 用 New-ScheduledTask 一次性创建 (见 docs/deployment-go-pg.md)"
    exit 0
}
Start-ScheduledTask -TaskName $WorkerTaskName
Write-Host "==> Worker task '$WorkerTaskName' 启动信号发出"
Write-Host "==> start.ps1 完成 (API healthy + Worker 启动)"
