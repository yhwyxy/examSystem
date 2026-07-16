import json
from types import SimpleNamespace

from scripts import word_exam_converter as converter
from scripts.word_exam_converter import (
    convert_directory,
    extract_with_officecli,
    normalize_extracted_lines,
    parse_answer_token,
    parse_exam_lines,
    parse_section_header,
    split_options,
)


def test_normalize_extracted_lines_removes_officecli_paths():
    text = "[/body/p[1]] 标题\n[/body/p[2]] 1、题目"

    assert normalize_extracted_lines(text) == ["标题", "1、题目"]


def test_parse_section_header_reads_type_and_score():
    section = parse_section_header("二、单项选择题（每题1.5分，共30分）")

    assert section is not None
    assert section.question_type == "single_choice"
    assert section.default_score == 1.5


def test_parse_answer_token_normalizes_objective_answers():
    assert parse_answer_token("√", "true_false") is True
    assert parse_answer_token("×", "true_false") is False
    assert parse_answer_token(" A、C ", "multiple_choice") == ["A", "C"]


def test_split_options_handles_inline_options():
    stem, options = split_options("温度如何变化 A.升高 B.降低 C.不变 D.改变")

    assert stem == "温度如何变化"
    assert options == [
        {"key": "A", "text": "升高"},
        {"key": "B", "text": "降低"},
        {"key": "C", "text": "不变"},
        {"key": "D", "text": "改变"},
    ]


def test_parse_exam_lines_builds_true_false_and_choice_questions():
    lines = [
        "化工专业考试试卷",
        "一、判断题（每题1分，共2分）",
        "1、理想气体混合物是一种理想溶液。（√）",
        "2、催化剂能使不可能反应发生。（×）",
        "二、单项选择题（每题2分，共2分）",
        "1、吸热反应升温是否有利（A） A.有利 B.不利 C.无关 D.不确定",
    ]

    result = parse_exam_lines("试卷（化工）.docx", lines)

    assert [q["type"] for q in result.paper["questions"]] == [
        "true_false",
        "true_false",
        "single_choice",
    ]
    assert [q["answer"] for q in result.paper["questions"]] == [True, False, "A"]
    assert result.paper["exam_info"]["total_score"] == 4


def test_parse_exam_lines_collects_multiline_subjective_answer():
    lines = [
        "专业试卷",
        "四、简答题（每题10分，共10分）",
        "1、影响反应速率的因素有哪些？（10分）",
        "答：温度、浓度和催化剂。",
        "提高温度或加入正催化剂可加速反应。",
    ]

    result = parse_exam_lines("试卷（化工）.docx", lines)

    question = result.paper["questions"][0]
    assert question["type"] == "short_answer"
    assert question["answer"] == (
        "温度、浓度和催化剂。\n提高温度或加入正催化剂可加速反应。"
    )


def test_parse_exam_lines_marks_missing_answer_for_review():
    result = parse_exam_lines(
        "试卷（软件开发）.docx",
        [
            "软件开发试卷",
            "一、单选题（每题2分）",
            "1、HTTP 默认端口是（ ） A.80 B.443",
        ],
    )

    assert result.issues[0].code == "missing_answer"


def test_parse_exam_lines_accepts_multiple_choice_answer_list():
    result = parse_exam_lines(
        "试卷（化工）.docx",
        [
            "化工试卷",
            "三、多项选择题（每题2分）",
            "1、常用冷却介质有（A、C） A.低温水 B.蒸汽 C.盐水 D.热油",
        ],
    )

    assert result.paper["questions"][0]["answer"] == ["A", "C"]
    assert result.issues == []


def test_extract_with_officecli_reads_docx_directly(tmp_path):
    source = tmp_path / "试卷.docx"
    calls = []

    def runner(args, **kwargs):
        calls.append((args, kwargs))
        return SimpleNamespace(stdout="[/body/p[1]] 标题\n")

    text = extract_with_officecli(source, tmp_path, runner=runner)

    assert text == "[/body/p[1]] 标题\n"
    assert calls[0][0] == ["officecli", "view", str(source), "text"]


