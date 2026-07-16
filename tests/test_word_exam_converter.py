from scripts.word_exam_converter import (
    normalize_extracted_lines,
    parse_answer_token,
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
