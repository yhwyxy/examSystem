import json
from types import SimpleNamespace

from scripts import word_exam_converter as converter
from scripts.word_exam_converter import (
    apply_source_corrections,
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


def test_normalize_extracted_lines_splits_question_joined_after_options():
    text = (
        "[/body/p[1]] A.按长度确定 C.按标准确定 "
        "8、链条磨损严重时应（C）。"
    )

    assert normalize_extracted_lines(text) == [
        "A.按长度确定 C.按标准确定",
        "8、链条磨损严重时应(C)。",
    ]


def test_normalize_extracted_lines_keeps_numbered_answer_list_together():
    text = "[/body/p[1]] 1、脱气; 2、均匀钢水温度; 3、促使夹杂物上浮;"

    assert normalize_extracted_lines(text) == [
        "1、脱气; 2、均匀钢水温度; 3、促使夹杂物上浮;"
    ]


def test_parse_section_header_reads_type_and_score():
    section = parse_section_header("二、单项选择题（每题1.5分，共30分）")

    assert section is not None
    assert section.question_type == "single_choice"
    assert section.default_score == 1.5


def test_parse_section_header_assigns_calculation_mode():
    section = parse_section_header("四、计算题（每题10分，共20分）")

    assert section is not None
    assert section.question_type == "essay"
    assert section.scoring_mode == "calculation"


def test_parse_section_header_assigns_code_mode_for_program_section():
    section = parse_section_header("三、程序题（每题10分，共20分）")

    assert section is not None
    assert section.question_type == "essay"
    assert section.scoring_mode == "code"


def test_parse_answer_token_normalizes_objective_answers():
    assert parse_answer_token("√", "true_false") is True
    assert parse_answer_token("×", "true_false") is False
    assert parse_answer_token("ⅹ", "true_false") is False
    assert parse_answer_token(" A、C ", "multiple_choice") == ["A", "C"]
    assert parse_answer_token("GB4458.4-84", "multiple_choice") is None


def test_split_options_handles_inline_options():
    stem, options = split_options("温度如何变化 A.升高 B.降低 C.不变 D.改变")

    assert stem == "温度如何变化"
    assert options == [
        {"key": "A", "text": "升高"},
        {"key": "B", "text": "降低"},
        {"key": "C", "text": "不变"},
        {"key": "D", "text": "改变"},
    ]


def test_split_options_handles_unpunctuated_and_fullwidth_markers():
    stem, options = split_options("A 熔化； B 氧化；Ｃ、还原；Ｄ、出钢")

    assert stem == ""
    assert [option["key"] for option in options] == ["A", "B", "C", "D"]
    assert [option["text"] for option in options] == ["熔化;", "氧化;", "还原;", "出钢"]


def test_split_options_does_not_treat_parenthesized_answer_as_option():
    stem, options = split_options("链条磨损严重时应( C )。")

    assert stem == "链条磨损严重时应( C )。"
    assert options == []


def test_split_options_handles_marker_touching_chinese_text():
    stem, options = split_options("A有害 B有益 C无用")

    assert stem == ""
    assert [option["key"] for option in options] == ["A", "B", "C"]
    assert [option["text"] for option in options] == ["有害", "有益", "无用"]


def test_split_options_does_not_treat_units_or_company_names_as_markers():
    stem, options = split_options("温度为1400°C，A公司负责检测。")

    assert stem == "温度为1400°C,A公司负责检测。"
    assert options == []


def test_split_options_uses_broad_boundaries_after_option_line_starts():
    stem, options = split_options("A包裹连生B穿插连生 C毗邻连生")

    assert stem == ""
    assert [option["key"] for option in options] == ["A", "B", "C"]


def test_split_options_does_not_repeat_single_letter_option_values():
    stem, options = split_options("A、A B、V C、W D、C")

    assert stem == ""
    assert [option["key"] for option in options] == ["A", "B", "C", "D"]
    assert [option["text"] for option in options] == ["A", "V", "W", "C"]


def test_apply_source_corrections_repairs_known_energy_option_typo():
    lines = ["A、焦耳;B、兆帕;B、瓦特;B、牛顿。"]

    assert apply_source_corrections("试卷（能源与动力工程）.docx", lines) == [
        "A、焦耳;B、兆帕;C、瓦特;D、牛顿。"
    ]


def test_apply_source_corrections_adds_missing_communication_question_score():
    lines = ["4、关于IP地址划分,根据表格中的IP地址规律,请完善如下表格。"]

    corrected = apply_source_corrections("试卷（通信工程）.doc", lines)

    assert corrected[0].endswith("(10分)")


def test_apply_source_corrections_rebuilds_software_choice_answer_key():
    corrected = apply_source_corrections(
        "试卷（软件开发）.docx",
        [
            "以下哪些加密算法是不可逆的:( AC ) 1、2、ABC",
            "3、ABCD",
            "1.",
            "8.哪条SQL语句用于插入新记录( )",
            "1.下面有两张表:",
            "2.找出元素item在给定数组array中的位置",
        ],
    )

    assert corrected == [
        "1.以下哪些加密算法是不可逆的:( AC )",
        "8.哪条SQL语句用于插入新记录(C)",
        "1.下面有两张表:(15分)",
        "2.找出元素item在给定数组array中的位置(15分)",
    ]


def test_parse_exam_lines_applies_provided_software_answers():
    result = parse_exam_lines(
        "试卷（软件开发）.docx",
        [
            "软件开发试卷",
            "二、简答题（每题10分）",
            *[f"{number}.占位题" for number in range(1, 17)],
        ],
    )

    assert "JavaScript" in result.paper["questions"][15]["answer"]


def test_parse_section_header_reads_formula_score():
    section = parse_section_header("一、单选题（20*1.5=30分）")

    assert section is not None
    assert section.default_score == 1.5


def test_parse_section_header_reads_each_small_question_score():
    section = parse_section_header("单项选择题（共10小题，每小题2 分，共20分）")

    assert section is not None
    assert section.default_score == 2


def test_parse_section_header_derives_score_from_total_and_count():
    section = parse_section_header("四、简答题（共4题）（共计40分）")

    assert section is not None
    assert section.default_score == 10


def test_parse_section_header_recognizes_indefinite_choice():
    section = parse_section_header("三、不定项选择（每题2分，共18分）")

    assert section is not None
    assert section.question_type == "multiple_choice"


def test_parse_section_header_recognizes_question_and_answer_section():
    section = parse_section_header("四问答题（每题10分，共40分）")

    assert section is not None
    assert section.question_type == "short_answer"
    assert section.default_score == 10


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


def test_parse_exam_lines_accepts_section_header_as_first_document_line():
    result = parse_exam_lines(
        "试卷（法务）.docx",
        [
            "一、单选（每题2分，共30分）",
            "1、下列说法正确的是？",
            "A.甲 B.乙 C.丙 D.丁",
            "标准答案：A",
        ],
    )

    assert len(result.paper["questions"]) == 1
    assert result.paper["questions"][0]["answer"] == "A"


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


def test_parse_exam_lines_keeps_numbered_items_inside_subjective_answer():
    result = parse_exam_lines(
        "试卷（焊接）.docx",
        [
            "焊接试卷",
            "四、简答题（共20分）",
            "1、焊条的选用原则有哪些？（10分）",
            "答：1.根据焊件材料选用焊条",
            "2.根据焊缝使用环境选用焊条",
            "3.根据抗裂性要求选用焊条",
            "2、焊接缺陷有哪些？（10分）",
            "答：裂纹、夹渣和气孔。",
        ],
    )

    assert len(result.paper["questions"]) == 2
    assert "2.根据焊缝使用环境选用焊条" in result.paper["questions"][0]["answer"]


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


def test_parse_exam_lines_does_not_treat_program_input_as_answer():
    result = parse_exam_lines(
        "试卷（软件开发）.docx",
        [
            "软件开发试卷",
            "三、程序题（共30分）",
            "1.找出元素item在数组中的位置（15分）",
            "输入：[1, 2, 3], 3",
            "输出：2",
        ],
    )

    question = result.paper["questions"][0]
    assert "输入" in question["question"]
    assert "answer" not in question
    assert any(issue.code == "missing_answer" for issue in result.issues)


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


def test_parse_exam_lines_reads_answer_embedded_after_multiple_spaces():
    result = parse_exam_lines(
        "试卷（仪器仪表）.docx",
        [
            "测控技术与仪器",
            "二、单选题（每题1.5分，共30分）",
            "1、将差压信号转换成电信号的设备是    C   。",
            "A、平衡容器      B、脉冲管路",
            "C、差压变送器    D、显示器",
        ],
    )

    question = result.paper["questions"][0]
    assert question["answer"] == "C"
    assert len(question["options"]) == 4
    assert result.issues == []


def test_parse_exam_lines_reads_answer_embedded_in_middle_of_stem():
    result = parse_exam_lines(
        "试卷（仪器仪表）.docx",
        [
            "测控技术与仪器",
            "二、单选题（每题1.5分）",
            "1、热电偶的热电特性是由    D     所决定的。",
            "A、长度 B、粗细 C、温度 D、材料",
        ],
    )

    assert result.paper["questions"][0]["answer"] == "D"
    assert result.issues == []


def test_parse_exam_lines_infers_boolean_question_in_fill_blank_section():
    result = parse_exam_lines(
        "试卷（仪器仪表）.docx",
        [
            "测控技术与仪器",
            "一、填空题（每题1分，共10分）",
            "1、仪表的给定值就是它的测量值。（ × ）",
        ],
    )

    question = result.paper["questions"][0]
    assert question["type"] == "true_false"
    assert question["answer"] is False


def test_parse_exam_lines_accepts_unlabelled_subjective_answer_after_question():
    result = parse_exam_lines(
        "试卷（安全管理）.docx",
        [
            "安全管理试卷",
            "四、简答题（共10分）",
            "1、什么是三违（10分）？",
            "领导和管理人员的违章指挥、操作人员的违规操作和违反劳动纪律。",
        ],
    )

    assert result.paper["questions"][0]["answer"].startswith("领导和管理人员")
    assert result.issues == []


def test_parse_exam_lines_accepts_unlabelled_answer_when_title_says_answer():
    result = parse_exam_lines(
        "试卷（矿物加工）.docx",
        [
            "矿物加工工程专业考试试卷（A）答案",
            "四、简答题（每题10分）",
            "1、矿物粉碎的目的。",
            "使物料粒度减小，并使有用矿物与脉石充分解离。",
        ],
    )

    assert result.paper["questions"][0]["answer"].startswith("使物料粒度减小")
    assert result.issues == []


def test_parse_exam_lines_reads_true_false_answer_on_following_line():
    result = parse_exam_lines(
        "试卷（通信工程）.docx",
        [
            "通信工程试卷答案",
            "一、判断题（每题1分）",
            "1、信息会被转换成二进制数进行处理。",
            "（ √ ）",
        ],
    )

    assert result.paper["questions"][0]["answer"] is True
    assert result.issues == []


def test_parse_exam_lines_reads_separate_standard_answer_paragraph():
    result = parse_exam_lines(
        "试卷（法务）.docx",
        [
            "法务试卷",
            "一、单选题（每题2分）",
            "1、下列说法正确的是：",
            "A.甲说法 B.乙说法 C.丙说法 D.丁说法",
            "标准答案：C",
        ],
    )

    assert result.paper["questions"][0]["answer"] == "C"
    assert result.issues == []


def test_parse_exam_lines_applies_compact_answer_key_to_recent_questions():
    result = parse_exam_lines(
        "试卷（法务）.docx",
        [
            "三、不定项选择（每题2分）",
            "1、第一题：",
            "A.甲 B.乙 C.丙 D.丁",
            "2、第二题：",
            "A.甲 B.乙 C.丙 D.丁",
            "标准答案：1、ABD 2、AC",
        ],
    )

    assert [q["answer"] for q in result.paper["questions"]] == [
        ["A", "B", "D"],
        ["A", "C"],
    ]
    assert result.issues == []


def test_parse_exam_lines_applies_numbered_reference_answers():
    result = parse_exam_lines(
        "试卷（法务）.docx",
        [
            "四、案例（共9分）",
            "1.第一问？（4分）",
            "2.第二问？（5分）",
            "参考答案：",
            "1.第一问参考答案。",
            "2.第二问参考答案。",
        ],
    )

    assert [q["answer"] for q in result.paper["questions"]] == [
        "第一问参考答案。",
        "第二问参考答案。",
    ]
    assert result.issues == []


def test_parse_exam_lines_keeps_dated_case_background_out_of_question_count():
    result = parse_exam_lines(
        "试卷（法务）.docx",
        [
            "四、案例（共4分）",
            "2月5日，甲与乙订立一份房屋买卖合同。",
            "问题：",
            "1.房屋的物权归属应当如何确定？（4分）",
            "参考答案：",
            "1.房屋归乙所有。",
        ],
    )

    assert len(result.paper["questions"]) == 1
    assert result.paper["questions"][0]["question"].startswith(
        "2月5日，甲与乙订立一份房屋买卖合同。"
    )
    assert result.issues == []


def test_parse_exam_lines_reads_true_false_symbol_at_sentence_end():
    result = parse_exam_lines(
        "试卷（焊接）.docx",
        [
            "焊接试卷",
            "一、判断题（每题1分）",
            "1、乙炔导管必须从回火防止器出口接出。 √",
        ],
    )

    assert result.paper["questions"][0]["answer"] is True
    assert result.issues == []


def test_parse_exam_lines_does_not_treat_ip_addresses_as_question_numbers():
    result = parse_exam_lines(
        "试卷（通信工程）.docx",
        [
            "通信工程试卷答案",
            "四、简答题（每题10分）",
            "1、完善IP地址表格。",
            "10.10.0.0",
            "10.10.0.1",
            "255.255.255.0",
        ],
    )

    assert len(result.paper["questions"]) == 1
    assert "10.10.0.0" in result.paper["questions"][0]["answer"]


def test_parse_exam_lines_accepts_question_number_without_punctuation():
    result = parse_exam_lines(
        "试卷（机械）.docx",
        [
            "机械试卷",
            "三、多选题（每题2分）",
            "10机械制图常见的剖视有（ACD）。",
            "A.全剖视图 B.主视图 C.半剖视图 D.局部剖视图",
        ],
    )

    assert len(result.paper["questions"]) == 1
    assert result.paper["questions"][0]["answer"] == ["A", "C", "D"]


def test_parse_exam_lines_accepts_question_number_before_opening_quote():
    result = parse_exam_lines(
        "试卷（物流）.docx",
        [
            "物流试卷",
            "一、单选题（每题1分）",
            "9 “物流冰山说”指（C）。",
            "A.甲 B.乙 C.丙 D.丁",
        ],
    )

    assert len(result.paper["questions"]) == 1
    assert result.paper["questions"][0]["answer"] == "C"


def test_parse_exam_lines_recognizes_chinese_to_english_section():
    result = parse_exam_lines(
        "试卷（物流）.docx",
        [
            "物流试卷",
            "四、中译英题目（每题10分）",
            "1、请关闭舱门。",
            "答：PLEASE CLOSE THE HATCH COVER.",
        ],
    )

    assert result.paper["questions"][0]["type"] == "short_answer"
    assert result.paper["questions"][0]["answer"].startswith("PLEASE CLOSE")


def test_parse_exam_lines_reports_duplicate_source_option_keys():
    result = parse_exam_lines(
        "试卷（能源）.docx",
        [
            "能源试卷",
            "二、单选题（每题1.5分）",
            "1、热量的单位是（A）。",
            "A、焦耳；B、兆帕；B、瓦特；B、牛顿。",
        ],
    )

    assert any(issue.code == "duplicate_option_key" for issue in result.issues)


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
