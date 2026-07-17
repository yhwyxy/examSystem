from __future__ import annotations

import argparse
import json
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from backend.question_loader import validate_questions


PAPER_SLUGS = {
    "仪器仪表": "instrumentation",
    "冶金专业": "metallurgy",
    "化学分析": "chemical-analysis",
    "化学工程与工艺": "chemical-engineering",
    "安全管理": "safety-management",
    "机械专业": "mechanical",
    "材料专业": "materials",
    "法务": "legal",
    "焊接专业": "welding",
    "物流管理": "logistics",
    "环保专业": "environmental",
    "电气": "electrical",
    "矿物加工": "mineral-processing",
    "能源与动力工程": "energy-power",
    "财务管理": "finance",
    "软件开发": "software-development",
    "通信工程": "communications",
    "金属材料专业": "metal-materials",
}


def atomic_write_json(path: Path, data: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(
        json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    temporary.replace(path)


def import_papers(source_dir: Path, papers_dir: Path) -> dict[str, Any]:
    index_path = papers_dir / "index.json"
    index = (
        json.loads(index_path.read_text(encoding="utf-8"))
        if index_path.exists()
        else {"papers": []}
    )
    existing = {
        item["slug"]: item
        for item in index.get("papers", [])
        if isinstance(item, dict) and item.get("slug")
    }
    now = datetime.now(timezone.utc).isoformat()
    imported: list[dict[str, Any]] = []

    for name, slug in PAPER_SLUGS.items():
        source = source_dir / f"试卷（{name}）.json"
        if not source.exists():
            raise FileNotFoundError(f"缺少已转换试卷: {source}")
        paper = json.loads(source.read_text(encoding="utf-8"))
        paper["paper_id"] = slug
        paper["name"] = name
        validate_questions(paper)
        atomic_write_json(papers_dir / f"{slug}.json", paper)

        meta = {
            "slug": slug,
            "name": name,
            "status": "closed",
            "question_count": len(paper["questions"]),
            "total_score": float(paper["exam_info"]["total_score"]),
            "updated_at": now,
        }
        existing[slug] = meta
        imported.append(meta)

    original_order = [
        item["slug"]
        for item in index.get("papers", [])
        if isinstance(item, dict) and item.get("slug") in existing
    ]
    imported_order = [slug for slug in PAPER_SLUGS.values() if slug not in original_order]
    index["papers"] = [existing[slug] for slug in [*original_order, *imported_order]]
    atomic_write_json(index_path, index)
    return {"imported_count": len(imported), "papers": imported}


def main() -> int:
    parser = argparse.ArgumentParser(description="导入 Word 转换后的专业试卷。")
    parser.add_argument("source_dir", type=Path)
    parser.add_argument("--papers-dir", required=True, type=Path)
    args = parser.parse_args()
    result = import_papers(args.source_dir, args.papers_dir)
    print(json.dumps(result, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
