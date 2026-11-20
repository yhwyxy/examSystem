"""P0 contracts: routing, rubric fidelity, evidence, degradation and review state."""
from types import SimpleNamespace

import pytest

from subjective_scoring import SubjectiveScoringService
from scoring_worker.grader_bridge import (
    aggregate_review_status, build_scoring_request, grade_subjective, parse_scoring_rubric,
)


@pytest.mark.parametrize("mode", ["text", "sql", "code", "calculation"])
def test_explicit_modes_never_change(mode):
    req = build_scoring_request({"id":"q", "type":"essay", "score":10,
                                 "scoring_mode":mode}, "answer")
    assert req.scoring_mode.value == mode


@pytest.mark.parametrize("metadata", [{"code_language":"sql"}, {"type":"sql"}, {"course_type":"database"}])
def test_missing_mode_uses_library_sql_route(metadata):
    req = build_scoring_request({"id":"q", "score":10, **metadata}, "SELECT 1")
    assert req.scoring_mode.value == "sql"


def test_point_attributes_and_request_config_are_preserved():
    point = {"id":"stable-conclusion", "text":"地球绕太阳公转", "score":10,
             "required":True,"critical":True,"conflict_policy":"zero_total",
             "conflict_score_cap_ratio":0.2,
             "accepted_expressions":["地球绕太阳运行"],
             "key_conditions":["以太阳为中心"],
             "source_evidence":"地球绕太阳公转"}
    req = build_scoring_request({"id":"q","score":10,"scoring_points":[point],
                                "scoring_config":{"score_precision":3}}, "地球绕太阳公转")
    assert req.scoring_points[0].model_dump(mode="json") == point
    assert req.scoring_config.score_precision == 3


@pytest.mark.parametrize("rubric", [None,"","无法解析的评分标准"])
def test_missing_rubric_never_uses_full_score_number_as_description(rubric):
    assert parse_scoring_rubric(rubric,10) is None
    req = build_scoring_request({"id":"q","score":10,"scoring_rubric":rubric},"answer")
    assert req.scoring_points == []


def test_duplicate_point_ids_fail_instead_of_double_counting():
    with pytest.raises(ValueError,match="duplicate"):
        build_scoring_request({"id":"q","score":10,"scoring_points":[
            {"id":"same","text":"one","score":5}, {"id":"same","text":"two","score":5}]}, "answer")


def test_selected_language_uses_matching_reference_and_points():
    captured = []
    service = SubjectiveScoringService(allow_model_load=False, code_pair_scorer=lambda q,d:1.0)
    original_score = service.score_with_trace
    def capture(req):
        captured.append(req)
        return original_score(req)
    service.score_with_trace = capture
    q = {"id":"code","type":"short_answer","score":5,"scoring_mode":"code",
         "code_language":"python","allowed_languages":["python","javascript"],
         "answer":"return a+b", "answers_by_language":{"python":"def add(a,b): return a+b","javascript":"function add(a,b) { return a+b; }"},
         "scoring_points_by_language":{"python":[{"id":"py","text":"return a+b","score":5}],
                                       "javascript":[{"id":"js","text":"return a+b","score":5}]}}
    _,_,_,detail = grade_subjective(q,{"language":"javascript","answer":"function add(a,b) { return a+b; }"},service=service)
    assert captured[0].code_language == "javascript"
    assert captured[0].scoring_points[0].id == "js"
    assert captured[0].reference_answer == q["answers_by_language"]["javascript"]
    assert detail["selected_language"] == "javascript"
    assert detail["student_answer"] == "function add(a,b) { return a+b; }"
    assert len(detail["scoring_config_version"]) == 64


def test_fallback_keeps_actual_backend_and_forces_review():
    from scoring_worker.grader_bridge import detail_from_scoring_result
    result = SimpleNamespace(score=5,confidence=1,need_manual_review=False,
        review_level="auto_pass",track="TextRerankerScorer",matched_points=[],missed_points=[],warnings=[],
        trace=SimpleNamespace(intermediate=SimpleNamespace(scorer="TextRerankerScorer",
            metadata={"model":"lexical_fallback","judge_backend":"llm_fallback","llm_error":"LLMJudgeRequestError"})))
    detail = detail_from_scoring_result({"id":"q","score":5},"answer",result)
    assert detail["scoring_backend"] == "TextRerankerScorer"
    assert detail["scoring_model"] == "lexical_fallback"
    assert detail["fallback_reason"] == "llm_failure:LLMJudgeRequestError"
    assert detail["need_manual_review"] and detail["review_status"] == "need_review"


