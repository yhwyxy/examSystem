// Package submissions 复刻 Python insert_submission_pending + insert_grading_job
// (事务外只读 + 短事务内写), 由 Task 6 实现。
//
// 包边界 (Task 6 设计):
//   - doc.go:        公开类型 (Session, RunLite, Submission, SubmitRequest, SubmitResult, PreparedSubmission).
//   - repository.go: PG 层, 实现 sessions.SubmissionStore (新签名) + GetStatus.
//   - service.go:    Submit 编排 + 适配 sessions 包内 SubmissionStore 接口.
//   - service_test.go / repository_test.go: 集成测试 (testutil postgres).
//
// 依赖单向: submissions -> {papers, objective, sessions} 在编译期为 prohibited;
// 这里只 papers / objective 单向; sessions 包依赖 submissions 包, 不反向.
package submissions
