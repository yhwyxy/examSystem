// Package finalize 复现 Python close_run + run_recover_loop:
//
//	ScanDue   — 选取 finalize_at <= now 的 closing run (FOR UPDATE SKIP LOCKED)
//	FinalizeRun — 在单事务内: lock run -> 校验 snapshot -> 并发安全地对剩
//	              余 active sessions 调 Task 6 的 CreateFromSessionTx (auto_submit_reason=admin_closed)
//	              -> run status closing->closed -> commit. 任一错全滚, run 保持 closing 等下次.
//
// 生命周期由调用方 (cmd/exam-server) 安排: 启动后先 ScanDue 一次, 每秒轮询;
// 关闭时 cancel context 并等待当前事务结束最长 15s.
package finalize
