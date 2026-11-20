# 隔离 SQL / 代码执行服务

P2 的执行服务运行在独立 sandbox 主机或 VM 上。该主机不保存考试数据库凭据、模型 API 密钥、管理员密码或业务文件；业务 worker 通过专用 HTTPS 协议调用它。Docker daemon 权限只授予该隔离主机的服务账号，不能把业务宿主 Docker socket 挂入 worker。

当前工作区已包含服务、客户端和验收程序，但还没有发布相应新版评分库或完成独立主机部署验收。这份操作说明不代表已部署。

2026-10-13 本机 OrbStack 已启动，Docker Engine 29.8.2（linux/arm64）上的固定 Python 镜像完成 stdin、网络/文件/凭据隔离、超时、输出限制和 SQLite 查询/修改状态共 11 项实际容器检查；固定 Node 22 镜像的 stdin 用例也通过。随后固定 Java 21 镜像通过编译/stdin，并在真实 loopback HTTP 服务上通过完整三语言 smoke 及认证拒绝，共 14 项。本机容器证据不等于独立 sandbox 主机、TLS、systemd 或正式发布库的验收。具体镜像 digest 与本地报告路径见开发检查点，生产上线仍需下述完整流程。

## 发布与配置

先从 subjective-scoring 工作区发布包含 `sandbox_transport`、`sandbox_server` 和 `sandbox_smoke` 的版本，并同步 worker 内置 wheel、`Dockerfile.worker` 版本、两个 pyproject/lock 文件及 wheel 忽略规则。当前 v0.1.13 的正式 wheel 不含这项接入，不能直接启用下面的环境变量。

在独立 Linux sandbox 主机安装 Python 3.12、Docker 和获批评分库的 `[sql,remote]` wheel。为 Python 3.12、Node 22 和 Java 21 JDK 选择并记录已审核的镜像 digest，预先拉取镜像；运行时始终 `--pull=never`。将评分库的 `deploy/sandbox/sandbox.env.example` 填好后放在 `/etc/subjective-sandbox.env`，只允许部署管理员读取。服务端 `SANDBOX_RUNNER_BUILD` 填获批 release 产物标识；服务会附加实际三种镜像配置的 SHA-256 指纹，生成 `/health` 返回的完整 `runner_build`。worker 填这个完整标识，任一镜像 digest 改变都会导致版本不匹配并转复核。

按 `deploy/sandbox/subjective-sandbox.service` 创建专用账号、venv 和 systemd 服务，把同目录 `cleanup-containers.sh` 放在 `/opt/subjective-sandbox/`。工作目录为 `/opt/subjective-sandbox`，venv 为其 `venv/`；运行时临时文件放在 `/run/subjective-sandbox`，由 Docker daemon 可见。不能启用 `PrivateTmp`，否则宿主 daemon 看不到代码 bind mount。服务启动时拒绝继承常见业务凭据，并要求三种运行镜像全部固定到 SHA-256 digest。容器带固定 sandbox label，systemd 的 `ExecStopPost` 在服务退出/失败后清理带该标记的遗留容器；不会选择未标记容器。此脚本仅供独立 sandbox daemon 使用，容器清理也必须做真实崩溃验收。

部署记录还需包含 systemd 的实际配置与进程状态：

```bash
systemctl cat subjective-sandbox.service
systemctl show subjective-sandbox.service \
  -p User -p SupplementaryGroups -p NoNewPrivileges -p ProtectSystem \
  -p ProtectHome -p PrivateDevices -p ReadWritePaths -p MemoryMax -p TasksMax
pid="$(systemctl show subjective-sandbox.service -p MainPID --value)"
sed -n '/^NoNewPrivs:/p' "/proc/$pid/status"
```

有效值须与 unit 一致：专用用户及 docker 补充组、`NoNewPrivileges=yes`、`ProtectSystem=strict`、`ProtectHome=yes`、`PrivateDevices=yes`、仅运行目录可写、512 MiB 内存和 64 个任务限制；内核 `NoNewPrivs` 为 1。任何 drop-in 改变这些要求时，暂停准入并处理具体平台配置，再重做执行与生命周期验收。

