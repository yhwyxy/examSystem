// Package jobs - service.go
//
// Service 提供两个 Enqueue 入口:
//   - EnqueueTx(ctx, tx, subID string, payload []byte) error
//     实现 sessions.JobEnqueuer 兼容签名. payload 是 JSON 编码的 JobSpec.
//     sessions.SubmitManual (deprecated path) 走这里.
//   - EnqueueJobTx(ctx, tx, subID int64, spec JobSpec) error
//     结构化主接口 (Task 6 HTTP 主路径 + 未来 Task 7 worker 回填用).
//
// sessions 包不依赖 jobs 包; jobs 也不依赖 sessions. Service 同时实现
// 既有 sessions.JobEnqueuer 字符串接口.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// ErrNotConfigured 表示 service 未正确装配 (nil receiver 或 nil repo).
var ErrNotConfigured = errors.New("JOBS_NOT_CONFIGURED")

// ErrBadPayload 表示 payload JSON 解析失败或字段缺失.
var ErrBadPayload = errors.New("JOBS_BAD_PAYLOAD")

// payloadSpec 是 sessions.JobEnqueuer 入口 EnqueueTx 的 payload 解码结果.
type payloadSpec struct {
	PaperID    string `json:"paper_id"`
	RunID      string `json:"run_id"`
	Generation int64  `json:"generation"`
}

// Service 包装 Repository; 实现 sessions.JobEnqueuer (字符串 EnqueueTx) +
// 结构化 EnqueueJobTx.
type Service struct {
	repo *Repository
}

// NewService 返回基于默认 Repository 的 Service.
func NewService(repo *Repository) *Service {
	if repo == nil {
		repo = NewRepository()
	}
	return &Service{repo: repo}
}

// EnqueueTx 实现 sessions.JobEnqueuer 兼容签名: payload 是 JobSpec JSON.
// submissionID string -> int64; generation 缺省为 1.
func (s *Service) EnqueueTx(ctx context.Context, tx pgx.Tx, submissionID string,
	payload []byte) error {
	if s == nil || s.repo == nil {
		return ErrNotConfigured
	}
	subID, err := strconv.ParseInt(submissionID, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: submission_id=%q", ErrBadPayload, submissionID)
	}
	spec := JobSpec{Generation: 1}
	if len(payload) > 0 {
		var ps payloadSpec
		if err := json.Unmarshal(payload, &ps); err != nil {
			return fmt.Errorf("%w: %v", ErrBadPayload, err)
		}
		spec.PaperID = ps.PaperID
		spec.RunID = ps.RunID
		if ps.Generation > 0 {
			spec.Generation = ps.Generation
		}
	}
	spec.SubmissionID = subID
	return s.repo.EnqueueTx(ctx, tx, spec)
}

// EnqueueJobTx 是结构化主接口 (新路径). 不解析 JSON, 直接接 JobSpec.
func (s *Service) EnqueueJobTx(ctx context.Context, tx pgx.Tx, submissionID int64,
	spec JobSpec) error {
	if s == nil || s.repo == nil {
		return ErrNotConfigured
	}
	spec.SubmissionID = submissionID
	return s.repo.EnqueueTx(ctx, tx, spec)
}
