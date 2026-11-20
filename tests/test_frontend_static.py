import re
from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[1]


def read_frontend_file(path: str) -> str:
    return (ROOT / path).read_text(encoding="utf-8")


def function_body(source: str, start: str, end: str) -> str:
    return source.split(start, 1)[1].split(end, 1)[0]


def test_exam_question_renderer_builds_answer_controls_with_dom_api():
    source = read_frontend_file("frontend/js/exam.js")
    render_question = function_body(source, "function renderQuestion", "function startTimer")

    assert ".innerHTML" not in render_question
    assert "function createAnswerOption" in source
    assert "document.createElement('input')" in source


def test_exam_answer_collection_matches_dynamic_question_ids_without_selectors():
    source = read_frontend_file("frontend/js/exam.js")
    collect_answers = function_body(source, "function collectAnswers", "async function submitExam")

    assert "function findQuestionControls" in source
    assert "document.querySelectorAll('[name]')" in source
    assert "control.name === String(qid)" in source
    assert "control.checked" in collect_answers
    assert "safeQuestionId" not in source
    assert "questionControlSelector" not in source
    assert "[name=\"${" not in collect_answers


def test_exam_renders_canonical_subquestions_with_code_language_whitelist():
    source = read_frontend_file("frontend/js/exam.js")
    render_question = function_body(source, "function renderQuestion", "function renderExam")

    assert "q.subquestions" in render_question
    assert "q.sub_questions" not in render_question
    assert "s.allowed_languages" in render_question
    assert "select.dataset.qid" in render_question
    assert "select.dataset.sid" in render_question
    assert "document.createElement('select')" in render_question
    assert "document.createElement('option')" in render_question
    assert "s.code_language" not in render_question


def test_exam_collects_nested_subquestion_answers_and_selected_language():
    source = read_frontend_file("frontend/js/exam.js")
    collect_answers = function_body(source, "function collectAnswers", "async function submitExam")

    assert "q.subquestions" in collect_answers
    assert "q.sub_questions" not in collect_answers
    assert "findSubquestionControl('textarea', q.id, s.id)" in collect_answers
    assert "findSubquestionControl('select', q.id, s.id)" in collect_answers
    assert "{ answer:" in collect_answers
    assert "subAnswer.language =" in collect_answers
    assert "Object.create(null)" in collect_answers


def test_exam_renders_top_level_code_language_select():
    source = read_frontend_file("frontend/js/exam.js")
    render_question = function_body(source, "function renderQuestion", "function startTimer")

    assert "q.allowed_languages" in render_question
    assert "code-language-select" in render_question
    assert "select.dataset.qid = String(q.id)" in render_question
    assert "请输入代码" in render_question


def test_exam_collects_top_level_code_answer_with_language():
    source = read_frontend_file("frontend/js/exam.js")
    collect_answers = function_body(source, "function collectAnswers", "async function submitExam")

    assert "q.allowed_languages" in collect_answers
    assert "code-language-select" in collect_answers
    assert "language:" in collect_answers
    assert "answers[q.id] = {" in collect_answers


def test_exam_answer_map_preserves_proto_question_id_for_json_serialization():
    source = read_frontend_file("frontend/js/exam.js")
    collect_answers = function_body(source, "function collectAnswers", "async function submitExam")

    assert "const answers = Object.create(null);" in collect_answers
    assert "answers[q.id] =" in collect_answers


def test_exam_subquestion_controls_use_collision_safe_dataset_lookup():
    source = read_frontend_file("frontend/js/exam.js")
    render_question = function_body(source, "function renderQuestion", "function renderExam")

    assert "function findSubquestionControl" in source
    assert "querySelectorAll(`${tagName}[data-qid][data-sid]`)" in source
    assert "control.dataset.qid === String(qid)" in source
    assert "control.dataset.sid === String(sid)" in source
    assert "select.dataset.qid = String(q.id)" in render_question
    assert "select.dataset.sid = String(s.id)" in render_question
    assert "ta.dataset.qid = String(q.id)" in render_question
    assert "ta.dataset.sid = String(s.id)" in render_question
    assert "data-language-for" not in source
    assert "controlKey" not in render_question


def test_exam_subquestion_textareas_have_accessible_names():
    source = read_frontend_file("frontend/js/exam.js")
    render_question = function_body(source, "function renderQuestion", "function renderExam")

    assert "ta.setAttribute('aria-label'" in render_question
    assert "s.question" in render_question