服务默认只监听 `127.0.0.1:8099`，由 sandbox 主机上的 TLS 反向代理公开 `/v1/execute` 与 `/health`，防火墙只允许业务 worker 来源。代理请求体上限应为 3 MiB，上游超时至少 70 秒，不记录 Authorization、请求体或响应体。服务按一次一个请求执行，容器的 CPU、内存、进程数、输出和时间限制仍由固定 runner 控制；扩容使用额外隔离主机。

评分库 `deploy/sandbox/nginx.conf.example` 提供已验证的代理配置：替换获批域名和证书路径，执行 `nginx -t` 后启用。证书必须在 worker 实际运行环境的信任库中受信任，并匹配 URL 主机名；客户端启用完整证书校验、拒绝重定向且忽略环境代理。私有 CA 须按部署方批准的信任流程安装，不能关闭证书校验。

业务部署 `.env` 和 Compose worker 已支持：

```dotenv
SANDBOX_API_URL=https://sandbox.example.com
SANDBOX_AUTH_TOKEN=<独立随机 token，至少 32 字符>
SANDBOX_RUNNER_BUILD=<服务端返回的完整 release:镜像清单指纹>
```

三项必须同时配置，修改后重启 worker。客户端拒绝凭据嵌入 URL、明文远程 HTTP 和重定向，不读取环境代理；只发送原始代码/SQL及验证用例，不发送数据库连接、考生账号、整卷或模型配置。返回结果经过条目数量、通过数、状态、原因和 build 校验；隐藏名称、标准输出及额外字段不会进入成绩依据。调用失败、非法响应或 build 不匹配转“无法验证”并要求人工复核，不按考生答错处理。

LLM 成功评分和参考答案完全一致的 SQL/代码结果，也必须经过同一执行验证。执行接收原始作答，避免代码或 SQL 归一化改变字符串字面量。

## SQL 修改语句的教师配置

默认 `query` 模式仅验证读取查询。若题目要求修改数据，教师可在高级评分配置中设置：

```json
{
  "sql_dialect": "sqlite",
  "sql_execution": {"mode": "mutation", "mutation_tables": ["items"]}
}
```

还必须配置至少两组 `sql_test_datasets`，每组都包含允许修改的表及初始数据。该策略和参考 SQL 随题目审核版本及考试快照固定。mutation 首批支持单条 INSERT、UPDATE、DELETE；参考与学生语句在相同初始数据的独立内存库执行，比较所有 fixture 表的最终内容，保留重复行及 NULL，既不共用执行后的数据库，也不只比较被修改行数。

只允许写入教师指定的表；ATTACH、PRAGMA、DDL、跨数据库目标、多个语句、未配置写操作和未支持方言返回“无法验证”，进入人工复核。测试 schema 仍使用现有 columns/type/rows 结构，不配置触发器或额外约束。错误 WHERE、遗漏行或重复行变化会产生 `state_mismatch` 证据；环境、参考执行和安全限制失败不会冒充考生答错。报告保存通过数、记录原因和行数，隐藏样例内容不进入成绩详情；界面以执行记录展示相应评分点依据。

## 真实环境验收

在持有 sandbox 专用 token 的验收环境运行：

```bash
python -m subjective_scoring.sandbox_smoke \
  --url https://sandbox.example.com --build <已批准的固定标识>
```

保存 JSON 输出和退出码：只有全部检查为 true 且退出 0 才通过。检查包括 Python/JavaScript/Java 的 stdin、公开与隐藏用例、外部网络及业务文件/凭据不可见、死循环超时、输出限制，以及 SQLite 排序、重复行、NULL、已配置 INSERT/UPDATE/DELETE 状态验证和未配置写操作的处理。`/health` 只是 API 存活检测，不能替代该验收。再完成 worker 经真实 PostgreSQL 队列评分并显示执行依据的闭环，验证服务中断/build 改变时转人工复核和发布被阻止。

