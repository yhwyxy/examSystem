# 精确评分模式（exact scoring modes）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为固定题库的 5 类主观题（列举/翻译/表格/分录/案例分析）添加 examSystem 本地确定性评分器，配套题库标注、lint 与快照同步，消除 22 项大误差中的绝大多数。

**Architecture:** 在 `scoring_worker/grader_bridge.py` 的 `grade_subjective()` 入口分派：题目 `scoring_mode` ∈ {enumeration, translation, table, ledger, case_analysis} 时走本地精确评分器；`calculation` 透传给 subjective-scoring 已有的 CalculationScorer；其余走 text reranker。composite 子题经 `_grade_composite()` 递归调用 `grade_subjective()`，自动获得同样分派。题库标注写在 `data/papers/*.json`，重评读的是 run 快照，故所有题库变更必须经 `scripts/sync_run_snapshots.py` 同步并更新 `exam_runs.snapshot_hash`。

**Tech Stack:** Python 3.12（scoring_worker 现有栈）、subjective-scoring v0.1.11（库侧零改动）、PostgreSQL dev 库

**Git:** 基于 `main` 新建 `feature/exact-scoring-modes`；每个 Task 独立提交；Task 6 回归通过后 `merge --no-ff` 回 `main`。

## Global Constraints

- subjective-scoring 仓库不做任何修改；等价词/计算配置走已有请求级 API：`ScoringOptions.text_bounded_corrections.extra_equivalences`、`ScoringOptions.calculation`、`ScoringMode.CALCULATION`
- 试卷 JSON 向后兼容：无 `scoring_mode` 或值不认识 → 走 text reranker，行为与现状一致
- DSN 固定为 `postgres://exam_app:exam_app_dev@127.0.0.1:5432/exam_system?sslmode=disable`（下文记 `$DSN`）
- worker 重启必须：unset 全部代理变量 + `HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1`，否则模型加载静默回退词法评分（单 job 毫秒级完成即是回退征兆）
- 改 `data/papers/*.json` 后必须跑 `scripts/sync_run_snapshots.py`（重评读 `data/exam_runs/*.json` 快照，Go 侧按 canonical JSON 的 sha256 与 `exam_runs.snapshot_hash` 校验）
- 回归验收（54 份，submissions id 3-56，共 177 个主观题评分行）：
  - 逐题 |系统分−估分|>3 的行数：24 → ≤10
  - 逐题 MAE：1.59 → ≤1.20
  - 对 v0.1.11 冻结基线的新增退化（基线 ≤3 且新版 >3）：0 例，或逐例书面归因确认可接受
  - 整卷总分误差 ≤3 的卷数不下降；客观题 54 份保持零偏差

---

## 文件结构

```
examSystem/
├── data/papers/*.json                     ← 改: 逐题加 scoring_mode + 模式配置
├── scoring_worker/
│   ├── grader_bridge.py                   ← 改: 分派点 + _grade_by_exact_mode 等
│   ├── case_analysis_scorer.py            ← 新: 案例分析（短语结论点 + text 理由点，回调库）
│   └── exact_scorers/
│       ├── __init__.py                    ← 新
│       ├── _base.py                       ← 新: ExactScoreResult
│       ├── enumeration_scorer.py          ← 新: 列举题
│       ├── translation_scorer.py          ← 新: 翻译题（语言校验 + 短语命中）
│       ├── table_scorer.py                ← 新: 表格补全（单元格值 + 行标签上下文）
│       └── ledger_scorer.py               ← 新: 会计分录（金额+科目关键词 / 处理思路）
├── scripts/
│   ├── lint_papers.py                     ← 新: 题库静态检查（L1-L6 规则）
│   └── sync_run_snapshots.py              ← 新: papers → exam_runs 快照同步 + hash 更新
├── benchmarks/subjective-54/
│   └── baseline_v0111_perq.jsonl          ← 新: v0.1.11 逐题分数冻结（回归对照）
└── tests/
    ├── test_lint_papers.py                ← 新
    └── worker/
        ├── test_exact_dispatch.py         ← 新: 分派器兜底行为
        ├── test_enumeration_scorer.py     ← 新
        ├── test_bridge_scoring_options.py ← 新: calculation/等价词透传
        ├── test_translation_scorer.py     ← 新
        ├── test_table_scorer.py           ← 新
        ├── test_ledger_scorer.py          ← 新
        └── test_case_analysis_scorer.py   ← 新
```

---

## 接口约定

**分派点**：`grader_bridge.py` 的 `grade_subjective()`，在空答案检查（现第 462-464 行）之后、`ssvc = get_subjective_service()`（现第 466 行）之前插入：

```python
    # === 精确评分模式先行，不经过 subjective-scoring ===
    mode = str(question.get("scoring_mode") or "").strip().lower()
    exact = _grade_by_exact_mode(mode, question, student_answer, preserve)
    if exact is not None:
        return exact
```

**评分器统一签名与返回值**：

```python
# scoring_worker/exact_scorers/_base.py
@dataclass
class ExactScoreResult:
    score: float
    detail: dict          # matched_points / missed_points / reason / warnings
    review_level: str     # "auto_pass" | "suggested_review" | "manual_required"

# 每个评分器: score_xxx(question: dict, student_answer: str) -> ExactScoreResult
```

`_grade_by_exact_mode()` 把 `ExactScoreResult` 转成与 `detail_from_scoring_result()` 同结构的 entry（前端 detail.js 与 `aggregate_review_status()` 契约字段齐备），返回 `(final, machine, review_status, entry)`。

---

**题库标注字段**（examSystem 私有，库与 Go 后端忽略未知键）：

```jsonc
{
  "scoring_mode": "enumeration",          // enumeration|translation|table|ledger|case_analysis|calculation|text
  "scoring_points": [                      // enumeration/translation/case_analysis 复用现有字段
    {"id": "p1", "text": "人力控制", "score": 3, "synonyms": ["手动", "手动控制"]},
    {"id": "p2", "text": "归丙", "score": 2, "match": "phrase"}   // case_analysis: phrase=精确结论点
  ],
  "translation": {"target_lang": "en"},   // translation 模式专用
  "table": {"cells": [                     // table 模式专用
    {"label": "a", "expected": ["255.255.255.0"], "score": 2, "require_label_context": true}
  ]},
  "ledger": {                              // ledger 模式专用
    "entries": [{"keywords": ["银行存款"], "numbers": [10530], "score": 1.5, "tolerance": 0.005}],
    "treatment_points": [{"text": "不确认收入", "synonyms": ["融资"], "score": 1.5}]
  },
  "calculation": {                         // 透传给库 CalculationScoringConfig
    "strategy": "static_values",
    "steps": [{"id": "s1", "description": "月产气量", "expected": 25200, "score": 4, "tolerance": 1}],
    "final_answers": [{"id": "f1", "description": "吨矿产气", "expected": 75.22, "score": 6, "tolerance": 0.5}]
  },
  "extra_equivalences": [["Mn", "锰"], ["Cr", "铬"]]   // text 模式题的业务同义组，透传给库
}
```

---

## 任务分解

### Task 0: 新建分支 + 精确评分骨架与分派器

**Files:**
- Create: `scoring_worker/exact_scorers/{__init__,_base,enumeration_scorer,translation_scorer,table_scorer,ledger_scorer}.py`
- Create: `scoring_worker/case_analysis_scorer.py`
- Modify: `scoring_worker/grader_bridge.py`（分派点 + 3 个新函数）
- Test: `tests/worker/test_exact_dispatch.py`

**Interfaces:**
- Consumes: 无
- Produces: `_grade_by_exact_mode(mode, question, student_answer, preserve) -> tuple[float, float, str, dict] | None`；`ExactScoreResult`（后续 Task 全部依赖）

- [ ] **Step 1: 新建分支**

```bash
cd /Users/yhw/Code/Github/examSystem
git checkout main && git checkout -b feature/exact-scoring-modes
```

- [ ] **Step 2: 创建包与 base 类型**

```python
# scoring_worker/exact_scorers/__init__.py
from .enumeration_scorer import score_enumeration
from .translation_scorer import score_translation
from .ledger_scorer import score_ledger
from .table_scorer import score_table

__all__ = ["score_enumeration", "score_translation", "score_ledger", "score_table"]
```

```python
# scoring_worker/exact_scorers/_base.py
from __future__ import annotations
from dataclasses import dataclass
from typing import Any


@dataclass
class ExactScoreResult:
    score: float
    detail: dict[str, Any]
    review_level: str  # "auto_pass" | "suggested_review" | "manual_required"
```

4 个 `exact_scorers/*_scorer.py` 与 `case_analysis_scorer.py` 先写占位实现（Task 1/3/4 逐个替换）：

