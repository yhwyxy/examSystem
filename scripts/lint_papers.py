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

            # 显式空 scoring_points（开放题）跳过后续检查
            is_empty_list = isinstance(u.get("scoring_points"), list) and not u.get("scoring_points")
            for p in points:
                has_eqs = bool(u.get("extra_equivalences"))
                text = str(p.get("text") or "")
                if mode in ("text", "enumeration") and _META_RE.search(text):
                    errs.append(f"L1 {uid}: 元评分点句式「{text[:30]}」，应改为具体条目")
                if mode == "text" and text and not _CJK_RE.search(text):
                    tag = f"L4(warning)" if has_eqs else "L2"
                    errs.append(f"{tag} {uid}: 非中文评分点「{text[:30]}」" + ("（已配等价组，放行）" if has_eqs else "会跳过库的有界修正"))
                if _DATE_RE.search(text):
                    errs.append(f"L3 {uid}: 评分点含日期「{text[:30]}」，会触发数字硬校验")
                if mode == "text" and _FORMULA_RE.search(text):
                    errs.append(f"L4(warning) {uid}: 评分点含化学式/编号「{text[:30]}」")

            if is_empty_list:
                continue  # 开放题不检查模式配置完整性
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
