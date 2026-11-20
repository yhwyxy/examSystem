# subjective-scoring 0.1.14 发布候选

2026-10-14 候选已构建和验证，尚未创建提交、PR 或 tag，未发布到远程，也未部署到生产。

## 产物与范围

- wheel：`packages/subjective_scoring-0.1.14-py3-none-any.whl`，SHA-256 `fec3b89fb323c9302fb31b7eded650a7f808cb25c60284bbb9d0856a1f46dc6a`。
- sdist 与产物核对记录：本机 `/private/tmp/scorer-release-0.1.14/`。源码包包含 sandbox 部署文件和双人标注评估说明；wheel 的26个源码模块与当前评分库逐字一致。
- worker 候选镜像：`exam-goal-worker:0.1.14-candidate`，本机镜像 ID `sha256:b1e17f923981903c9e826f34f9bb0c209a028b4ac6afde32674d9b612d3b33fb`。
- 评分库版本及自身 lock 已同步0.1.14；此前 lock 的本地项目版本仍为0.1.8。Docker 默认 wheel 版本、Git/Docker wheel 放行规则和 Windows 离线安装版本已同步候选，旧wheel保留用于核对/回退。

候选新增受限 SQL/代码执行、认证 HTTP sandbox、逐点执行依据与 gate、SQLite显式修改策略、部署清理和双人标注评估工具。无法验证或缺少其他可靠判断的结果保留人工复核；没有声明真实模型质量达标。

## 已执行验证

- 评分库源码全量：304 passed。
- 安装候选 wheel 后关闭源码路径导入的全量：304 passed；确认模块实际来自临时 venv 的 site-packages。
- examSystem 对安装候选 wheel 的全量兼容回归：279 passed，使用隔离本地 PostgreSQL 和 Go API。
- worker 镜像构建通过；以 network=none运行，验证实际库版本0.1.14、uid10001、双类远程 runner 配置可用。该镜像还核对当前worker repository源码SHA-256 af48c83718bde355bb296c012d3d7d427d05cb6083c400cbd0f18c55d70ec47e；该检查不涉及模型 preflight，也不冒充 Docker 镜像的业务队列验收。
- 之前的真实 SQL与三语言业务闭环、14项 smoke与13项systemd生命周期记录使用源码模块逐字一致的旧metadata候选；该行为证据与本次新包内容核对分别记录在开发检查点，不能把旧产物版本冒充本次正式release。

## 发布与依赖同步门槛

`tests/test_dependency_boundaries.py`要求根工程和worker的开发依赖固定同一GitHub tag。当前两份pyproject和lock保留真实v0.1.13，未填写虚构v0.1.14提交号。Windows离线安装明确要求0.1.14，以避免已安装0.1.13的环境被无版本约束误判为满足依赖。

后续须按授权的Git流程为评分库候选创建短期分支、显式审查/提交、推送并建立PR；通过review和必要检查后，再处理版本tag。真实v0.1.14存在后，同步根工程/worker的Git依赖及两份lock、边界测试和候选状态说明，重新验证锁解析、安装及应用回归。库README中的候选状态说明需在正式发布流程中更新并重建最终产物，核对最终哈希。当前候选默认构建仅供隔离验收。

正式生产准入仍需批准的sandbox主机/TLS、真实模型和双人独立标注评估、P7灰度与恢复流程，见发布手册。P6按用户决定在输入可用前保持开放。

2026-10-14 应用候选补充题库审计迁移 0013：具名身份绑定、原因与前后内容、审核版本围栏、审计失败回滚及不可修改历史。该变更仅影响 Go API、数据库与题库页面；上述 worker 镜像及评分库 artifact 不变。Go 实际数据库回归通过；应用使用安装的 0.1.14 库全量 279 passed，未配置契约专用 DSN 的跳过运行不算此验收结果。P6 按用户决定保留开放，真实模型及人工标注证据仍待提供。

当前 Go 候选还补齐 P3/P4 租约实际截止和 attempt_count 围栏，防止同进程重新领取后的旧执行写回。最新 Go 全量数据库回归、vet及安装候选库的应用279项回归通过；当前二进制恢复报告为 `/private/tmp/exam-goal-restore-rehearsals/restore-rehearsal-4247be6783de4e7e95d63a1598adb108/report.json`。worker镜像和库包没有因此改变，正式Git版本发布仍待授权。
