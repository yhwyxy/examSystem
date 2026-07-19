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
    render_question = function_body(source, "function renderQuestion", "function renderExam")

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
    assert "item.code_language" not in collect_editor


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