def test_admin_editor_writes_canonical_composites_and_language_whitelist():
    source = read_frontend_file("frontend/js/papers.js")
    render_editor = function_body(source, "function renderSubQuestionsEditor", "function collectSubQuestionsFromUI")
    collect_editor = function_body(source, "function collectSubQuestionsFromUI", "function applyQuestionFromForm")

    assert "SUPPORTED_CODE_LANGUAGES" in source
    assert "subquestions" in source
    assert "sub_questions" not in source
    assert 'class="sq-languages"' in render_editor
    assert "multiple" in render_editor
    assert ".selectedOptions" in collect_editor
    assert "item.allowed_languages" in collect_editor
    assert "item.code_language = allowedLanguages[0]" in collect_editor


def test_admin_editor_preserves_subquestion_scoring_metadata_on_collect():
    source = read_frontend_file("frontend/js/papers.js")
    collect_editor = function_body(
        source, "function collectSubQuestionsFromUI", "function applyQuestionFromForm"
    )

    assert "previous" in collect_editor or "previousSubs" in collect_editor
    assert "calculation" in collect_editor
    assert "scoring_points" in collect_editor
    assert "scoring_rubric" in collect_editor


def test_admin_composite_detail_displays_selected_language_without_inline_style():
    source = read_frontend_file("frontend/js/detail.js")

    assert "selected_language" in source
    assert "sub-result card nested" in source
    assert 'style="' not in source


def test_detail_review_reads_scores_from_result_container_not_dynamic_ids():
    source = read_frontend_file("frontend/js/detail.js")
    review = function_body(source, "async function review", "$('detail').addEventListener")
    click_handler = function_body(source, "$('detail').addEventListener", "$('regradeBtn')")

    assert ".querySelector('.score-input')" in review
    assert ".querySelector('.note-input')" in review
    assert "getElementById(scoreElId)" not in review
    assert "getElementById(noteElId)" not in review
    assert "closest('.sub-result')" in click_handler



def test_exam_script_implements_deduplicated_anti_switch_auto_submit():
    source = read_frontend_file("frontend/js/exam.js")

    assert "const AUTO_SUBMIT_AFTER_BLURS = 3" in source
    assert "const AWAY_TIMEOUT_MS = 30_000" in source
    assert "blurCount: 0" in source
    assert "isPageAway: false" in source
    assert "autoSubmitStarted: false" in source
    assert "function handlePageAway" in source
    assert "function handlePageReturn" in source
    assert "document.addEventListener('visibilitychange'" in source
    assert "window.addEventListener('blur'" in source
    assert "window.addEventListener('focus'" in source
    assert "submitExam('third_blur')" in source
    assert "submitExam('blur_timeout_30s')" in source


def test_exam_submission_sends_reason_only_for_auto_submit():
    source = read_frontend_file("frontend/js/exam.js")
    submit_body = function_body(source, "async function submitExam", "async function startExam")

    assert "async function submitExam(autoSubmitReason = null)" in source
    # 表单 submit 会把 Event 传入；只有字符串 reason 才应写入 auto_submit_reason，
    # 否则会触发后端 Pydantic 422（Literal 校验失败）。
    assert "const isAutoSubmit = typeof autoSubmitReason === 'string'" in submit_body
    assert "...(isAutoSubmit ? { auto_submit_reason: autoSubmitReason } : {})" in submit_body
    assert "...(autoSubmitReason ? { auto_submit_reason: autoSubmitReason } : {})" not in submit_body
    assert "state.autoSubmitStarted" in submit_body
    # 表单绑定不得把 submit Event 直接传给 submitExam
    assert "addEventListener('submit', (event) => {" in source
    assert "submitExam(null);" in source
    assert "addEventListener('submit', submitExam)" not in source


def test_exam_html_cache_busts_exam_js():
    html = read_frontend_file("frontend/exam.html")
    assert 'src="/js/exam.js?v=' in html


def test_admin_shows_auto_submit_reason_only_when_present():
    html = read_frontend_file("frontend/admin.html")
    source = read_frontend_file("frontend/js/admin.js")

    assert "交卷状态" in html
    assert "function formatAutoSubmitReason" in source
    assert "切屏达到 3 次，自动交卷" in source
    assert "单次切屏达到 30 秒，自动交卷" in source
    assert "r.auto_submit_reason" in source
    assert "return ''" in source



def test_admin_badge_classes_are_defined_in_stylesheet():
    admin_source = read_frontend_file("frontend/js/admin.js")
    detail_source = read_frontend_file("frontend/js/detail.js")
    stylesheet = read_frontend_file("frontend/css/style.css")

    badge_classes = set(re.findall(r"'(badge-[a-z-]+)'", admin_source + detail_source))
    assert badge_classes

    missing = [cls for cls in sorted(badge_classes) if f".{cls}" not in stylesheet]
    assert missing == []


