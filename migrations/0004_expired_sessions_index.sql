-- 支持后台按个人截止时间扫描仍未提交的会话。
CREATE INDEX idx_exam_sessions_active_deadline
    ON exam_sessions (deadline_at, id)
    WHERE status = 'active';