本地协议测试使用真实 loopback HTTP 连接和可控 fake 执行器，只证明传输、认证与校验行为。真实镜像、容器生命周期、OS 隔离和部署恢复仍需以上环境验收，不能以 fake 测试的通过代替。

本地 SQL 业务契约支持复用已启动的 sandbox：在隔离 PostgreSQL 配置之外设置上述三项 `SANDBOX_*`，运行 `python -m pytest -q tests/contract/test_p0_scoring_flow.py::test_configured_sql_mutation_crosses_api_snapshot_worker_and_detail`。配置不完整会失败，不会回落到离线 harness。正确/错误 SQL、build 不匹配与不可用端点均通过实际 worker 队列进入 API 详情；后两种完成当前检测后仍必须以 `REVIEW_REQUIRED` 拒绝发布。普通回归没有配置 sandbox 时显式使用 loopback + SQLite harness，不能把它的结果登记成 Docker 验收。

2026-10-13 已用真实固定镜像服务执行该组四个用例；在服务正常时执行前三项，然后实际停止该次验收拥有的服务，保留原 endpoint、设置 `EXAM_SANDBOX_STOPPED=1`，执行 `-k unavailable` 验证中断。该变量仅为验收说明服务已停止，不负责停止外部服务。报告记录在本地开发检查点；它证明 SQL 队列闭环，没有证明代码题业务队列、生产 TLS/systemd、宿主隔离部署或正式评分库发布。

同日另以真实服务执行 `tests/contract/test_p0_scoring_flow.py::test_code_execution_crosses_worker_history_reports_and_publication` 的 Python、JavaScript、Java 三项契约，验证执行记录、历史、必要人工复核、当前检测与发布。代码路径保留原有复核要求，测试通过不自动清除该要求。默认测试使用明确的代码协议 fixture；只有配置真实固定镜像服务的运行才属于 Docker 业务验收。

本地还完成服务 SIGKILL 后的遗留容器验证：在确认所有 sandbox 标签目标均属该次测试后手动运行部署清理脚本，标签容器移除，无标签对照容器保留，中断请求无法验证。共享 daemon 上不能直接执行该脚本；部署仍要求独立 sandbox daemon。此测试没有运行 Linux systemd，因此自动 `ExecStopPost`、重启和独立主机隔离仍待现场验收。

随后在专用 OrbStack 隔离 Ubuntu 24.04 测试机中安装候选 wheel、导入固定 ARM64 manifest 镜像并执行 Linux systemd 验收。平台的全局 `zzz-lxc-service.conf` 起初关闭了部分保护；仅对该测试服务添加更晚的 drop-in 恢复原 unit 选项，核对有效属性与内核位后，14 项执行 smoke 和 13 项生命周期检查通过，包括自动 `ExecStopPost`、自动重启、无标签容器保留、正常停止、运行目录删除、再次启动及保护设置保持。测试机禁用宿主共享与 SSH agent，未导入业务凭据；报告导出后已停止。

该测试机属于 OrbStack LXC 环境，仍共享外层虚拟机内核；报告仅证明上述 Linux 服务及其独立内层 Docker daemon 的行为。完整物理/虚拟主机隔离、公开 TLS 接入、正式评分库发布及生产准入仍待验收。报告、有效 unit/drop-in、依赖版本和镜像清单在开发检查点记录。

2026-10-14 升级同一测试机到实际0.1.14候选wheel，在loopback Nginx与私有测试CA下完成24项TLS/执行检查：可信证书链、真实三语言/SQL执行、认证及3MiB代理上限；未信任CA、错误主机名、过期证书均由SSL校验拒绝，重定向未跟随，环境代理未使用。测试仅临时信任该guest venv内的CA，结束恢复原证书库，没有修改macOS或其他应用的信任设置，也没有导出私钥/token。测试机已再次停止。此证据不代表获批公开域名/PKI或生产部署已完成。