```python
# scoring_worker/exact_scorers/enumeration_scorer.py（其余 4 个文件同构，改函数名）
"""列举题精确评分器：评分点=条目集合，命中任一同义词即得该条目满分。"""
from ._base import ExactScoreResult


def score_enumeration(question: dict, student_answer: str) -> ExactScoreResult:
    return ExactScoreResult(0.0, {"reason": "not implemented"}, "manual_required")
```

注意 `case_analysis_scorer.py` 在 `scoring_worker/` 根下（它要回调 `grader_bridge`，放包外避免循环导入），占位函数名 `score_case_analysis`。

- [ ] **Step 3: grader_bridge 插入分派点与转换函数**

按「接口约定」把 4 行分派代码插入 `grade_subjective()`；然后在 `_empty_subjective_detail` 定义之前新增：

```python
_EXACT_SCORER_MAP: dict[str, str] = {
    "enumeration": "score_enumeration",
    "translation": "score_translation",
    "ledger": "score_ledger",
    "table": "score_table",
    "case_analysis": "score_case_analysis",
}


def _grade_by_exact_mode(
    mode: str, question: dict, student_answer: str,
    preserve: dict | None = None,
) -> tuple[float, float, str, dict] | None:
    """精确评分模式分派；返回 None 表示走 text reranker 兜底。"""
    fn_name = _EXACT_SCORER_MAP.get(mode)
    if not fn_name:
        return None
    if fn_name == "score_case_analysis":
        from .case_analysis_scorer import score_case_analysis as fn
    else:
        from . import exact_scorers
        fn = getattr(exact_scorers, fn_name)
    result = fn(question, student_answer)
    entry = _exact_result_to_detail(question, student_answer, result, preserve)
    return (round(float(entry["final_score"]), 6),
            round(float(entry["machine_score"]), 6),
            entry["review_status"], entry)


def _exact_result_to_detail(
    question: dict, student_answer: str, result, preserve: dict | None = None,
) -> dict:
    """ExactScoreResult -> 与 detail_from_scoring_result 同结构的 entry。"""
    max_score = float(question.get("score", 0) or 0)
    machine = max(0.0, min(max_score, float(result.score)))
    manual = bool((preserve or {}).get("manually_reviewed"))
    final = float((preserve or {}).get("final_score", machine)) if manual else machine
    qid = question.get("id")
    review_status = _exact_review_status(result.review_level)
    need_manual = result.review_level == "manual_required"
    confidence = 1.0 if result.review_level == "auto_pass" else 0.5
    inner = {
        "track": f"ExactScorer({question.get('scoring_mode')})",
        "reason": result.detail.get("reason"),
        "warnings": result.detail.get("warnings", []),
        "max_score": max_score, "confidence": confidence,
        "final_score": machine, "machine_score": machine,
        "question_id": qid, "review_level": result.review_level,
        "scoring_mode": question.get("scoring_mode"),
        "matched_points": result.detail.get("matched_points", []),
        "missed_points": result.detail.get("missed_points", []),
        "student_answer": student_answer,
        "reference_answer": question.get("answer", ""),
        "manually_reviewed": manual, "need_manual_review": need_manual,
    }
    return {
        "id": qid, "question_id": qid, "type": question.get("type"),
        "question": question.get("question", ""),
        "reference_answer": question.get("answer", ""),
        "student_answer": student_answer,
        "max_score": max_score,
        "machine_score": machine, "score": machine, "final_score": final,
        "confidence": confidence,
        "grading_method": f"exact:{question.get('scoring_mode')}",
        "reason": result.detail.get("reason"),
        "review_status": review_status,
        "manually_reviewed": manual,
        "low_confidence": need_manual,
        "need_manual_review": need_manual,
        "lowest_confidence_flagged": need_manual,
        "detail": inner,
    }


def _exact_review_status(level: str) -> str:
    """与 _legacy_review_status 同语义：级别 -> 单题 review_status。"""
    if level == "auto_pass":
        return "high_confidence"
    if level == "suggested_review":
        return "reviewed"
    if level == "manual_required":
        return "need_review"
    return "low_confidence"
```

- [ ] **Step 4: 写分派器兜底测试**

```python
# tests/worker/test_exact_dispatch.py
"""分派器兜底行为——不加载 reranker 模型即可跑。"""
import pathlib, sys
ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from scoring_worker.grader_bridge import _grade_by_exact_mode


def test_no_mode_falls_through_to_text():
    q = {"id": "x", "type": "short_answer", "score": 10, "question": "q", "answer": "a"}
    assert _grade_by_exact_mode("", q, "ans") is None
    assert _grade_by_exact_mode("text", q, "ans") is None
    assert _grade_by_exact_mode("nonsense", q, "ans") is None


def test_placeholder_scorer_forces_manual_review():
    q = {"id": "x", "type": "short_answer", "score": 10,
         "scoring_mode": "enumeration", "question": "q", "answer": "a"}
    final, machine, status, entry = _grade_by_exact_mode("enumeration", q, "ans")
    assert (final, machine, status) == (0.0, 0.0, "need_review")
    assert entry["need_manual_review"] is True
```

- [ ] **Step 5: 运行测试**

```bash
cd /Users/yhw/Code/Github/examSystem && python -m pytest tests/worker/test_exact_dispatch.py -v
```
Expected: 2 passed

- [ ] **Step 6: 提交**

```bash
git add scoring_worker/ tests/worker/test_exact_dispatch.py
git commit -m "feat(scoring): exact scoring skeleton + dispatcher in grader_bridge (Task 0)"
```

---

### Task 1: enumeration 评分器实现 + 31 道列举题标注

**覆盖题目**（31 道，含子题；已有 scoring_points 逐条结构，只需加 `scoring_mode` 与必要 `synonyms`）：
mechanical q42/q44、welding q41/q42/q43/q44、safety-management q41/q42/q43、metallurgy q41/q42、energy-power q41/q43、mineral-processing q41/q44、metal-materials q41/q44、communications q41/q42/q43、environmental q41/q42/q43/q44、software-development q17/q19、chemical-engineering q41/q43、electrical q41/q42、materials q41

**解决的误差项**：mechanical q44 (−12.8)、welding q41 (−4.2) / q44 (−5.5)、metallurgy q41 (−3.4)、safety-management q42 (−3.4)、metal-materials q41 (+4.0)、environmental q41 (+3.3) / q44 (+9.9)、materials q41、chemical-engineering q43 (−4.7，零命中转人工)

**Files:**
- Modify: `scoring_worker/exact_scorers/enumeration_scorer.py`
- Modify: `data/papers/*.json`（上述 31 题）
- Test: `tests/worker/test_enumeration_scorer.py`

**Interfaces:**
- Consumes: `ExactScoreResult`（Task 0）
- Produces: `score_enumeration(question, student_answer) -> ExactScoreResult`；内部工具 `_norm(text) -> str`、`_item_hit(point, student_norm) -> str | None`（Task 3 translation 复用）

**评分与复核策略**（写进 docstring）：
- 命中 = 条目 text 或任一 synonym 归一化后是学生答案子串；长度 >3 的变体额外允许 bigram 覆盖率 ≥0.75
- 命中条目得该条满分，一条最多计一次；总分 `min(Σ命中分, 题目满分)`；不倒扣
- 命中数 ≥1 → auto_pass（确定性结果）；零命中且作答非空 → manual_required（意译保护，交人工）；空答案不会进入（上游已拦）

- [ ] **Step 1: 写失败测试（用 54 份回归中的真实作答）**

```python
# tests/worker/test_enumeration_scorer.py
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
```

- [ ] **Step 2: 运行测试确认失败**

```bash
cd /Users/yhw/Code/Github/examSystem && python -m pytest tests/worker/test_enumeration_scorer.py -v
```
Expected: 2 failed, 1 passed（套话用例恰与占位实现行为一致，先行通过属正常）

- [ ] **Step 3: 实现 score_enumeration**

