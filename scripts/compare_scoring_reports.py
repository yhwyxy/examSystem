"""Compare two scoring validation JSON reports."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any


SPECIALIST_SUFFIX = "-specialist"


def _rounded(value: float) -> float:
    return round(float(value), 4)


def compare_reports(baseline: dict[str, Any], candidate: dict[str, Any]) -> dict[str, Any]:
    baseline_metrics = baseline["metrics"]
    candidate_metrics = candidate["metrics"]
    metric_deltas = {
        "workflow_success_rate": _rounded(
            candidate_metrics["workflow_success_rate"]
            - baseline_metrics["workflow_success_rate"]
        ),
        "scoring_error_count": int(candidate_metrics["scoring_error_count"])
        - int(baseline_metrics["scoring_error_count"]),
        "ordering_accuracy": _rounded(
            candidate_metrics["ordering"]["accuracy"]
            - baseline_metrics["ordering"]["accuracy"]
        ),
    }

    def submission_map(report: dict[str, Any]) -> dict[tuple[str, str], float]:
        return {
            (item["paper_id"], item["quality"]): float(item["total_score"])
            for item in report["submissions"]
            if str(item["paper_id"]).endswith(SPECIALIST_SUFFIX)
        }

    baseline_submissions = submission_map(baseline)
    candidate_submissions = submission_map(candidate)
    submission_deltas = []
    for paper_id, quality in baseline_submissions.keys() & candidate_submissions.keys():
        old = baseline_submissions[(paper_id, quality)]
        new = candidate_submissions[(paper_id, quality)]
        submission_deltas.append(
            {
                "paper_id": paper_id,
                "quality": quality,
                "baseline": old,
                "candidate": new,
                "delta": _rounded(new - old),
            }
        )
    quality_order = {"complete": 0, "partial": 1, "wrong": 2}
    submission_deltas.sort(
        key=lambda item: (item["paper_id"], quality_order.get(item["quality"], 99))
    )

    def record_map(report: dict[str, Any]) -> dict[tuple[str, str, str], float]:
        return {
            (item["paper_id"], item["quality"], item["question_id"]): float(
                item["actual_score"]
            )
            for item in report["records"]
            if str(item["paper_id"]).endswith(SPECIALIST_SUFFIX)
        }

    baseline_records = record_map(baseline)
    candidate_records = record_map(candidate)
    question_changes = []
    for paper_id, quality, question_id in baseline_records.keys() & candidate_records.keys():
        old = baseline_records[(paper_id, quality, question_id)]
        new = candidate_records[(paper_id, quality, question_id)]
        question_changes.append(
            {
                "paper_id": paper_id,
                "quality": quality,
                "question_id": question_id,
                "baseline": old,
                "candidate": new,
                "delta": _rounded(new - old),
            }
        )
    question_changes.sort(key=lambda item: abs(item["delta"]), reverse=True)

    return {
        "metric_deltas": metric_deltas,
        "submission_deltas": submission_deltas,
        "largest_question_changes": question_changes[:10],
    }


def render_markdown(comparison: dict[str, Any]) -> str:
    metrics = comparison["metric_deltas"]
    lines = [
        "# Scoring Comparison: v0.1.2 → v0.1.3",
        "",
        "## Metric Changes",
        "",
        f"- Workflow success rate: `{metrics['workflow_success_rate']:+.4f}`",
        f"- Scoring error count: `{metrics['scoring_error_count']:+d}`",
        f"- Ordering accuracy: `{metrics['ordering_accuracy']:+.4f}`",
        "",
        "## Specialized Paper Totals",
        "",
        "| Paper | Quality | v0.1.2 | v0.1.3 | Delta |",
        "|---|---|---:|---:|---:|",
    ]
    for item in comparison["submission_deltas"]:
        lines.append(
            f"| {item['paper_id']} | {item['quality']} | {item['baseline']:.1f} | "
            f"{item['candidate']:.1f} | {item['delta']:+.1f} |"
        )
    lines.extend(
        [
            "",
            "## Largest Question-Level Changes",
            "",
            "| Paper | Quality | Question | v0.1.2 | v0.1.3 | Delta |",
            "|---|---|---|---:|---:|---:|",
        ]
    )
    for item in comparison["largest_question_changes"]:
        lines.append(
            f"| {item['paper_id']} | {item['quality']} | {item['question_id']} | "
            f"{item['baseline']:.1f} | {item['candidate']:.1f} | {item['delta']:+.1f} |"
        )
    return "\n".join(lines) + "\n"


def compare_versions(reports: dict[str, dict[str, Any]]) -> dict[str, Any]:
    versions = list(reports)
    if len(versions) < 2:
        raise ValueError("at least two versions are required")

    submission_maps: dict[str, dict[tuple[str, str], float]] = {}
    record_maps: dict[str, dict[tuple[str, str, str], float]] = {}
    for version, report in reports.items():
        submission_maps[version] = {
            (item["paper_id"], item["quality"]): float(item["total_score"])
            for item in report["submissions"]
            if str(item["paper_id"]).endswith(SPECIALIST_SUFFIX)
        }
        record_maps[version] = {
            (item["paper_id"], item["quality"], item["question_id"]): float(
                item["actual_score"]
            )
            for item in report["records"]
            if str(item["paper_id"]).endswith(SPECIALIST_SUFFIX)
        }

    expected_submission_keys = set(submission_maps[versions[0]])
    expected_record_keys = set(record_maps[versions[0]])
    for version in versions[1:]:
        if set(submission_maps[version]) != expected_submission_keys:
            raise ValueError(f"submission keys differ for {version}")
        if set(record_maps[version]) != expected_record_keys:
            raise ValueError(f"record keys differ for {version}")

    quality_order = {"complete": 0, "partial": 1, "wrong": 2}
    totals = []
    for paper_id, quality in sorted(
        expected_submission_keys,
        key=lambda key: (key[0], quality_order.get(key[1], 99)),
    ):
        scores = {
            version: submission_maps[version][(paper_id, quality)]
            for version in versions
        }
        totals.append(
            {
                "paper_id": paper_id,
                "quality": quality,
                "scores": scores,
                "deltas": {
                    f"{versions[index - 1]}->{versions[index]}": _rounded(
                        scores[versions[index]] - scores[versions[index - 1]]
                    )
                    for index in range(1, len(versions))
                },
                "overall_delta": _rounded(scores[versions[-1]] - scores[versions[0]]),
            }
        )

    ordering_status: dict[str, dict[str, bool]] = {}
    papers = sorted({paper_id for paper_id, _ in expected_submission_keys})
    for version in versions:
        ordering_status[version] = {}
        for paper_id in papers:
            scores = submission_maps[version]
            ordering_status[version][paper_id] = (
                scores[(paper_id, "complete")]
                > scores[(paper_id, "partial")]
                > scores[(paper_id, "wrong")]
            )

    previous_version, latest_version = versions[-2:]
    latest_changes = []
    for paper_id, quality, question_id in expected_record_keys:
        old = record_maps[previous_version][(paper_id, quality, question_id)]
        new = record_maps[latest_version][(paper_id, quality, question_id)]
        latest_changes.append(
            {
                "paper_id": paper_id,
                "quality": quality,
                "question_id": question_id,
                "baseline": old,
                "candidate": new,
                "delta": _rounded(new - old),
            }
        )
    latest_changes.sort(key=lambda item: abs(item["delta"]), reverse=True)

    metrics = {}
    for version, report in reports.items():
        source = report["metrics"]
        metrics[version] = {
            "workflow_success_rate": float(source["workflow_success_rate"]),
            "scoring_error_count": int(source["scoring_error_count"]),
            "band_hit_rate": float(source.get("band_hit_rate", 0.0)),
            "mean_absolute_error": float(source.get("mean_absolute_error", 0.0)),
            "ordering_accuracy": float(source["ordering"]["accuracy"]),
        }

    return {
        "versions": versions,
        "metrics": metrics,
        "totals": totals,
        "ordering_status": ordering_status,
        "largest_latest_changes": latest_changes[:10],
    }


def render_versions_markdown(comparison: dict[str, Any]) -> str:
    versions = comparison["versions"]
    arrow_title = " → ".join(versions)
    lines = [
        f"# Scoring Comparison: {arrow_title}",
        "",
        "## Version Metrics",
        "",
        "| Version | Workflow | Errors | Band hit | MAE | Ordering |",
        "|---|---:|---:|---:|---:|---:|",
    ]
    for version in versions:
        metric = comparison["metrics"][version]
        lines.append(
            f"| {version} | {metric['workflow_success_rate']:.1%} | "
            f"{metric['scoring_error_count']} | {metric['band_hit_rate']:.1%} | "
            f"{metric['mean_absolute_error']:.3f} | {metric['ordering_accuracy']:.1%} |"
        )

    delta_headers = [f"Δ {versions[index - 1]}→{versions[index]}" for index in range(1, len(versions))]
    lines.extend(
        [
            "",
            "## Specialized Paper Totals",
            "",
            "| Paper | Quality | " + " | ".join(versions + delta_headers + ["Δ overall"]) + " |",
            "|---|---|" + "---:|" * (len(versions) + len(delta_headers) + 1),
        ]
    )
    for item in comparison["totals"]:
        scores = [f"{item['scores'][version]:.1f}" for version in versions]
        deltas = [
            f"{item['deltas'][f'{versions[index - 1]}->{versions[index]}']:+.1f}"
            for index in range(1, len(versions))
        ]
        lines.append(
            f"| {item['paper_id']} | {item['quality']} | "
            + " | ".join(scores + deltas + [f"{item['overall_delta']:+.1f}"])
            + " |"
        )

    lines.extend(["", "## Quality Ordering", ""])
    for version in versions:
        for paper_id, passed in comparison["ordering_status"][version].items():
            lines.append(f"- `{version}` `{paper_id}`: {'pass' if passed else 'FAIL'}")

    lines.extend(
        [
            "",
            f"## Largest Question Changes: {versions[-2]} → {versions[-1]}",
            "",
            "| Paper | Quality | Question | Previous | Latest | Delta |",
            "|---|---|---|---:|---:|---:|",
        ]
    )
    for item in comparison["largest_latest_changes"]:
        lines.append(
            f"| {item['paper_id']} | {item['quality']} | {item['question_id']} | "
            f"{item['baseline']:.1f} | {item['candidate']:.1f} | {item['delta']:+.1f} |"
        )
    return "\n".join(lines) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("baseline", type=Path, nargs="?")
    parser.add_argument("candidate", type=Path, nargs="?")
    parser.add_argument("output", type=Path, nargs="?")
    parser.add_argument("--output-file", type=Path)
    parser.add_argument(
        "--version",
        action="append",
        default=[],
        metavar="LABEL=PATH",
        help="compare multiple labeled reports; repeat for each version",
    )
    args = parser.parse_args()
    output_path = args.output_file or args.output
    if output_path is None:
        parser.error("an output path is required")

    if args.version:
        reports: dict[str, dict[str, Any]] = {}
        for item in args.version:
            if "=" not in item:
                parser.error("--version must use LABEL=PATH")
            label, raw_path = item.split("=", 1)
            reports[label] = json.loads(Path(raw_path).read_text(encoding="utf-8"))
        output = render_versions_markdown(compare_versions(reports))
    else:
        if args.baseline is None or args.candidate is None:
            parser.error("baseline and candidate are required without --version")
        baseline = json.loads(args.baseline.read_text(encoding="utf-8"))
        candidate = json.loads(args.candidate.read_text(encoding="utf-8"))
        output = render_markdown(compare_reports(baseline, candidate))
    output_path.write_text(output, encoding="utf-8")
    print(output_path)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
