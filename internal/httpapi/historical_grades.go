package httpapi

// A completed score predating generation history remains historical until its
// current generation has a new-process history record. This is a display label,
// never a substitute for review, detection or publication gates.
const historicalGradeSQL = `(r.id IS NULL OR r.is_legacy OR (
s.grading_status='done'
AND coalesce(s.graded_at,s.submitted_at) <= coalesce(
    (SELECT applied_at FROM schema_migrations WHERE version='0003_grading_history'),'-infinity'::timestamptz)
AND NOT EXISTS(SELECT 1 FROM grading_history h WHERE h.submission_id=s.id AND h.generation=s.grading_generation)
))`