```python
# scoring_worker/exact_scorers/enumeration_scorer.py 全文替换
"""列举题精确评分器：评分点=条目集合，命中任一同义词即得该条目满分。"""
from __future__ import annotations

import re
import unicodedata

from ._base import ExactScoreResult

_STRIP_RE = re.compile(r"[\s,，。.;；:：、！!？?（）()\[\]【】\"'“”‘’\-—–/\\]+")


def _norm(text: str) -> str:
    return _STRIP_RE.sub("", unicodedata.normalize("NFKC", str(text or "")).casefold())


def _bigram_coverage(term: str, student: str) -> float:
    grams = [term[i:i + 2] for i in range(len(term) - 1)]
    if not grams:
        return 1.0 if term and term in student else 0.0
    return sum(1 for g in grams if g in student) / len(grams)


def _item_hit(point: dict, student_norm: str) -> str | None:
    """命中返回证据变体原文，未命中返回 None。"""
    variants = [str(point.get("text") or "")] + [str(s) for s in (point.get("synonyms") or [])]
    for v in variants:
        nv = _norm(v)
        if not nv:
            continue
        if len(nv) <= 3:
            if nv in student_norm:
                return v
        elif nv in student_norm or _bigram_coverage(nv, student_norm) >= 0.75:
            return v
    return None


def score_enumeration(question: dict, student_answer: str) -> ExactScoreResult:
    points = question.get("scoring_points") or []
    max_score = float(question.get("score", 0) or 0)
    student_norm = _norm(student_answer)
    matched, missed, total = [], [], 0.0
    for p in points:
        w = float(p.get("score", 0) or 0)
        ev = _item_hit(p, student_norm)
        if ev is not None:
            total += w
            matched.append({"point_id": p.get("id"), "score": w, "max_score": w,
                            "evidence": ev, "reason": "条目命中（精确/同义词）"})
        else:
            missed.append({"point_id": p.get("id"), "score": 0.0, "max_score": w,
                           "reason": f"未命中条目：{p.get('text')}"})
    total = round(min(total, max_score), 4)
    detail = {"matched_points": matched, "missed_points": missed}
    if not matched and student_norm:
        detail["reason"] = "枚举零命中：疑似意译或错答，转人工确认"
        return ExactScoreResult(0.0, detail, "manual_required")
    return ExactScoreResult(total, detail, "auto_pass")
```

- [ ] **Step 4: 运行测试确认通过**

```bash
cd /Users/yhw/Code/Github/examSystem && python -m pytest tests/worker/test_enumeration_scorer.py tests/worker/test_exact_dispatch.py -v
```
Expected: 5 passed（含 Task 0 的 2 个分派测试不回归）

