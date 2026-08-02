"""列举题评分器单元测试 —— 用 Task 0 54 份回归中的真实作答。"""
import pathlib, sys
ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from scoring_worker.exact_scorers import score_enumeration

MECHANICAL_Q44 = {
    "id": "q44-1", "type": "short_answer", "score": 15.0,
    "scoring_mode": "enumeration", "question": "换向阀常用的几种控制方式?",
    "answer": "人力控制、机械控制、电气控制、直接压力控制、先导控制。",
    "scoring_points": [
        {"id": "p1", "text": "人力控制", "score": 3, "synonyms": ["手动", "手动控制"]},
        {"id": "p2", "text": "机械控制", "score": 3, "synonyms": ["机动", "行程控制"]},
        {"id": "p3", "text": "电气控制", "score": 3, "synonyms": ["电磁", "电磁控制"]},
        {"id": "p4", "text": "直接压力控制", "score": 3, "synonyms": ["液动", "液压控制"]},
        {"id": "p5", "text": "先导控制", "score": 3, "synonyms": ["电液动", "电液控制"]},
    ],
}


def test_synonym_hits_score_full():
    # SIM-M 真实作答：v0.1.11 下被实体门槛压到 2.2/15，人工估分 15
    r = score_enumeration(MECHANICAL_Q44, "常用的有手动、机动、电磁、液动和电液动几种控制方式。")
    assert r.score == 15.0 and r.review_level == "auto_pass"


def test_boilerplate_zero_and_manual():
    # SIM-L 真实作答：套话必须零分且转人工
    r = score_enumeration(MECHANICAL_Q44, "换向阀的控制要按操作规程来，注意安全就行。")
    assert r.score == 0.0 and r.review_level == "manual_required"


def test_long_answer_with_paraphrase_synonym():
    # metallurgy q41 SIM-H：v0.1.11 被否定词误判（"非金属"）压到 6.6/10
    q = {
        "id": "q41", "type": "short_answer", "score": 10.0,
        "scoring_mode": "enumeration", "question": "氩气吹入钢包搅拌钢水的作用?",
        "answer": "1、均匀钢水成分; 2、均匀钢水温度; 3、促使夹杂物碰撞上浮;",
        "scoring_points": [
            {"id": "p1", "text": "均匀钢水成分", "score": 3.3333, "synonyms": ["成分均匀"]},
            {"id": "p2", "text": "均匀钢水温度", "score": 3.3333, "synonyms": ["温度均匀"]},
            {"id": "p3", "text": "促使夹杂物碰撞上浮", "score": 3.3334,
             "synonyms": ["夹杂物上浮", "夹杂物碰撞长大", "去除夹杂物"]},
        ],
    }
    ans = ("钢包底吹氩通过透气砖使氩气泡弥散上浮,其作用主要有:(1)均匀钢水成分和温度,"
           "消除浓度与温度梯度;(2)促进非金属夹杂物碰撞长大并上浮进入渣层,提高钢水洁净度。")
    r = score_enumeration(q, ans)
    assert abs(r.score - 10.0) < 0.01 and r.review_level == "auto_pass"