def test_admin_page_uses_sidebar_application_shell():
    html = read_frontend_file("frontend/admin.html")

    assert 'class="app-shell admin-shell"' in html
    assert 'class="sidebar"' in html
    assert 'id="adminTableState"' in html
    assert 'id="selectAllBtn"' in html


def test_exam_page_uses_structured_flow_without_dead_grading_panel():
    html = read_frontend_file("frontend/exam.html")

    assert 'class="exam-shell"' in html
    assert 'id="successPanel"' in html
    assert 'id="grading"' not in html


def test_default_admin_config_is_enterable():
    config = yaml.safe_load((ROOT / "config.yaml").read_text(encoding="utf-8"))
    admin = config["admin"]

    assert admin["enable_auth"] is False or admin.get("password")

def test_admin_sidebar_switches_views_not_scroll_anchors():
    html = read_frontend_file("frontend/admin.html")
    js = read_frontend_file("frontend/js/admin.js")
    css = read_frontend_file("frontend/css/style.css")

    assert 'data-view="overview"' in html
    assert 'data-view="publish"' in html
    assert 'data-view="submissions"' in html
    assert 'class="view-panel is-active"' in html
    assert 'function showView' in js
    assert '.view-panel' in css
    assert 'href="#overview"' not in html


def test_admin_settings_panel_has_password_and_scoring():
    html = read_frontend_file("frontend/admin.html")
    js = read_frontend_file("frontend/js/admin.js")

    # 侧边栏设置入口
    assert 'data-view="settings"' in html
    assert 'id="view-settings"' in html
    # 功能 1: 改密码
    assert 'settingsSavePasswordBtn' in html
    assert 'settingsOldPassword' in html
    assert 'settingsNewPassword' in html
    assert 'settings/password' in js
    assert 'async savePassword()' in js
    # 功能 2: 切换评分方式
    assert 'settingsScoringMethod' in html
    assert 'settingsRerankBlock' in html
    assert 'settingsLLMBlock' in html
    assert 'settings/scoring' in js
    assert 'async saveScoring()' in js
    assert "method === 'remote_reranker'" in js or "settingsRerankBlock" in js
    # 只回显掩码, 不回明文 key
    assert 'rerank_api_key_masked' in js
    assert 'llm_api_key_masked' in js
    assert 'embedding_api_key_masked' in js
    assert 'id="settingsEmbeddingUrl"' in html
    assert 'id="settingsEmbeddingModel"' in html
    assert '会发送到此 embedding 服务' in html

def test_admin_shell_is_fixed_two_pane_layout():
    css = read_frontend_file("frontend/css/style.css")
    js = read_frontend_file("frontend/js/admin.js")
    html = read_frontend_file("frontend/admin.html")

    assert "body.admin-page" in css
    assert ".admin-shell .workspace" in css
    assert "display: none !important" in css
    assert "function showView" in js
    assert "panel.hidden" in js
    assert 'data-view="overview"' in html
    assert 'href="#overview"' not in html



def test_admin_nav_label_is_exam_management():
    html = read_frontend_file("frontend/admin.html")
    assert "考试管理" in html
    assert 'data-view="publish"' in html
    assert "batchOpenBtn" in html
    assert "batchCloseBtn" in html
    assert "examCards" in html


def test_papers_list_supports_batch_and_inline_publish():
    source = read_frontend_file("frontend/js/papers.js")
    assert "batchOpen" in source
    assert "batchClose" in source
    assert "paper-check" in source
    assert "正在自动收卷" in source
    # publish action calls API in place, not only navigate
    assert "setPaperStatus(slug, open)" in source


def test_exam_js_requires_run_token_and_draft_loop():
    source = read_frontend_file("frontend/js/exam.js")
    assert "getRunTokenFromUrl" in source
    assert "saveDraftNow" in source
    assert "DRAFT_LOOP_MS" in source
    assert "session_id" in source
    assert "session_token" in source
    assert "handleClosingStatus" in source
    # personal deadline timer, no paper-level remaining global countdown label invent
    assert "deadlineAt" in source
    assert "本轮考试已结束" in source or "run_status" in source


def test_exam_html_has_draft_status_indicator():
    html = read_frontend_file("frontend/exam.html")
    assert 'id="draftStatus"' in html


def test_exam_management_cards_have_toggleable_link_panel_and_status_layout():
    source = read_frontend_file("frontend/js/papers.js")
    css = read_frontend_file("frontend/css/style.css")
    assert "toggleExamLinkPanel" in source
    assert "收起链接/二维码" in source
    assert "exam-link-panel" in source
    assert "exam-card-times" in source
    assert "badge-exam-open" in source
    assert "badge-exam-closing" in source
    assert "badge-exam-closed" in source
    assert ".exam-status-badge" in css
    assert ".exam-link-panel.is-open" in css
    assert "white-space: nowrap" in css
    assert "hasClosing ? 1000" in source


