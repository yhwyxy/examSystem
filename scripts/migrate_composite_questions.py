"""Idempotently migrate approved papers to the canonical composite schema."""
from __future__ import annotations

import json
from pathlib import Path
from typing import Any


TARGETS = {
    "mechanical": {"q41": ["三视图组成及投影规律", "主视图、俯视图、左视图；长对正、高平齐、宽相等。"], "q44": ["换向阀控制方式", "人力控制、机械控制、电气控制、直接压力控制、先导控制。"]},
    "materials": {"q42": ["钢的淬透性定义", "在规定条件下钢淬火获得马氏体的能力。", "提高淬透性的合金元素", "B、Mn、Mo、Cr、Si、Ni。"], "q43": ["过冷度定义", "理论结晶温度和实际结晶温度的差值。", "过冷度影响", "过冷度越大，结晶速度越快、晶粒越细。"]},
    "instrumentation": {"q41": ["零位误差", "输入标准信号为零时设备显示不为零的值。", "测量范围", "仪器能够测量的最小值与最大值之间的范围。"], "q42": ["引用误差", "绝对误差与仪表满量程值之比。", "绝对误差", "测量值与真实值之差。"], "q43": ["测量范围变化", "100-200 kPa。", "仪表量程", "100 kPa。", "输出对应压力", "4mA 对应100kPa，12mA 对应150kPa，20mA 对应200kPa。"]},
    "chemical-analysis": {"q42": ["稀硫酸配置方法", "将浓硫酸徐徐加入水中并搅拌散热，不可将水加入酸中。"], "q43": ["反应方程式", "Na2CO3 + 2HCl → 2NaCl + H2O + CO2↑。", "盐酸浓度计算", "0.2000 mol/L。"]},
    "chemical-engineering": {"q42": ["影响因素", "反应物浓度、温度、催化剂、诱导作用。", "加速方法", "增加浓度、升高温度、加入正催化剂或利用诱导反应。"], "q44": ["鉴别试剂", "选用 Ba(OH)2 溶液分别加入并加热。", "鉴别现象", "白色沉淀且刺激性气体为硫酸铵；仅气体为氯化铵；仅沉淀为硫酸钠；均无为氯化钠。"]},
    "metal-materials": {"q43": ["铁碳合金基本相", "铁素体、奥氏体、渗碳体。", "机械性能", "铁素体强度硬度不高但塑韧性好；奥氏体硬度低塑性高；渗碳体硬度高、脆性大。"]},
    "legal": {},
}


def _composite(q: dict[str, Any], parts: list[str]) -> dict[str, Any]:
    n = len(parts) // 2
    base = q.get("score", 0)
    scores = [round(base / n, 2)] * n
    scores[-1] = round(base - sum(scores[:-1]), 2)
    subs = [{"id": f"{q['id']}-{i+1}", "question": parts[i*2], "answer": parts[i*2+1], "score": scores[i], "scoring_mode": "text"} for i in range(n)]
    q["type"] = "composite"
    q["subquestions"] = subs
    q.pop("sub_questions", None)
    return q


def _legal(qs: list[dict[str, Any]]) -> None:
    selected = [q for q in qs if q.get("id") in {"q35", "q36", "q37", "q38", "q39"}]
    if not selected or any(q.get("type") == "composite" for q in selected):
        return
    parent = dict(selected[0])
    parent["id"] = "q35"
    parent["score"] = 32.0
    parent["question"] = selected[0]["question"].split("\n1.", 1)[0]
    parent.pop("answer", None)
    parts = [
        ("01号房屋的物权归属应当如何确定？为什么？", "01号房屋的物权归属应当以不动产登记簿为准。丙已经支付全部房款并办理过户登记，故01号房屋所有权已经转移给丙。"),
        ("甲、丙之间的房屋买卖合同效力如何？应考虑哪些因素？", "甲、丙之间于2月8日形成的房屋买卖合同,该合同为有效合同。尽管甲已就该房与乙签订合同,但甲丙行为不违反公序良俗及强制性规定,不存在无效因素,也不属于恶意串通损害乙权利。"),
        ("2月12日甲、乙修改原合同的行为效力如何？为什么？", "2月12日,甲、乙之间修改合同的行为,该行为有效,其性质属于双方变更合同。双方受变更后的合同的约束。"),
        ("乙的诉讼请求是否应当得到支持？为什么？", selected[1]["answer"]),
        ("针对甲要求乙履行购买02号房的义务，乙可主张什么权利？为什么？", selected[2]["answer"]),
        ("邻居丁所遭受的损失应当由谁赔偿？为什么？", selected[3]["answer"]),
        ("丙热水器的毁损应由谁承担赔偿责任？为什么？", selected[4]["answer"]),
    ]
    parent["subquestions"] = [{"id": f"q35-{i+1}", "question": a, "answer": b, "score": s, "scoring_mode": "text"} for i, (s, (a,b)) in enumerate(zip([4,5,5,5,5,4,4], parts))]
    parent["type"] = "composite"
    qs[:] = [q for q in qs if q.get("id") not in {"q35","q36","q37","q38","q39"}]
    qs.insert(next((i for i,q in enumerate(qs) if q.get('id','') > 'q35'), len(qs)), parent)