- [ ] **Step 5: 标注 31 道题（data/papers/*.json）**

通用做法：题目（或 composite 子题）加 `"scoring_mode": "enumeration"`，给字面差异大的评分点补 `synonyms`。重点题的标注值（其余题只改 scoring_mode，synonyms 按参考答案与 54 份作答语料酌补）：

- `mechanical.json` q44 子题 q44-1：按 Task 1 Step 1 测试中 MECHANICAL_Q44 的五点 synonyms 原样写入
- `metallurgy.json` q41：按 Step 1 测试中的三点 synonyms 写入
- `safety-management.json` q42：p2 "违规操作" 加 `["违章作业", "违章操作"]`；p3 "违反劳动纪律" 加 `["劳动纪律"]`
- `welding.json` q44：数字型条目改写为可命中文本——p1 "E表示焊条"；p2 text "43表示熔敷金属抗拉强度" 加 `["抗拉强度430", "抗拉强度的最小值", "抗拉强度不低于430"]`；p3 "1表示全位置焊接" 加 `["全位置焊接"]`；p4 "5表示低氢钠型药皮直流反接" 加 `["低氢钠型", "15表示", "直流反接"]`
- `electrical.json` q41（点=品牌列表）：p1 text "西门子" 加 `["abb", "施耐德", "欧姆龙", "三菱", "罗克韦尔"]`；p2 text "浙江中控" 加 `["科远", "tmeic", "艾默生", "横河", "和利时"]`（任一品牌命中即该点 5 分）
- `environmental.json` q44（元评分点重写为具体条目）：删除原 p1/p2，改为 13 条具体工作各 5 分（超低排放/清江行动/钢渣清理/除尘改造/棚化改造/清污分流/噪声整理/209号文件/脱硫脱硝/危废库建设/煤气回收改造/皮带通廊封闭/花园式工厂），`min(Σ, 10)` 自动实现"任答两项满分"
- `materials.json` q41：p1 text "sqrt(2)/2" 加 `["1/sqrt(2)", "√2/2", "0.707"]`；p2 text "45°" 加 `["45度", "夹角是45", "β=45"]`
- `communications.json` q43：端口条目 p1 "23-telnet" 加 `["23:telnet", "23 telnet", "telnet"]`，其余端口同构

- [ ] **Step 6: 抽查验证 + 提交**

```bash
cd /Users/yhw/Code/Github/examSystem
python -m pytest tests/worker/ -v
python - <<'EOF'
import json, glob
n = 0
for f in glob.glob("data/papers/*.json"):
    doc = json.load(open(f))
    for q in doc["questions"]:
        subs = q.get("subquestions") or []
        n += sum(1 for x in [q, *subs] if x.get("scoring_mode") == "enumeration")
print("enumeration 标注题数:", n)   # 预期 31
EOF
git add data/papers/ scoring_worker/exact_scorers/enumeration_scorer.py tests/worker/test_enumeration_scorer.py
git commit -m "feat(scoring): enumeration scorer + 31 题列举模式标注 (Task 1)"
```

---

### Task 2: bridge 透传 calculation 与 extra_equivalences + 3 道计算题标注

**解决的误差项**：mineral-processing q42 ×2（−6.0，75.22 vs 75.2 舍入）、metallurgy q43（753/135 kg）、mechanical q43、materials q42-2（元素符号↔中文名，走 text+等价词）

**Files:**
- Modify: `scoring_worker/grader_bridge.py`（`build_scoring_request` 的 mode 映射 + `_build_scoring_options`）
- Modify: `data/papers/{mineral-processing,mechanical,metallurgy,materials}.json`
- Test: `tests/worker/test_bridge_scoring_options.py`

**Interfaces:**
- Consumes: subjective-scoring 的 `ScoringMode.CALCULATION`、`ScoringOptions`（`calculation` / `text_bounded_corrections.extra_equivalences` 字段）
- Produces: `_build_scoring_options(question) -> ScoringOptions | None`；`build_scoring_request` 在题目带 `calculation` 配置时返回 CALCULATION 模式请求

- [ ] **Step 1: 写失败测试**

```python
# tests/worker/test_bridge_scoring_options.py
import pathlib, sys
ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from scoring_worker.grader_bridge import build_scoring_request

CALC_Q = {
    "id": "q42", "type": "short_answer", "score": 10.0,
    "scoring_mode": "calculation", "question": "吨矿产气是多少kg/t?",
    "answer": "吨矿产气量=35*24*30*1000/335000=75.22kg/t",
    "scoring_points": [{"id": "p1", "text": "结果为 75.22kg/t", "score": 10}],
    "calculation": {
        "strategy": "static_values",
        "steps": [{"id": "s1", "description": "月产气量 25200 吨", "expected": 25200, "score": 4, "tolerance": 1}],
        "final_answers": [{"id": "f1", "description": "吨矿产气 kg/t", "expected": 75.22, "score": 6, "tolerance": 0.5}],
    },
}


def test_calculation_mode_and_config_passthrough():
    req = build_scoring_request(CALC_Q, "月产气=35×24×30=25200吨，吨矿产气≈75.2 kg/t")
    assert req.scoring_mode.value == "calculation"
    calc = req.scoring_config.calculation
    assert calc.final_answers[0].expected == 75.22
    assert calc.final_answers[0].tolerance == 0.5


def test_calculation_mode_without_config_stays_text():
    q = dict(CALC_Q)
    q.pop("calculation")
    req = build_scoring_request(q, "答案")
    assert req.scoring_mode.value == "text"   # 无配置不冒进，保持现状


def test_extra_equivalences_passthrough():
    q = {"id": "q42-2", "type": "short_answer", "score": 10.0, "scoring_mode": "text",
         "question": "提高淬透性的合金元素", "answer": "B、Mn、Mo、Cr、Si、Ni。",
         "scoring_points": [{"id": "p1", "text": "Mn", "score": 5}],
         "extra_equivalences": [["Mn", "锰"], ["Cr", "铬"]]}
    req = build_scoring_request(q, "锰、铬等元素")
    eqs = req.scoring_config.text_bounded_corrections.extra_equivalences
    assert ("Mn", "锰") in eqs and ("Cr", "铬") in eqs
```

- [ ] **Step 2: 运行确认失败**

```bash
cd /Users/yhw/Code/Github/examSystem && python -m pytest tests/worker/test_bridge_scoring_options.py -v
```
Expected: 2 failed, 1 passed（`without_config` 用例与现状一致，先行通过属正常）

- [ ] **Step 3: 实现 bridge 改动**

`build_scoring_request()` 的 mode 判定改为：

```python
    if mode_raw == "code" or (mode_raw == "" and has_lang):
        mode = ScoringMode.CODE
    elif mode_raw == "calculation" and isinstance(question.get("calculation"), dict):
        mode = ScoringMode.CALCULATION
    else:
        mode = ScoringMode.TEXT
```

`request_kwargs` 增加一项 `"scoring_config": _build_scoring_options(question),`（None 会被 `_build_scoring_request_kwargs` 的清洗逻辑丢弃）。新增：

```python
def _build_scoring_options(question: dict[str, Any]) -> Any | None:
    """题目带 calculation 配置或业务同义组时构造请求级 ScoringOptions。"""
    calc = question.get("calculation")
    eqs = question.get("extra_equivalences")
    if not isinstance(calc, dict) and not eqs:
        return None
    try:
        from subjective_scoring import ScoringOptions  # type: ignore
    except ImportError:
        from subjective_scoring.models.schemas import ScoringOptions  # type: ignore
    payload: dict[str, Any] = {}
    if isinstance(calc, dict):
        payload["calculation"] = calc
    if isinstance(eqs, list) and eqs:
        payload["text_bounded_corrections"] = {
            "extra_equivalences": [tuple(str(x) for x in g) for g in eqs if isinstance(g, list) and len(g) >= 2],
        }
    return ScoringOptions.model_validate(payload)
```

- [ ] **Step 4: 运行测试确认通过**

```bash
cd /Users/yhw/Code/Github/examSystem && python -m pytest tests/worker/test_bridge_scoring_options.py -v
```
Expected: 3 passed

- [ ] **Step 5: 标注计算题与等价词并提交**

- `mineral-processing.json` q42：`scoring_mode: "calculation"` + Step 1 测试中 CALC_Q 的 calculation 配置原样写入
- `mechanical.json` q43（已是 calculation 模式但缺配置）：`calculation.steps=[{id:"s1",description:"传动比 i≈1.67",expected:1.67,score:5,tolerance:0.05}]`，`final_answers=[{id:"f1",description:"n2=600 r/min",expected:600,score:5,tolerance:1}]`
- `metallurgy.json` q43（同上）：`steps=[{id:"s1",description:"硅锰合金 753kg",expected:753,score:10,tolerance:3}]`，`final_answers=[{id:"f1",description:"硅铁 135kg",expected:135,score:10,tolerance:2}]`
- `materials.json` q42 子题 q42-2（text 模式保持）：加 `extra_equivalences: [["B","硼"],["Mn","锰"],["Mo","钼"],["Cr","铬"],["Si","硅"],["Ni","镍"]]`
- `chemical-analysis.json` q42 子题 q42-1（text 模式保持）：评分点文本中 "H2SO4" 全部改写为 "浓硫酸/稀硫酸"（化学式会被库的数字/单位启发式误判为 数字2/4+单位S）

```bash
git add scoring_worker/grader_bridge.py data/papers/ tests/worker/test_bridge_scoring_options.py
git commit -m "feat(scoring): calculation/equivalences 请求级透传 + 计算题配置 (Task 2)"
```

---

### Task 3: translation + table 评分器 + 标注

**解决的误差项**：logistics q52 SIM-L（+5.7，中文套话骗分——非 CJK 评分点绕过库门槛）、SIM-M（−5.0，"No. 3" 触发英文否定词）、logistics q51、communications q44（+6.5，数值撞车+蹭分）

**Files:**
- Modify: `scoring_worker/exact_scorers/translation_scorer.py`、`table_scorer.py`
- Modify: `data/papers/{logistics,communications}.json`
- Test: `tests/worker/test_translation_scorer.py`、`tests/worker/test_table_scorer.py`

**Interfaces:**
- Consumes: `score_enumeration` 的 `_norm` / `_item_hit`（translation 短语命中复用枚举逻辑）
- Produces: `score_translation(question, student_answer)`、`score_table(question, student_answer)`

- [ ] **Step 1: 写失败测试（translation）**

```python
# tests/worker/test_translation_scorer.py
import pathlib, sys
ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from scoring_worker.exact_scorers import score_translation

LOGISTICS_Q52 = {
    "id": "q52", "type": "short_answer", "score": 10.0,
    "scoring_mode": "translation", "translation": {"target_lang": "en"},
    "question": "翻译：船长，为了保持船体平衡，我们要换3、5舱作业，请打开舱口。",
    "answer": "CAPTAIN! IN ORDER TO KEEP SHIP’S BALANCE, WE WILL SHIFT TO HOLD NO.3&5, PLEASE OPEN HATCH COVER.",
    "scoring_points": [
        {"id": "p1", "text": "CAPTAIN", "score": 2.0},
        {"id": "p2", "text": "IN ORDER TO KEEP SHIP'S BALANCE", "score": 3.0,
         "synonyms": ["keep the ship balanced", "keep ship balance", "even keel"]},
        {"id": "p3", "text": "WE WILL SHIFT TO HOLD NO.3&5", "score": 3.0,
         "synonyms": ["shift to no.3 and no.5", "shift to hold no.3", "no.3 and no.5 hatches"]},
        {"id": "p4", "text": "PLEASE OPEN HATCH COVER", "score": 2.0,
         "synonyms": ["open the hatch", "open hatch"]},
    ],
}


def test_correct_english_translation_full_marks():
    # SIM-M 真实作答：v0.1.11 被 "No. 3" 否定词误判压到 5.0/10，人工估分 10
    ans = "Captain, we will shift to No. 3 and No. 5 hatches to keep the ship balanced. Please open the hatches."
    r = score_translation(LOGISTICS_Q52, ans)
    assert r.score == 10.0 and r.review_level == "auto_pass"


def test_chinese_answer_to_english_task_zero():
    # SIM-L 真实作答：v0.1.11 跨语言相似度虚高给 5.7 且 auto_pass
    r = score_translation(LOGISTICS_Q52, "船上的事情听安排，按规范操作，注意安全。")
    assert r.score == 0.0 and r.review_level == "manual_required"
```

- [ ] **Step 2: 实现 score_translation**

```python
# scoring_worker/exact_scorers/translation_scorer.py 全文替换
"""翻译题精确评分器：先校验作答语言与目标语言一致，再按短语条目命中。"""
from __future__ import annotations

import re

from ._base import ExactScoreResult
from .enumeration_scorer import score_enumeration

_CJK_RE = re.compile(r"[一-鿿]")


def _cjk_ratio(text: str) -> float:
    chars = [c for c in str(text or "") if not c.isspace()]
    if not chars:
        return 0.0
    return sum(1 for c in chars if _CJK_RE.match(c)) / len(chars)


def score_translation(question: dict, student_answer: str) -> ExactScoreResult:
    cfg = question.get("translation") or {}
    target = str(cfg.get("target_lang") or "en").lower()
    ratio = _cjk_ratio(student_answer)
    if target == "en" and ratio > 0.5:
        return ExactScoreResult(0.0, {
            "reason": f"作答语言与目标语言不符（要求英文，中文占比 {ratio:.0%}）",
            "matched_points": [], "missed_points": [],
        }, "manual_required")
    if target == "zh" and ratio < 0.2:
        return ExactScoreResult(0.0, {
            "reason": f"作答语言与目标语言不符（要求中文，中文占比 {ratio:.0%}）",
            "matched_points": [], "missed_points": [],
        }, "manual_required")
    return score_enumeration(question, student_answer)
```

- [ ] **Step 3: 写失败测试（table，用 communications q44 真实数据）**

```python
# tests/worker/test_table_scorer.py
import pathlib, sys
ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from scoring_worker.exact_scorers import score_table

COMM_Q44 = {
    "id": "q44", "type": "short_answer", "score": 10.0,
    "scoring_mode": "table", "question": "根据表格中的IP地址规律，完善表格。",
    "answer": "A 掩码 255.255.255.0；B 网段 172.16.0.0，结束 172.16.255.254；C 起始 192.168.1.1，结束 192.168.1.254",
    "table": {"cells": [
        {"label": "a", "expected": ["255.255.255.0"], "score": 2.0, "require_label_context": True},
        {"label": "b", "expected": ["172.16.0.0"], "score": 2.0},
        {"label": "b", "expected": ["172.16.255.254"], "score": 2.0},
        {"label": "c", "expected": ["192.168.1.1"], "score": 2.0},
        {"label": "c", "expected": ["192.168.1.254"], "score": 2.0},
    ]},
}


def test_generic_mask_statement_scores_low():
    # SIM-M 真实作答：默认掩码通论，未填表。v0.1.11 给了 6.5，人工估分 0
    ans = "按首段分A、B、C类，默认掩码分别为255.0.0.0、255.255.0.0、255.255.255.0。"
    r = score_table(COMM_Q44, ans)
    assert r.score <= 2.0 and r.review_level == "manual_required"


def test_filled_table_full_marks():
    r = score_table(COMM_Q44, COMM_Q44["answer"])
    assert r.score == 10.0 and r.review_level == "auto_pass"
```

- [ ] **Step 4: 实现 score_table**

```python
# scoring_worker/exact_scorers/table_scorer.py 全文替换
"""表格补全评分器：单元格期望值精确匹配，可要求行标签出现在值的邻域。"""
from __future__ import annotations

import re
import unicodedata

from ._base import ExactScoreResult

# 保留 ASCII 句点（IP/掩码等含点值），只清空白与标点
_STRIP_RE = re.compile(r"[\s,，。;；:：、！!？?（）()\[\]【】\"'“”‘’\-—–/\\]+")
_CTX_WINDOW = 24


def _norm(text: str) -> str:
    return _STRIP_RE.sub("", unicodedata.normalize("NFKC", str(text or "")).casefold())


def score_table(question: dict, student_answer: str) -> ExactScoreResult:
    cells = (question.get("table") or {}).get("cells") or []
    max_score = float(question.get("score", 0) or 0)
    student_norm = _norm(student_answer)
    matched, missed, total = [], [], 0.0
    for i, cell in enumerate(cells):
        w = float(cell.get("score", 0) or 0)
        label = _norm(cell.get("label") or "")
        hit = None
        for v in (cell.get("expected") or []):
            nv = _norm(v)
            idx = student_norm.find(nv) if nv else -1
            if idx < 0:
                continue
            if cell.get("require_label_context") and label:
                ctx = student_norm[max(0, idx - _CTX_WINDOW): idx]
                if label not in ctx:
                    continue
            hit = v
            break
        cid = f"cell{i + 1}"
        if hit is not None:
            total += w
            matched.append({"point_id": cid, "score": w, "max_score": w,
                            "evidence": hit, "reason": "单元格值命中"})
        else:
            missed.append({"point_id": cid, "score": 0.0, "max_score": w,
                           "reason": f"未命中单元格 {cell.get('label')}：{cell.get('expected')}"})
    total = round(min(total, max_score), 4)
    detail = {"matched_points": matched, "missed_points": missed}
    if len(matched) < len(cells):
        detail["reason"] = "表格未完整填写，转人工核对"
        return ExactScoreResult(total, detail, "manual_required")
    return ExactScoreResult(total, detail, "auto_pass")
```

- [ ] **Step 5: 运行两组测试确认通过**

```bash
cd /Users/yhw/Code/Github/examSystem && python -m pytest tests/worker/test_translation_scorer.py tests/worker/test_table_scorer.py -v
```
Expected: 4 passed

- [ ] **Step 6: 标注并提交**

- `logistics.json` q52：`scoring_mode: "translation"` + `translation: {"target_lang": "en"}` + Step 1 测试中 LOGISTICS_Q52 的四点 synonyms 原样写入
- `logistics.json` q51：同构标注，四点 synonyms——p1 "HELLO! CHIEF OFFICER! I'M FOREMAN" 加 `["chief officer", "foreman"]`；p2 "IF IT IS RAINING" 加 `["if it rains", "in case of rain", "when raining"]`；p3 "PLEASE CLOSE HATCH COVER" 加 `["close the hatch", "close hatch"]`；p4 "STOP WORKING" 加 `["stop the operation", "stop cargo work", "suspend work"]`
- `communications.json` q44：`scoring_mode: "table"` + Step 3 测试中 COMM_Q44 的 table.cells 原样写入（保留原 scoring_points 不删，table 模式不读它）

```bash
git add scoring_worker/exact_scorers/ data/papers/logistics.json data/papers/communications.json \
        tests/worker/test_translation_scorer.py tests/worker/test_table_scorer.py
git commit -m "feat(scoring): translation/table 精确评分器 + 标注 (Task 3)"
```

---

### Task 4: ledger + case_analysis 评分器 + 标注

**解决的误差项**：finance q31 SIM-M（−6.7，概括作答被"数字不一致"清零）/ SIM-H（−5.0，"不确认收入"否定词误判）、legal q35 SIM-M（−11.9，套话签名误伤简洁结论）/ SIM-L（+3.1）

**Files:**
- Modify: `scoring_worker/exact_scorers/ledger_scorer.py`、`scoring_worker/case_analysis_scorer.py`
- Modify: `data/papers/{finance,legal}.json`
- Test: `tests/worker/test_ledger_scorer.py`、`tests/worker/test_case_analysis_scorer.py`

**Interfaces:**
- Consumes: `_norm`/`_item_hit`（enumeration）；case_analysis 回调 `grader_bridge.get_subjective_service()` + `build_scoring_request()`
- Produces: `score_ledger(question, student_answer)`、`score_case_analysis(question, student_answer)`

**ledger 设计**：`entries`（分录：全部 `numbers` 命中（相对容差）且任一 `keywords` 命中 → 得该条分）与 `treatment_points`（处理思路短语，概括作答的部分分）两个池，总分 `min(Σ, 满分)`。有任一命中 → suggested_review（金额正确性仍值得人眼扫过）；零命中且非空 → manual_required。

**case_analysis 设计**：`scoring_points` 中 `match: "phrase"` 的是结论点（走枚举命中）；其余是理由点（构造仅含理由点的浅拷贝题目走 text reranker）。`总分 = 结论分 + 理由分`；review_level 取两部分较严者（text 部分沿用库的 review_level）。

- [ ] **Step 1: 写失败测试（ledger，用 finance q31-1 真实数据）**

```python
# tests/worker/test_ledger_scorer.py
import pathlib, sys
ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from scoring_worker.exact_scorers import score_ledger

FINANCE_Q31_1 = {
    "id": "q31-1", "type": "short_answer", "score": 6.0,
    "scoring_mode": "ledger", "question": "编制业务（1）在12月份的相关会计分录。",
    "answer": "借：银行存款 10530 贷：应交税费-销项 1530 其他应付款 9000；借：财务费用 100 贷：其他应付款 100",
    "ledger": {
        "entries": [
            {"keywords": ["银行存款"], "numbers": [10530], "score": 1.2},
            {"keywords": ["销项税额", "应交税费", "销项"], "numbers": [1530], "score": 1.2},
            {"keywords": ["其他应付款"], "numbers": [9000], "score": 1.2},
            {"keywords": ["财务费用"], "numbers": [100], "score": 1.2},
        ],
        "treatment_points": [
            {"text": "不确认收入", "synonyms": ["融资", "售后回购按融资处理"], "score": 1.2},
        ],
    },
}


def test_full_entries_answer_near_full():
    # SIM-H 真实作答（缩写）：v0.1.11 被 "不确认收入" 否定词误判扣 p1
    ans = ("本业务为售后回购，实质是融资行为，不确认收入。借：银行存款10530；"
           "贷：其他应付款9000，应交税费—应交增值税（销项税额）1530。"
           "借：财务费用100；贷：其他应付款100。")
    r = score_ledger(FINANCE_Q31_1, ans)
    assert r.score == 6.0 and r.review_level == "suggested_review"


def test_prose_summary_gets_partial_credit():
    # SIM-M 真实作答：概括处理思路，只含 9000/300 两个数字
    ans = "售后回购按融资处理，不确认收入，收到的9000万元记入其他应付款，回购差价300万元分期计入财务费用。"
    r = score_ledger(FINANCE_Q31_1, ans)
    assert 2.0 <= r.score <= 3.0   # 其他应付款9000 + 处理思路 ≈ 2.4
```

- [ ] **Step 2: 实现 score_ledger**

```python
# scoring_worker/exact_scorers/ledger_scorer.py 全文替换
"""会计分录评分器：金额+科目关键词判分录，处理思路短语给概括作答部分分。"""
from __future__ import annotations

import re

from ._base import ExactScoreResult
from .enumeration_scorer import _item_hit, _norm

_NUM_RE = re.compile(r"-?\d+(?:\.\d+)?")


def _numbers_in(text: str) -> list[float]:
    return [float(m.group(0)) for m in _NUM_RE.finditer(str(text or ""))]


def _number_present(expected: float, student_nums: list[float], tol: float) -> bool:
    for n in student_nums:
        if abs(n - expected) <= max(abs(expected) * tol, 1e-9):
            return True
    return False


def score_ledger(question: dict, student_answer: str) -> ExactScoreResult:
    cfg = question.get("ledger") or {}
    max_score = float(question.get("score", 0) or 0)
    student_norm = _norm(student_answer)
    student_nums = _numbers_in(student_answer)
    matched, missed, total = [], [], 0.0
    for i, e in enumerate(cfg.get("entries") or []):
        w = float(e.get("score", 0) or 0)
        tol = float(e.get("tolerance", 0.005) or 0.005)
        nums_ok = all(_number_present(float(n), student_nums, tol) for n in (e.get("numbers") or []))
        kws = e.get("keywords") or []
        kw_ok = any(_norm(k) in student_norm for k in kws) if kws else True
        pid = f"entry{i + 1}"
        if nums_ok and kw_ok:
            total += w
            matched.append({"point_id": pid, "score": w, "max_score": w,
                            "evidence": "+".join(kws[:1] + [str(n) for n in (e.get("numbers") or [])]),
                            "reason": "分录命中（金额+科目）"})
        else:
            missed.append({"point_id": pid, "score": 0.0, "max_score": w,
                           "reason": f"分录未命中：{kws} {e.get('numbers')}"})
    for i, t in enumerate(cfg.get("treatment_points") or []):
        w = float(t.get("score", 0) or 0)
        pid = f"treatment{i + 1}"
        ev = _item_hit(t, student_norm)
        if ev is not None:
            total += w
            matched.append({"point_id": pid, "score": w, "max_score": w,
                            "evidence": ev, "reason": "处理思路命中"})
        else:
            missed.append({"point_id": pid, "score": 0.0, "max_score": w,
                           "reason": f"处理思路未命中：{t.get('text')}"})
    total = round(min(total, max_score), 4)
    detail = {"matched_points": matched, "missed_points": missed}
    if not matched and student_norm:
        detail["reason"] = "分录与处理思路均零命中，转人工"
        return ExactScoreResult(0.0, detail, "manual_required")
    return ExactScoreResult(total, detail, "suggested_review")
```

- [ ] **Step 3: 写失败测试（case_analysis，纯结论点用例不加载模型）**

```python
# tests/worker/test_case_analysis_scorer.py
import pathlib, sys
ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from scoring_worker.case_analysis_scorer import score_case_analysis

LEGAL_Q35_1 = {
    "id": "q35-1", "type": "short_answer", "score": 4.0,
    "scoring_mode": "case_analysis",
    "question": "01号房屋的物权归属应当如何确定？为什么？",
    "answer": "甲、丙办理了过户登记，完成不动产物权公示，物权由甲变更为丙。",
    "scoring_points": [
        {"id": "p1", "text": "归丙", "score": 2.0, "match": "phrase",
         "synonyms": ["归属丙", "丙取得", "属于丙", "所有权归丙", "变更为丙"]},
        # 本用例不放理由点，避免单测加载 reranker；理由点路径由 Task 6 回归覆盖
    ],
}


def test_conclusion_phrase_hit():
    # SIM-M 真实作答：v0.1.11 套话签名把 25 字正确结论压到 0.4/2
    r = score_case_analysis(LEGAL_Q35_1, "归丙，因为不动产以登记为准，丙已经办理了过户登记。")
    assert r.score == 2.0 and r.review_level == "suggested_review"


def test_conclusion_miss_goes_manual():
    r = score_case_analysis(LEGAL_Q35_1, "房子归谁要看具体情况，按法律规定来处理就行。")
    assert r.score == 0.0 and r.review_level == "manual_required"
```

- [ ] **Step 4: 实现 score_case_analysis**

```python
# scoring_worker/case_analysis_scorer.py 全文替换
"""案例分析评分器：phrase 结论点精确命中，其余理由点回调 text reranker。"""
from __future__ import annotations

from .exact_scorers._base import ExactScoreResult
from .exact_scorers.enumeration_scorer import _item_hit, _norm


def score_case_analysis(question: dict, student_answer: str) -> ExactScoreResult:
    points = question.get("scoring_points") or []
    phrase_pts = [p for p in points if p.get("match") == "phrase"]
    reason_pts = [p for p in points if p.get("match") != "phrase"]
    student_norm = _norm(student_answer)
    matched, missed, total = [], [], 0.0
    for p in phrase_pts:
        w = float(p.get("score", 0) or 0)
        ev = _item_hit(p, student_norm)
        if ev is not None:
            total += w
            matched.append({"point_id": p.get("id"), "score": w, "max_score": w,
                            "evidence": ev, "reason": "结论点命中"})
        else:
            missed.append({"point_id": p.get("id"), "score": 0.0, "max_score": w,
                           "reason": f"结论点未命中：{p.get('text')}"})
    level = "suggested_review"
    if reason_pts:
        r_score, r_detail, r_level = _score_reason_points(question, student_answer, reason_pts)
        total += r_score
        matched += r_detail.get("matched_points") or []
        missed += r_detail.get("missed_points") or []
        if r_level == "manual_required":
            level = "manual_required"
    if not matched and student_norm:
        level = "manual_required"
    total = round(min(total, float(question.get("score", 0) or 0)), 4)
    return ExactScoreResult(total, {"matched_points": matched, "missed_points": missed}, level)


def _score_reason_points(question: dict, student_answer: str, reason_pts: list) -> tuple:
    """理由点走 text reranker：构造只含理由点的浅拷贝题目回调库。"""
    from .grader_bridge import (build_scoring_request, detail_from_scoring_result,
                                get_subjective_service)
    sub_q = dict(question)
    sub_q["scoring_mode"] = "text"
    sub_q["scoring_points"] = reason_pts
    sub_q["score"] = round(sum(float(p.get("score", 0) or 0) for p in reason_pts), 4)
    svc = get_subjective_service()
    result = svc.score(build_scoring_request(sub_q, student_answer))
    entry = detail_from_scoring_result(sub_q, student_answer, result)
    level = "suggested_review" if entry["review_status"] in (
        "high_confidence", "reviewed") else "manual_required"
    return float(entry["machine_score"]), entry.get("detail") or {}, level
```

- [ ] **Step 5: 运行测试确认通过**

```bash
cd /Users/yhw/Code/Github/examSystem && python -m pytest tests/worker/test_ledger_scorer.py tests/worker/test_case_analysis_scorer.py -v
```
Expected: 4 passed

- [ ] **Step 6: 标注 finance q31 与 legal q35 并提交**

`finance.json` q31 的 5 个子题全部 `scoring_mode: "ledger"`（金额取自参考分录，treatment 给概括作答部分分）：
- q31-1（6 分）：按 Step 1 测试 FINANCE_Q31_1 原样写入
- q31-2（6 分）：entries 应收账款1755 / 主营业务收入1500 / 销项255 / 主营业务成本750（各 1.2）+ treatment "正常销售确认收入"（synonyms ["符合收入确认条件"]，1.2）
- q31-3（6 分）：entries 收入600 / 销项102 / 应收账款702 / 成本300（各 1.2）+ treatment "销售退回冲减当期收入"（synonyms ["冲减收入", "红字发票"]，1.2）
- q31-4（9 分）：entries 发出商品600 / 收入750 / 销项127.5 / 成本450 / 销售费用75（各 1.5）+ treatment "代销发出商品不确认收入"（synonyms ["收到代销清单确认收入"]，1.5）
- q31-5（13 分）：entries 银行存款956（2）/ 劳务收入22.5（2.5）/ 劳务成本15（2.5）+ treatment "设备收入待安装验收后确认"（synonyms ["不确认设备收入", "预收账款"]，3）与 "按完工进度确认劳务收入"（synonyms ["完工百分比", "75%"]，3）

`legal.json` q35 的 7 个子题全部 `scoring_mode: "case_analysis"`，每子题 1-2 个 phrase 结论点 + 原评分点并为 reason 点，结论 synonyms：
- q35-1 "归丙" `["归属丙", "丙取得", "属于丙", "变更为丙"]`；q35-2 "合同有效" + "不构成恶意串通" `["单纯知情", "不属于恶意串通"]`
- q35-3 "变更合同" `["合意变更", "双方变更", "新合同取代"]`（点文本删除 "2月12日" 日期）；q35-4 "不应支持" `["不能得到支持", "不予支持"]`
- q35-5 "解除合同" `["请求解除", "主张解除"]` + "返还" `["返还购房款", "返还首付"]`
- q35-6 "A公司承担" `["由a公司赔偿", "a公司承担"]` + "丙承担相应责任" `["丙有过错", "选任过失"]`；q35-7 "B公司承担" `["由b公司承担", "b公司赔偿"]`

```bash
git add scoring_worker/ data/papers/finance.json data/papers/legal.json \
        tests/worker/test_ledger_scorer.py tests/worker/test_case_analysis_scorer.py
git commit -m "feat(scoring): ledger/case_analysis 评分器 + finance/legal 标注 (Task 4)"
```

---

### Task 5: 题库 lint 脚本 + 快照同步脚本

**Files:**
- Create: `scripts/lint_papers.py`、`scripts/sync_run_snapshots.py`
- Test: `tests/test_lint_papers.py`

**Interfaces:**
- Consumes: `data/papers/*.json` 全量标注结果（Task 1-4）
- Produces: `lint_paper(doc) -> list[str]`（错误清单，空即通过）；CLI 非零退出码即失败；`sync_run_snapshots.py` 执行后 `exam_runs.snapshot_hash` 与快照内容一致

**lint 规则**（L1-L6，检查范围含 composite 子题）：
- L1 元评分点：text/enumeration 模式的评分点文本含 `答出.*之[一二三四]|任答|任意.*[条点项]` 句式 → error
- L2 非中文评分点：text 模式评分点无 CJK 字符（库的有界修正会整体跳过）→ error（应改 translation/table 或加标注）
- L3 评分点含日期（`\d+月\d+日`）→ error（会触发库的数字硬校验）
- L4 化学式/单字母风险：text 模式评分点含 `[A-Z][a-z]?\d`（如 H2SO4）或独立单字母拉丁实体 → warning
- L5 模式配置完整性：enumeration 需非空 scoring_points；translation 需 `translation.target_lang`；table 需 `table.cells` 且 Σcell.score == 题目 score；ledger 需 `ledger.entries`；calculation 需 `calculation.final_answers` 且 Σ(steps+final_answers).score == 题目 score；case_analysis 需至少 1 个 `match:"phrase"` 点 → error
- L6 分值和：enumeration/case_analysis 的 Σpoint.score 若小于题目 score → warning（枚举允许超出后 min 封顶，不足则满分不可达）

- [ ] **Step 1: 写失败测试**

```python
# tests/test_lint_papers.py
import pathlib, sys
ROOT = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from scripts.lint_papers import lint_paper


def _paper(*questions):
    return {"paper_id": "t", "name": "t", "exam_info": {}, "questions": list(questions)}


def test_meta_point_detected():
    q = {"id": "q1", "type": "short_answer", "score": 10, "scoring_mode": "text",
         "question": "环保工作?", "answer": "x",
         "scoring_points": [{"id": "p1", "text": "答出参考答案中的公司环保工作之一", "score": 5}]}
    assert any("L1" in e for e in lint_paper(_paper(q)))


def test_date_in_point_detected():
    q = {"id": "q1", "type": "short_answer", "score": 10, "scoring_mode": "text",
         "question": "x", "answer": "x",
         "scoring_points": [{"id": "p1", "text": "2月12日双方变更合同", "score": 10}]}
    assert any("L3" in e for e in lint_paper(_paper(q)))


def test_table_score_sum_checked():
    q = {"id": "q1", "type": "short_answer", "score": 10, "scoring_mode": "table",
         "question": "x", "answer": "x",
         "table": {"cells": [{"label": "a", "expected": ["1"], "score": 4}]}}
    assert any("L5" in e for e in lint_paper(_paper(q)))


def test_clean_paper_passes():
    q = {"id": "q1", "type": "short_answer", "score": 10, "scoring_mode": "enumeration",
         "question": "x", "answer": "x",
         "scoring_points": [{"id": "p1", "text": "大气污染", "score": 10}]}
    assert lint_paper(_paper(q)) == []
```

- [ ] **Step 2: 实现 scripts/lint_papers.py**

```python
#!/usr/bin/env python3
"""题库静态检查：python scripts/lint_papers.py [data/papers/*.json]，非零退出码=有 error。"""
from __future__ import annotations

import glob
import json
import re
import sys

_META_RE = re.compile(r"答出.*之[一二三四]|任答|任意.*[条点项]")
_DATE_RE = re.compile(r"\d+月\d+日")
_CJK_RE = re.compile(r"[一-鿿]")
_FORMULA_RE = re.compile(r"[A-Z][a-z]?\d")
_EXACT_MODES = {"enumeration", "translation", "table", "ledger", "case_analysis", "calculation"}


def _units(question: dict):
    """题目本体 + composite 子题，统一检查。"""
    yield question
    for sub in question.get("subquestions") or []:
        yield sub


def _sum_scores(items, key="score") -> float:
    return round(sum(float(i.get(key, 0) or 0) for i in items), 2)


def lint_paper(doc: dict) -> list[str]:
    errs: list[str] = []
    slug = doc.get("paper_id", "?")
    for q in doc.get("questions") or []:
        if q.get("type") in ("single_choice", "multiple_choice", "true_false"):
            continue
        for u in _units(q):
            uid = f"{slug}/{u.get('id')}"
            mode = str(u.get("scoring_mode") or "text").lower()
            points = u.get("scoring_points") or []
            score = float(u.get("score", 0) or 0)
            for p in points:
                text = str(p.get("text") or "")
                if mode in ("text", "enumeration") and _META_RE.search(text):
                    errs.append(f"L1 {uid}: 元评分点句式「{text[:30]}」，应改为具体条目")
                if mode == "text" and text and not _CJK_RE.search(text):
                    errs.append(f"L2 {uid}: 非中文评分点「{text[:30]}」会跳过库的有界修正")
                if _DATE_RE.search(text):
                    errs.append(f"L3 {uid}: 评分点含日期「{text[:30]}」，会触发数字硬校验")
                if mode == "text" and _FORMULA_RE.search(text):
                    errs.append(f"L4(warning) {uid}: 评分点含化学式/编号「{text[:30]}」")
            if mode == "enumeration" and not points:
                errs.append(f"L5 {uid}: enumeration 模式无 scoring_points")
            if mode == "translation" and not (u.get("translation") or {}).get("target_lang"):
                errs.append(f"L5 {uid}: translation 模式缺 target_lang")
            if mode == "table":
                cells = (u.get("table") or {}).get("cells") or []
                if not cells or abs(_sum_scores(cells) - score) > 0.01:
                    errs.append(f"L5 {uid}: table.cells 缺失或分值和 {_sum_scores(cells)} != {score}")
            if mode == "ledger" and not (u.get("ledger") or {}).get("entries"):
                errs.append(f"L5 {uid}: ledger 模式缺 entries")
            if mode == "calculation":
                calc = u.get("calculation") or {}
                items = list(calc.get("steps") or []) + list(calc.get("final_answers") or [])
                if not calc.get("final_answers") or abs(_sum_scores(items) - score) > 0.01:
                    errs.append(f"L5 {uid}: calculation 配置缺失或分值和 {_sum_scores(items)} != {score}")
            if mode == "case_analysis" and not any(p.get("match") == "phrase" for p in points):
                errs.append(f"L5 {uid}: case_analysis 缺 phrase 结论点")
            if mode in ("enumeration", "case_analysis") and points and _sum_scores(points) < score - 0.01:
                errs.append(f"L6(warning) {uid}: 评分点分值和 {_sum_scores(points)} < 满分 {score}，满分不可达")
    return errs


def main() -> int:
    files = sys.argv[1:] or sorted(glob.glob("data/papers/*.json"))
    all_errs = [e for f in files for e in lint_paper(json.load(open(f)))]
    for e in all_errs:
        print(e)
    hard = [e for e in all_errs if "(warning)" not in e]
    print(f"\n{len(files)} papers, {len(hard)} errors, {len(all_errs) - len(hard)} warnings")
    return 1 if hard else 0


if __name__ == "__main__":
    sys.exit(main())
```

- [ ] **Step 3: 跑测试 + 全量清扫**

```bash
cd /Users/yhw/Code/Github/examSystem
python -m pytest tests/test_lint_papers.py -v          # 预期 4 passed
python scripts/lint_papers.py                          # 输出剩余问题清单
```
对 lint 输出的每条 error 逐一修 `data/papers/*.json`（Task 1-4 已覆盖大部分；剩余典型：instrumentation q43 无评分点属有意配置，L5 不涉及；electrical q43 开放题 `scoring_points: []` 同样跳过——lint 对显式空数组不报错，需在 `lint_paper` 的 points 判空前加 `if isinstance(u.get("scoring_points"), list) and not u.get("scoring_points"): continue`）。修到 `errors == 0` 为止（warnings 允许保留）。

- [ ] **Step 4: 实现 scripts/sync_run_snapshots.py**

```python
#!/usr/bin/env python3
"""papers -> exam_runs 快照同步：重写快照 questions 并更新 snapshot_hash。

Go 侧校验的是 canonical JSON（sort_keys + 紧凑分隔符 + 非 ASCII 不转义）
的 sha256，与本脚本 canonical() 一致；文件本身的缩进格式不影响校验。
"""
from __future__ import annotations

import hashlib
import json
import os
import subprocess

DSN = os.environ.get(
    "DATABASE_URL",
    "postgres://exam_app:exam_app_dev@127.0.0.1:5432/exam_system?sslmode=disable",
)
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def canonical(doc) -> bytes:
    return json.dumps(doc, ensure_ascii=False, sort_keys=True,
                      separators=(",", ":")).encode("utf-8")


def main() -> None:
    out = subprocess.run(
        ["psql", DSN, "-t", "-A", "-F", "|", "-c",
         "SELECT id, paper_id, snapshot_path FROM exam_runs WHERE snapshot_path IS NOT NULL;"],
        capture_output=True, text=True, check=True).stdout
    papers: dict[str, dict] = {}
    for line in out.splitlines():
        if not line.strip():
            continue
        run_id, paper_id, snap_path = line.split("|", 2)
        if paper_id not in papers:
            papers[paper_id] = json.load(open(os.path.join(ROOT, "data", "papers", f"{paper_id}.json")))
        path = snap_path if os.path.isabs(snap_path) else os.path.join(ROOT, snap_path)
        snap = json.load(open(path))
        snap["questions"] = papers[paper_id]["questions"]
        with open(path, "w", encoding="utf-8") as f:
            json.dump(snap, f, ensure_ascii=False, indent=2)
        digest = hashlib.sha256(canonical(snap)).hexdigest()
        subprocess.run(["psql", DSN, "-q", "-c",
                        f"UPDATE exam_runs SET snapshot_hash='{digest}' WHERE id='{run_id}';"],
                       check=True, capture_output=True)
        print(f"{run_id}  {paper_id}  {digest[:12]}")


if __name__ == "__main__":
    main()
```

- [ ] **Step 5: 提交**

```bash
git add scripts/lint_papers.py scripts/sync_run_snapshots.py tests/test_lint_papers.py data/papers/
git commit -m "feat(scoring): 题库 lint + run 快照同步脚本，全量清扫通过 (Task 5)"
```

---

### Task 6: 54 份基准卷回归 + 合并 main

**Files:**
- Create: `benchmarks/subjective-54/baseline_v0111_perq.jsonl`（冻结基线）
- 不改代码；只执行、验证、合并

- [ ] **Step 1: 冻结 v0.1.11 逐题基线（必须在触发任何 regrade 之前）**

```bash
cd /Users/yhw/Code/Github/examSystem
python3 - <<'EOF'
import json, os, subprocess
DSN = os.environ.get("DATABASE_URL",
    "postgres://exam_app:exam_app_dev@127.0.0.1:5432/exam_system?sslmode=disable")
out = subprocess.run(["psql", DSN, "-t", "-A", "-F", "\x01", "-c",
    "SELECT paper_id, employee_id, grading_detail_json::text FROM submissions "
    "WHERE id BETWEEN 3 AND 56;"], capture_output=True, text=True, check=True).stdout
with open("benchmarks/subjective-54/baseline_v0111_perq.jsonl", "w") as f:
    for line in out.splitlines():
        if not line.strip():
            continue
        slug, emp, detail = line.split("\x01", 2)
        for e in json.loads(detail):
            if e.get("type") in ("single_choice", "multiple_choice", "true_false"):
                continue
            ms = e.get("machine_score")
            if ms is None:
                continue
            f.write(json.dumps({"slug": slug, "emp": emp,
                                "qid": str(e.get("question_id") or e.get("id")),
                                "machine": float(ms)}, ensure_ascii=False) + "\n")
print("frozen")
EOF
git add benchmarks/subjective-54/baseline_v0111_perq.jsonl
git commit -m "chore(benchmarks): 冻结 v0.1.11 逐题分数作回归对照"
```

- [ ] **Step 2: 全套单测 + lint 通过**

```bash
cd /Users/yhw/Code/Github/examSystem
python -m pytest tests/worker/ tests/test_lint_papers.py -v
python scripts/lint_papers.py
```
Expected: 单测全 passed；lint `0 errors`

- [ ] **Step 3: 同步 run 快照**

```bash
python scripts/sync_run_snapshots.py    # 每行输出 run_id / paper_id / hash 前缀
```

- [ ] **Step 4: 重启 worker（严守环境要求，防词法回退）**

```bash
pkill -f "python -m scoring_worker" || true
cd /Users/yhw/Code/Github/examSystem
set -a && source .env && set +a
unset ALL_PROXY all_proxy HTTP_PROXY http_proxy HTTPS_PROXY https_proxy
export DATABASE_URL='postgres://exam_app:exam_app_dev@127.0.0.1:5432/exam_system?sslmode=disable' \
       HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1
nohup .venv/bin/python -m scoring_worker >> /tmp/scoring_worker.log 2>&1 &
sleep 5 && tail -20 /tmp/scoring_worker.log
```
检查：日志无 "加载 CrossEncoder 失败"；重评开始后单份卷耗时应为秒级——毫秒级完成即是词法回退，立即停下排查环境。

- [ ] **Step 5: 触发 54 份重评并轮询完成**

```bash
TOKEN=$(curl -s -X POST http://127.0.0.1:8000/api/admin/login \
        -H 'Content-Type: application/json' -d '{"password":"admin123"}' \
        | python3 -c "import sys,json;print(json.load(sys.stdin)['token'])")
for id in $(seq 3 56); do
  curl -s -X POST "http://127.0.0.1:8000/api/admin/regrade/$id" \
       -H "Authorization: Bearer $TOKEN" > /dev/null
done
# 轮询直至 54 份全部完成（grading_status 不再有 pending/grading）
watch -n 10 "psql 'postgres://exam_app:exam_app_dev@127.0.0.1:5432/exam_system?sslmode=disable' \
  -t -A -c \"SELECT grading_status, count(*) FROM submissions WHERE id BETWEEN 3 AND 56 GROUP BY 1;\""
```

- [ ] **Step 6: 回归指标核对**

```bash
python3 benchmarks/subjective-54/compare.py
```
对照 Global Constraints 的验收线逐条核对：
1. `误差<=3` 当前值 ≥167/177（即 |误差|>3 ≤10）
2. `MAE` 当前值 ≤1.20
3. 整卷总分 `<=3分` 卷数不下降；客观题零偏差（compare.py 不含客观题差异即为零偏差，另抽 2 份人工核对 objective_score 未变）
4. 新增退化对照冻结基线：

```bash
python3 - <<'EOF'
import json, os, subprocess
DSN = os.environ.get("DATABASE_URL",
    "postgres://exam_app:exam_app_dev@127.0.0.1:5432/exam_system?sslmode=disable")
est = json.load(open("benchmarks/subjective-54/est_perq.json"))
old = {}
for line in open("benchmarks/subjective-54/baseline_v0111_perq.jsonl"):
    r = json.loads(line)
    old[(r["slug"], r["emp"], r["qid"])] = r["machine"]
out = subprocess.run(["psql", DSN, "-t", "-A", "-F", "\x01", "-c",
    "SELECT paper_id, employee_id, grading_detail_json::text FROM submissions "
    "WHERE id BETWEEN 3 AND 56;"], capture_output=True, text=True, check=True).stdout
regressed = []
for line in out.splitlines():
    if not line.strip():
        continue
    slug, emp, detail = line.split("\x01", 2)
    for e in json.loads(detail):
        if e.get("type") in ("single_choice", "multiple_choice", "true_false"):
            continue
        qid = str(e.get("question_id") or e.get("id"))
        ms, key = e.get("machine_score"), (slug, emp, qid)
        ev = est.get(slug, {}).get(emp, {}).get(qid)
        if ms is None or ev is None or key not in old:
            continue
        if abs(old[key] - ev) <= 3 < abs(float(ms) - ev):
            regressed.append(f"{slug} {emp} {qid}: est={ev} v0111={old[key]} now={ms}")
print(f"新增退化 {len(regressed)} 例:")
print("\n".join(regressed) or "(无)")
EOF
```
预期：`新增退化 0 例`；若有，逐例归因——数据标注错误立即修（回到对应 Task 的标注步骤重跑 Step 3-6），确属可接受的（如估分标准本身修正）书面记录后放行。

- [ ] **Step 7: 验收通过 → 合并 main；未通过 → 修复后从 Step 2 重跑**

```bash
# 仅当 Step 6 四项验收全部通过才执行
cd /Users/yhw/Code/Github/examSystem
git checkout main
git merge --no-ff feature/exact-scoring-modes \
    -m "feat(scoring): 题库分模式精确评分（enumeration/translation/table/ledger/case_analysis + calculation 透传）

54 份基准回归：|误差|>3 24→N，逐题 MAE 1.59→M（N/M 填实测值）"
git push origin main
```

未通过时：不合并、不回滚分支；按 Step 6 的归因结果修对应 Task 的代码或标注，提交后从 Step 2 重跑。若多轮仍收敛不到验收线，停下把逐例归因清单交人工决策（放宽验收线或调整题库标注策略）。

回归通过后建议顺手更新 `主观题评分误差分析.md`：在各误差案例下追加「修复 exact-modes」标注（与 v0.1.9 修复标注同格式），单独提交。

---

## Self-Review 检查记录

- 覆盖检查：22 项大误差全部有归宿——枚举 9 项（Task 1）、计算 3 项（Task 2）、翻译/表格 4 项（Task 3）、分录/案例 4 项（Task 4）、意译类 2 项（chemical-engineering q43、mineral-processing q43 → 枚举零命中转人工，属预期兜底）
- 类型一致性：`ExactScoreResult(score, detail, review_level)` 三字段在 Task 0 定义，Task 1-4 全部按位置参数构造；`_norm`/`_item_hit` 由 enumeration_scorer 导出供 translation/ledger/case_analysis 复用；table_scorer 自带保留小数点的 `_norm`（IP 值需要），不与枚举版混用
- 占位符扫描：无 TBD/TODO；所有测试均含真实作答与期望值；所有标注给出具体字段值或明确的同构规则
- 风险提示：`_item_hit` 的 bigram 0.75 阈值与 synonyms 质量直接决定枚举题效果，Task 6 回归若出现枚举题退化，优先调整该题 synonyms 而非阈值本身