def test_extract_with_officecli_converts_doc_before_reading(tmp_path):
    source = tmp_path / "旧试卷.doc"
    calls = []

    def runner(args, **kwargs):
        calls.append((args, kwargs))
        return SimpleNamespace(stdout="[/body/p[1]] 标题\n")

    extract_with_officecli(source, tmp_path, runner=runner)

    converted = tmp_path / "旧试卷.docx"
    assert calls[0][0] == [
        "textutil",
        "-convert",
        "docx",
        "-output",
        str(converted),
        str(source),
    ]
    assert calls[1][0] == ["officecli", "view", str(converted), "text"]


def test_convert_directory_writes_valid_utf8_json(tmp_path):
    source_dir = tmp_path / "source"
    output_dir = tmp_path / "json"
    source_dir.mkdir()
    (source_dir / "试卷（化工）.docx").touch()
    extracted = "\n".join(
        [
            "[/body/p[1]] 化工试卷",
            "[/body/p[2]] 一、判断题（每题1分）",
            "[/body/p[3]] 1、理想气体混合物是一种理想溶液。（√）",
        ]
    )

    report = convert_directory(
        source_dir,
        output_dir,
        extractor=lambda source, temp_dir: extracted,
        validator=lambda paper: None,
    )

    output = output_dir / "试卷（化工）.json"
    paper = json.loads(output.read_text(encoding="utf-8"))
    assert paper["exam_info"]["title"] == "化工试卷"
    assert "化工试卷" in output.read_text(encoding="utf-8")
    assert report["files"][0]["status"] == "valid"


def test_convert_directory_reports_needs_review_without_writing_paper(tmp_path):
    source_dir = tmp_path / "source"
    output_dir = tmp_path / "json"
    source_dir.mkdir()
    (source_dir / "试卷（软件）.docx").touch()
    extracted = "\n".join(
        [
            "[/body/p[1]] 软件试卷",
            "[/body/p[2]] 一、单选题（每题2分）",
            "[/body/p[3]] 1、HTTP 默认端口是（ ） A.80 B.443",
        ]
    )

    report = convert_directory(
        source_dir,
        output_dir,
        extractor=lambda source, temp_dir: extracted,
        validator=lambda paper: None,
    )

    assert report["files"][0]["status"] == "needs_review"
    assert not (output_dir / "试卷（软件）.json").exists()


def test_convert_directory_records_validator_failure(tmp_path):
    source_dir = tmp_path / "source"
    output_dir = tmp_path / "json"
    source_dir.mkdir()
    (source_dir / "试卷（化工）.docx").touch()
    extracted = "\n".join(
        [
            "[/body/p[1]] 化工试卷",
            "[/body/p[2]] 一、判断题（每题1分）",
            "[/body/p[3]] 1、理想气体混合物是一种理想溶液。（√）",
        ]
    )

    def invalid(_paper):
        raise ValueError("invalid paper")

    report = convert_directory(
        source_dir,
        output_dir,
        extractor=lambda source, temp_dir: extracted,
        validator=invalid,
    )

    assert report["files"][0]["status"] == "invalid"
    assert report["files"][0]["issues"] == ["invalid paper"]


def test_main_passes_source_and_output_to_converter(tmp_path, monkeypatch):
    source_dir = tmp_path / "source"
    output_dir = tmp_path / "json"
    source_dir.mkdir()
    received = {}

    def fake_convert(source, output):
        received["source"] = source
        received["output"] = output
        return {"file_count": 0, "files": []}

    monkeypatch.setattr(converter, "convert_directory", fake_convert)

    exit_code = converter.main([str(source_dir), "--output", str(output_dir)])

    assert exit_code == 0
    assert received == {"source": source_dir, "output": output_dir}