def migrate_all(root: Path, *, report_path: Path) -> dict[str, Any]:
    report = {"version": 1, "migrated": [], "skipped": [], "processed": []}
    for slug, mapping in TARGETS.items():
        path = root / f"{slug}.json"; data = json.loads(path.read_text())
        report["processed"].extend(f"{slug}:{qid}" for qid in (mapping or ({"q35": None} if slug == "legal" else {})))
        changed = False
        for q in data.get("questions", []):
            if slug == "instrumentation" and q.get("id") == "q43" and q.get("type") == "composite":
                q["subquestions"][0].update({"question": "测量范围（含上下界）", "answer": "仪表的测量范围变为100-200 kPa。", "scoring_mode": "calculation", "calculation": {"final_answers": [{"id": "lower-bound", "description": "测量范围下限", "expected": 100, "score": 3.5, "tolerance": 0.1, "keywords": ["下限", "100", "kPa"]}, {"id": "upper-bound", "description": "测量范围上限", "expected": 200, "score": 3.5, "tolerance": 0.1, "keywords": ["上限", "200", "kPa"]}]}})
                q["subquestions"][1].update({"calculation": {"final_answers": [{"id": "span", "description": "仪表量程", "expected": 100, "score": 6, "tolerance": 0.1}]}})
                q["subquestions"][2].update({"calculation": {"final_answers": [{"id": "out-4mA", "description": "4mA对应输入压力", "expected": 100, "score": 2.34, "tolerance": 0.1}, {"id": "out-12mA", "description": "12mA对应输入压力", "expected": 150, "score": 2.33, "tolerance": 0.1}, {"id": "out-20mA", "description": "20mA对应输入压力", "expected": 200, "score": 2.33, "tolerance": 0.1}]}})
                changed = True
            if q.get("id") in mapping and q.get("type") != "composite":
                _composite(q, mapping[q["id"]]); changed = True; report["migrated"].append(f"{slug}:{q['id']}")
        if slug == "instrumentation" and not any(q.get("id") == "q43" for q in data["questions"]):
            data["questions"].append({"id":"q43","type":"composite","question":"压力变送器零位迁移计算","score":20.0,"subquestions":[{"id":"q43-1","question":"测量范围（含上下界）","answer":"仪表的测量范围变为100-200 kPa。","score":7.0,"scoring_mode":"calculation","calculation":{"final_answers":[{"id":"lower-bound","description":"测量范围下限","expected":100,"score":3.5,"tolerance":0.1,"keywords":["下限","100","kPa"]},{"id":"upper-bound","description":"测量范围上限","expected":200,"score":3.5,"tolerance":0.1,"keywords":["上限","200","kPa"]}]}},{"id":"q43-2","question":"仪表量程","answer":"100 kPa","score":6.0,"scoring_mode":"calculation","calculation":{"final_answers":[{"id":"span","description":"仪表量程","expected":100,"score":6,"tolerance":0.1}]}},{"id":"q43-3","question":"输出对应压力","answer":"输入100 kPa输出4mA；输入150 kPa输出12mA；输入200 kPa输出20mA。","score":7.0,"scoring_mode":"calculation","calculation":{"final_answers":[{"id":"out-4mA","description":"4mA对应输入压力","expected":100,"score":2.34,"tolerance":0.1},{"id":"out-12mA","description":"12mA对应输入压力","expected":150,"score":2.33,"tolerance":0.1},{"id":"out-20mA","description":"20mA对应输入压力","expected":200,"score":2.33,"tolerance":0.1}]}}]}); changed = True; report["migrated"].append("instrumentation:q43")
            data.setdefault("exam_info", {})["total_score"] = 100.0
        if slug == "legal" and not any(q.get("id") == "q35" and q.get("type") == "composite" for q in data["questions"]):
            _legal(data["questions"]); changed = True; report["migrated"].append("legal:q35")
        if slug == "legal":
            data.setdefault("exam_info", {})["total_score"] = 100.0
            parent = next((q for q in data["questions"] if q.get("id") == "q35" and q.get("type") == "composite"), None)
            if parent:
                answers = ["01号房屋的物权归属应当以不动产登记簿为准。丙已经支付全部房款并办理过户登记，故01号房屋所有权已经转移给丙。", "甲、丙之间于2月8日形成的房屋买卖合同,该合同为有效合同。尽管甲已就该房与乙签订合同,但甲丙行为不违反公序良俗及强制性规定,不存在无效因素,也不属于恶意串通损害乙权利。", "2月12日,甲、乙之间修改合同的行为,该行为有效,其性质属于双方变更合同。双方受变更后的合同的约束。"]
                for sub, answer in zip(parent["subquestions"][:3], answers): sub["answer"] = answer
                changed = True
        if slug == "instrumentation":
            data.setdefault("exam_info", {})["total_score"] = 100.0
            changed = True
        if changed: path.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n")
    report_path.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    return report


if __name__ == "__main__":
    migrate_all(Path("data/papers"), report_path=Path("data/papers/composite-migration-report.json"))