def test_manual_review_resolves_old_flags_but_not_sibling_requirements():
    reviewed = {"manually_reviewed":True,"need_manual_review":True,"detail":{"low_confidence":True}}
    assert aggregate_review_status([reviewed]) == "reviewed"
    assert aggregate_review_status([reviewed,{"need_manual_review":True}]) == "need_review"
    assert aggregate_review_status([{"sub_results":[reviewed,{"need_manual_review":True}],"manually_reviewed":True}]) == "need_review"
    assert aggregate_review_status([reviewed,{"review_status":"high_confidence"}]) == "partial_manual"


def test_point_description_relation_evidence_survive_bridge():
    service = SubjectiveScoringService(allow_model_load=False,text_pair_scorer=lambda q,d:0.99)
    q = {"id":"q","type":"short_answer","score":5,"scoring_mode":"text",
         "answer":"地球围绕太阳运行。","scoring_points":[{"id":"orbit","text":"地球绕太阳公转","score":5}]}
    _,_,_,detail = grade_subjective(q,"地球绕太阳公转。",service=service)
    point = detail["matched_points"][0]
    assert point["point_id"] == "orbit"
    assert point["description"] == "地球绕太阳公转"
    assert point["relation"] == "supported"
    assert point["evidence"] in detail["student_answer"]


def test_composite_objective_subquestion_uses_objective_rules_and_partial_setting():
    from scoring_worker.grading import grade_submission
    paper={"questions":[{"id":"parent","type":"composite","score":4,"subquestions":[
        {"id":"choice","type":"multiple_choice","score":4,"answer":["A","B"]}]}]}
    partial = grade_submission(paper,{"parent":{"choice":{"answer":["A"]}}},None,multiple_choice_partial=True)
    full_only = grade_submission(paper,{"parent":{"choice":{"answer":["A"]}}},None,multiple_choice_partial=False)
    assert partial["subjective_score_final"] == 2
    assert full_only["subjective_score_final"] == 0


def test_regrade_preserves_manual_objective_final_and_machine_score():
    from scoring_worker.grading import grade_submission
    paper={"questions":[{"id":"q","type":"single_choice","score":4,"answer":"A"}]}
    result=grade_submission(paper,{"q":"B"},None,preserve={"q":{"manually_reviewed":True,"final_score":2,"review_status":"reviewed"}})
    assert result["objective_score"] == 2
    assert result["detail"][0]["machine_score"] == 0
    assert result["detail"][0]["final_score"] == 2


@pytest.mark.parametrize("policy,expected",[("point_zero",5),("cap_total",2),("zero_total",0)])
def test_bridge_enforces_critical_conflicts_even_with_old_library_results(policy,expected):
    from scoring_worker.grader_bridge import detail_from_scoring_result
    point = SimpleNamespace(point_id="critical",score=5,max_score=5,relation="contradicted",relation_confidence=0.95,evidence="相反的结论")
    result = SimpleNamespace(score=10,confidence=1,need_manual_review=False,review_level="auto_pass",track="LLMJudgeScorer",matched_points=[],missed_points=[point],warnings=[])
    q={"id":"q","score":10,"scoring_points":[{"id":"critical","text":"正确结论","score":5,"critical":True,"conflict_policy":policy,"conflict_score_cap_ratio":0.2}]}
    detail = detail_from_scoring_result(q,"相反的结论",result)
    assert detail["machine_score"] == expected
    assert detail["missed_points"][0]["score"] == 0
    assert detail["decision"] == "manual_review"
    assert detail["need_manual_review"] is True


def test_exact_reference_shortcut_still_has_original_answer_evidence():
    service=SubjectiveScoringService(allow_model_load=False)
    _,_,_,detail=grade_subjective({"id":"q","type":"short_answer","scoring_mode":"text","score":5,"answer":"标准答案。"},"标准答案。",service=service)
    assert detail["machine_score"] == 5
    assert detail["matched_points"][0]["evidence"] == "标准答案。"
    assert detail["matched_points"][0]["evidence_located"] is True
