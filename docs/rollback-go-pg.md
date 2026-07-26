# 回滚指南: Go + PostgreSQL 栈 (Task 12 Step 6)

回滚决策树 (按"是否已收新工作提交"分两类):

## 情况 A: 未切流 / 新版本未收任何工作提交 (灰度阶段发现故障)

1. stop.ps1 停 Go 栈.
2. 回退部署根到上一版本 dist 目录 (升级前应保留备份 `D:\exam-prev\`).
3. PG 数据回退: 用升级前 `pg_dump` 备份恢复 (若 schema 未变可跳过).
4. Python 旧栈保留在线? 默认**否** — 灰度阶段旧栈已下线, 回退到上一版本 Go 栈.
5. start.ps1 起, preflight + health 两道都绿才算回滚完成.

## 情况 B: 已收新工作提交 / 不可丢数据 (需代次切换)

不可用"覆盖旧 bin"粗暴回退 (会丢已收数据). 走代次切换流程:
1. stop.ps1 停 Go 栈 (停止接收新提交).
2. 用升级前 `pg_dump` 备份建一个**新代次库** exam_system_genN (保留当前代次).
3. migrations/ V_N+1 回退脚本上当前代次 (不 DROP, 仅打 supersede 标记).
4. 上一版本 bin 指向新代次库重起 (详见 plan Task 10 代次切换设计 A/A/A/A).
5. 切流后观测 24h, 无异常再归档当前代次库 (不立即 DROP).

## 回退包完整性核对 (必做)

回退前**强制**核对目标 dist 的 manifest.sha256, 防止备份包被篡改:

```powershell
$base = "D:\exam-prev"   # 上一版本备份
Get-Content "$base\manifest.sha256" | ForEach-Object {
    $parts = $_ -split '\s+', 2
    $expect = $parts[0]; $rel = $parts[1]
    $actual = (Get-FileHash "$base\$rel" -Algorithm SHA256).Hash
    if ($actual -ne $expect) { Write-Error "校验失败: $rel"; exit 1 }
}
Write-Host "回退包完整性 OK"
```

## 禁止事项

- **禁止**拷回 `backend/` 目录 (Python 旧栈源码, 与 Go 栈二进制不兼容, 拷回会污染部署根).
- **禁止**拷回 `*.db` / `*.db-journal` / `*.pyc` (SQLite 残留, 与 PG 栈无关, 误导运维).
- **禁止**在情况 B 直接 DROP 当前代次库 (已收工作提交 = 不可丢数据, 见上).
- **禁止**回滚后跳过 preflight / health 两道自检就放行流量.

## rollback-tools 与预切流备份分工

| 工具 / 流程 | 职责 |
|---|---|
| `scripts/windows/stop.ps1` | 停双服务 (算力归零) |
| `pg_dump` (升级前一次性) | 预切流备份 = 回退点, 不可省 |
| `manifest.sha256` 核对 | 回退包未被篡改 |
| `scoring_worker --preflight` | 回退后评分链路健康再确认 |
| Go `/api/health` | 回退后 API 就绪确认 |
