"""成绩导出。"""
from __future__ import annotations

import json
from io import BytesIO
from typing import Any

from . import database
from .question_loader import get_question_list


def _answer_to_text(answer: Any) -> str:
    if isinstance(answer, (list, dict)):
        return json.dumps(answer, ensure_ascii=False)
    if isinstance(answer, bool):
        return "正确" if answer else "错误"
    return "" if answer is None else str(answer)


def export_submissions_xlsx() -> bytes:
    try:
        from openpyxl import Workbook
        from openpyxl.styles import Font, PatternFill
    except ModuleNotFoundError as e:
        raise RuntimeError("导出 Excel 需要安装 openpyxl，请执行 pip install -r requirements.txt") from e

    rows = database.list_submissions(sort_by="submitted_at", order="desc")
    questions = get_question_list()

    wb = Workbook()
    ws = wb.active
    ws.title = "考试成绩"

    base_headers = ["ID", "姓名", "工号", "部门", "客观题分", "主观题机器分", "主观题最终分", "总分", "复核状态", "提交时间"]
    question_headers = [f"{q['id']}({q['score']}分)" for q in questions]
    headers = base_headers + question_headers
    ws.append(headers)

    header_fill = PatternFill("solid", fgColor="D9EAF7")
    for cell in ws[1]:
        cell.font = Font(bold=True)
        cell.fill = header_fill

    # 流式逐行 append：openpyxl 在 append 时即序列化，避免显式持有中间结构。
    # 注：write_only 模式可进一步降内存但不支持 column_dimensions 列宽，
    # 当前考试系统数据量（百~千行）下普通模式已足够，保留列宽体验。
    for r in rows:
        answers = json.loads(r.get("answers_json") or "{}")
        base = [
            r.get("id"), r.get("name"), r.get("employee_id"), r.get("department"),
            r.get("objective_score"), r.get("subjective_score_machine"),
            r.get("subjective_score_final"), r.get("total_score"),
            r.get("review_status"), r.get("submitted_at"),
        ]
        ans_cols = [_answer_to_text(answers.get(q["id"])) for q in questions]
        ws.append(base + ans_cols)

    # 列宽：按表头长度估算，避免遍历全部 cell 造成的二次全表扫描
    from openpyxl.utils import get_column_letter
    for idx, h in enumerate(headers, start=1):
        letter = get_column_letter(idx)
        # 基础列宽：表头长度 + 2，上限 45
        ws.column_dimensions[letter].width = min(len(str(h)) + 2, 45)

    buf = BytesIO()
    wb.save(buf)
    return buf.getvalue()