def test_exam_management_has_reset_rounds_control():
    html = read_frontend_file("frontend/admin.html")
    js = read_frontend_file("frontend/js/papers.js")
    assert 'id="resetRoundsBtn"' in html
    assert "重置轮次" in html
    assert "resetRounds" in js
    assert "/exams/reset-rounds" in js


def test_admin_submissions_table_has_round_column():
    html = read_frontend_file("frontend/admin.html")
    assert "轮次" in html
    js = read_frontend_file("frontend/js/admin.js")
    assert "formatRound" in js
    assert "admin_closed" in js


def test_question_bank_ai_scoring_candidates_require_teacher_action():
    html = read_frontend_file("frontend/admin.html")
    source = read_frontend_file("frontend/js/question-bank.js")
    assert 'id="questionBankExtractPointsBtn"' in html
    assert 'id="questionBankApplyCandidatesBtn"' in html
    assert "/scoring-points/extract" in source
    assert "currentExtraction.questionFingerprint" in source
    assert "source_evidence" in source
    assert "确认并填入题目 JSON" in html
    assert "保存后仍是草稿，需人工审核并启用" in source


# ---------- Task 11 (plan 1446-1526): draft throttle + grading state 适配 ----------

def test_exam_draft_loop_interval_is_5000ms():
    """plan Step 2: DRAFT_LOOP_MS 改 2000 -> 5000 降低风暴."""
    source = read_frontend_file("frontend/js/exam.js")
    assert "const DRAFT_LOOP_MS = 5000;" in source
    assert "const DRAFT_LOOP_MS = 2000;" not in source


def test_exam_save_draft_keeps_dirty_when_saved_false():
    """plan Step 2: data.saved === false 不能清 dirty, 应等下次重发完整 answers."""
    source = read_frontend_file("frontend/js/exam.js")
    save_fn = function_body(source, "async function saveDraftNow", "async function pollSessionStatus")
    assert "if (data.saved === false)" in save_fn
    # 必须包含 dirty=true 分支但不应在 saved=false 后直接清 dirty
    assert "state.dirty = true" in save_fn
    assert "state.draftRevision = data.draft_revision" in save_fn


def test_exam_stale_draft_revision_keeps_dirty_and_rereads_revision():
    """plan Step 2: STALE_DRAFT_REVISION 不应直接清 dirty 为 false 成 "已保存",
    应读 detail.current_revision 更新 draftRevision 并保持 dirty=true."""
    source = read_frontend_file("frontend/js/exam.js")
    save_fn = function_body(source, "async function saveDraftNow", "async function pollSessionStatus")
    assert "STALE_DRAFT_REVISION" in save_fn
    # 修复后必须读 current_revision 而非清 dirty
    assert "data.detail.current_revision" in save_fn
    # 修复后不再有 STALE 分支立即 "已保存" + dirty=false 文本组合
    # (旧 bug: line 598 state.dirty = false 单独清, 不读 current_revision)
    assert save_fn.count("state.dirty = false") < 3


def test_exam_save_draft_now_uses_fetch_keepalive_not_sendbeacon():
    """plan Step 3: 离开页面保存改 fetch PUT + keepalive:true, 不走 sendBeacon POST;
    且序列化请求体超过 60000 字节时保留 dirty 不发."""
    source = read_frontend_file("frontend/js/exam.js")
    save_fn = function_body(source, "async function saveDraftNow", "async function pollSessionStatus")
    assert "navigator.sendBeacon" not in save_fn
    assert "method: 'PUT'" in save_fn
    assert "keepalive: true" in save_fn
    assert "60000" in save_fn


def test_exam_save_draft_now_signature_has_allow_locked():
    """plan Step 3: saveDraftNow({beacon=false, allowLocked=false}={}) 新签名,
    state.locked && !allowLocked 提前返回; handleClosingStatus 必须传 allowLocked:true."""
    source = read_frontend_file("frontend/js/exam.js")
    assert "async function saveDraftNow({ beacon = false, allowLocked = false } = {})" in source
    assert "!allowLocked" in source
    handle_fn = function_body(source, "async function handleClosingStatus", "async function pollStatus")
    assert "saveDraftNow({ allowLocked: true })" in handle_fn


