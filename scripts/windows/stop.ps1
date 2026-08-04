# stop.ps1 (Task 12 Step 4): 停止 Windows staging.
# 顺序: 先停 Worker 再停 API (避免 Worker 在 API 失联时仍在写 PG / claim job 触发 fencing 异常).
[CmdletBinding()]
param(
    [string]$ApiTaskName = "ExamSystemGoAPI",
    [string]$WorkerTaskName = "ExamSystemGoScoringWorker",
    [int]$StopGraceSec = 10
)
$ErrorActionPreference = 'Stop'
# 1) 先停 Worker (Stop-ScheduledTask 不 kill 进程, 让它接到 stop 信号优雅退出)
$worker = Get-ScheduledTask -TaskName $WorkerTaskName -ErrorAction SilentlyContinue
if (-not $worker) {
    Write-Warning "Worker ScheduledTask '$WorkerTaskName' 不存在; 跳过"
} else {
    Stop-ScheduledTask -TaskName $WorkerTaskName -ErrorAction SilentlyContinue
    Write-Host "==> Worker 任务 '$WorkerTaskName' 停止信号发出"
    Start-Sleep -Seconds $StopGraceSec
}
# 2) 再停 API (允许 finalize 收卷 scan 循环优雅结束)
$api = Get-ScheduledTask -TaskName $ApiTaskName -ErrorAction SilentlyContinue
if (-not $api) {
    Write-Warning "API ScheduledTask '$ApiTaskName' 不存在; 跳过"
} else {
    Stop-ScheduledTask -TaskName $ApiTaskName -ErrorAction SilentlyContinue
    Write-Host "==> API 任务 '$ApiTaskName' 停止信号发出"
}
Write-Host "==> stop.ps1 完成 (Worker 先停 + API 后停)"
