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


def test_exam_answer_collection_escapes_dynamic_question_ids():
    source = read_frontend_file("frontend/js/exam.js")
    collect_answers = function_body(source, "function collectAnswers", "async function submitExam")

    assert "safeQuestionId" in source
    assert "CSS.escape" in source
    assert '`input[name="${q.id}"]:checked`' not in collect_answers
    assert '`[name="${q.id}"]`' not in collect_answers



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
    assert "...(autoSubmitReason ? { auto_submit_reason: autoSubmitReason } : {})" in submit_body
    assert "state.autoSubmitStarted" in submit_body

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