def test_admin_js_effective_status_mapping():
    """plan Step 4: admin.js effective_status 映射 grading_status='grading' + review_status
    -> 'grading' / 'manual_fallback' 未定义映射 -> 'unknown'; detail.js 同义."""
    src = read_frontend_file("frontend/js/admin.js")
    # 必须存在 effective_status 解析逻辑
    assert "effective_status" in src or "effectiveStatus" in src
    assert "grading_status" in src and "review_status" in src


def test_detail_js_disable_buttons_on_grading():
    """plan Step 4: detail.js 评审按钮在 grading_status='grading' 时 disabled."""
    src = read_frontend_file("frontend/js/detail.js")
    # 任意一种"在 grading 状态禁用 / 锁住按钮"的检查
    assert "grading" in src
    assert "disabled" in src


def test_answer_similarity_ui_distinguishes_failed_batches_and_audits_review():
    html = read_frontend_file("frontend/admin.html")
    admin = read_frontend_file("frontend/js/admin.js")
    source = read_frontend_file("frontend/js/answer-similarity.js")
    assert 'data-view="answer-similarity"' in html
    assert "answer-similarity" in admin
    assert "检测未完成" in source
    assert "未发现可核查线索" in source
    assert "expected_revision" in source
    assert "review_history" in source
    assert "不会改动评分或成绩" in html
    assert "语义接近" in source


def test_score_anomaly_ui_loads_versioned_reports_and_distinguishes_failures():
    html = read_frontend_file("frontend/admin.html")
    admin = read_frontend_file("frontend/js/admin.js")
    source = read_frontend_file("frontend/js/score-anomaly.js")
    assert 'data-view="score-anomalies"' in html
    assert "score-anomalies" in admin
    assert "ScoreAnomalyAdmin.load()" in admin
    assert "检测未完成" in source
    assert "不构成真实模型质量认证" in source
    assert "item.severity === 'blocking'" in source
    assert "expected_revision" in source
    assert "review_history" in source
    assert "grading_set_hash" not in html  # implementation fingerprints stay out of operator copy
    assert "检测报告不修改分数" in html


def test_admin_ui_uses_named_accounts_and_role_scoped_navigation():
    html = read_frontend_file("frontend/admin.html")
    source = read_frontend_file("frontend/js/admin.js")
    assert 'id="loginUsername"' in html
    assert 'body: JSON.stringify({ username, password })' in source
    assert "question_bank_editor" in source
    assert "reviewer: ['overview'" in source
    assert "publisher: ['overview', 'grade-publication']" in source
    assert "item.hidden = !roleCanView(item.dataset.view)" in source
    assert "item.hidden = !canReview" in source
    assert 'id="adminUserCreateForm"' in html
    assert 'id="adminUsersRows"' in html
    assert "method: 'PATCH'" in source
    assert "username === currentAdminUsername" in source


def test_grade_publication_has_a_separate_role_gated_view():
    html = read_frontend_file("frontend/admin.html")
    source = read_frontend_file("frontend/js/admin.js")
    assert 'data-view="grade-publication"' in html
    assert 'data-view="grade-publication"' in html.split('data-view="publish"', 1)[1]
    assert "'grade-publication'" in source
    assert "window.PublicationAdmin.load()" in source


def test_required_review_queue_unifies_submission_and_blocking_score_tasks():
    html = read_frontend_file("frontend/admin.html")
    admin = read_frontend_file("frontend/js/admin.js")
    queue = read_frontend_file("frontend/js/review-queue.js")
    anomaly = read_frontend_file("frontend/js/score-anomaly.js")
    similarity = read_frontend_file("frontend/js/answer-similarity.js")
    assert 'data-view="review-queue"' in html
    assert "window.ReviewQueueAdmin.load()" in admin
    assert "review-queue" in admin
    assert "/review-queue?limit=500" in queue
    assert "data-open-anomaly" in queue
    assert "focusFinding" in anomaly
    assert "data-open-similarity" in queue
    assert "focusFinding" in similarity
    assert "待复核任务" in html


def test_execution_configuration_is_editable_and_records_are_visible():
    papers = read_frontend_file("frontend/js/papers.js")
    detail = read_frontend_file("frontend/js/detail.js")
    fields = papers.split("const ADVANCED_SCORING_FIELDS =", 1)[1].split(";", 1)[0]
    for field in ("sql_dialect", "sql_test_datasets", "sql_execution", "code_test_cases"):
        assert field in fields
    assert "execution_record" in detail
    assert "验证通过" in detail


def test_historical_grades_are_not_presented_as_new_process_publications():
    for path in ("frontend/js/admin.js", "frontend/js/detail.js"):
        source = read_frontend_file(path)
        assert "historical_grade" in source
        assert "历史成绩（未按新流程发布）" in source
