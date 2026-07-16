from scripts.word_exam_converter import (
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
