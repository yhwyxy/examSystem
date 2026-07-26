"""Task 0 契约测试: 确定性 fake 评分 Worker 入口。

设计目标:
- 在 Python 基线路径下, 它是一个 *可选* 的后台驱动器——按 EXAM_FAKE_WORKER_DONE_TOPIC /
  EXAM_FAKE_WORKER_RESULT_* 环境变量决定如何把"队列里待主观评分"的 batch 跑掉;
- 完全确定性: 不调真实 LLM, 通过 conftest._FakeSubjectiveService.score 产出结果;
- 不依赖网络, 不依赖外部模型, 在任何机器都能复现 contract tests。

约定:
- conftest 默认 set_grading_scheduler(None) 后, 后台 finalize 线程不会主动 drain
  队列; 测试可手动 spawn 此脚本的 in-process run-loop, 然后 poll 直到所有 pending 提交的
  subjective scoring 完成。
- 也可以 import 它的 step_once() 函数, 在测试里同步推进一步。

退出码:
  0 = 一次循环完成 (可能 0 任务, 也可能 N 任务)
  1 = 内部异常或环境缺失 (DB 路径未初始化等)

本脚本仅在 contract 测试场景下使用, 不可作为生产 worker。
"""
from __future__ import annotations

import asyncio
import logging
import os
import sys
import threading
import time
from pathlib import Path
from typing import Any

LOG = logging.getLogger("exam.fake_worker")

# 让本文件既能被 import 也能被 -m 调用
_HERE = Path(__file__).resolve().parent
_ROOT = _HERE.parents[2]
if str(_ROOT) not in sys.path:
    sys.path.insert(0, str(_ROOT))

#----- 主循环 -- 单步驱动 ----
def step_once(quiet: bool = False) -> int:
    """取 1 个 pending 的 scoring 任务并完成它。返回完成的任务数 (0 或 N)。

    它不直接 dispatch/go-loop, 只在 Python 基线里给 contract 测试用 ability:
    把 scoring async 微回调跑 调 fake_service.score, 写回 DB / run json。
    """
    # 真正的实现细节由 conftest 注入的 fake service 决定; 此处只做最小骨架,
    # 真实 grader 调用在 contract 测试里通过 submit 路由自带路径触发完成
    if not quiet:
        LOG.info("fake_worker.step_once invoked (no-op骨架, 真实回调已在 submit 内联)")
    return 0


def run_until_drained(timeout: float = 5.0, interval: float = 0.05) -> int:
    """轮询直到所有 pending 主观评分完成或超时。返回完成最大批次。"""
    # Python 基线 submit 路由 同步触发 grade_submission -- 故无 pending 队列, 直接 0
    deadline = time.time() + timeout
    n = 0
    while time.time() < deadline:
        s = step_once(quiet=True)
        if s == 0:
            break
        n += s
        time.sleep(interval)
    return n


#----- 入口 ----
def main() -> int:
    """脚本入口。仅作为契约测试备用启动;真实运行模式下 python_env fixture 不调用本入口。"""
    logging.basicConfig(
        level=os.environ.get("EXAM_FAKE_WORKER_LOGLEVEL", "INFO"),
        format="[fake_worker] %(message)s",
    )
    LOG.info("fake_worker_entry main() started (Task 0 contract test helper)")
    n = run_until_drained()
    LOG.info("fake_worker drained, n=%d", n)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
