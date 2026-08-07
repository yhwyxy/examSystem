#!/bin/sh
# docker-entrypoint.sh — exam 镜像自愈入口 (生产部署用)
#
# 背景: exam 以非 root (uid 10001) 运行, 但 ./data 与 ./logs 是宿主机 bind mount。
# 宿主目录属主若为 root (scp/tar 解包; 或目录不存在时 docker 自动创建, 属主即
# root), uid 10001 一写入就 EACCES — 典型报错:
#   papers.AtomicWriteJSON tmp open: .../chemical-analysis.json.tmp.tmp: permission denied
#   以及 logs/ 写不了日志、data/exam_runs 写不了 run 快照 (同源, 都是 chown 没做)。
#
# 解法: 入口以 root 启动, 对挂载目录 chown 到 10001 后经 su-exec 降权, exec 成
# exam-server。运行态仍是非 root, 只是把「部署时必须手动 chown」的运维步骤吸收进
# 容器自愈 (postgres 官方镜像同款模式)。任何方式拷贝/解包/自动创建都不再踩属主坑。
#
# 注意: 只读挂载 (worker 的 data/models) 不在此列; 若误把 data/logs 挂成 ro,
# chown 会失败并直接退出 — 宁可启动失败也不要静默以 root 跑服务。

set -e

# 与 compose 的 bind mount 对齐 (/app/data /app/logs); 目录不存在时 docker 自动
# 创建的 bind 源是 root 属主, 这里一并 mkdir + chown 修掉。
for d in /app/data /app/logs; do
    if [ ! -d "$d" ]; then
        mkdir -p "$d"
    fi
    chown -R 10001:10001 "$d"
done

exec su-exec 10001:10001 exam-server "$@"
