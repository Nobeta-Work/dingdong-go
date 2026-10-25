#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
# 只调用验收客户端；不启动引擎、容器、迁移或服务。帮助模式不会访问网络。
if [[ ${1:-} == --help || ${1:-} == -h ]]; then
    printf '%s\n' '用法：bash scripts/acceptance.sh -mode cache|concurrency|rate|faults|windows|all [参数]' \
        '先由使用者准备双交易实例与独立演示环境，详见 M2.md。' \
        '密码：DEMO_PASSWORD；faults/windows/all 需显式 -allow-crash。' \
        '地址：-trade-url URL -trade-peer-url URL -user-url URL -catalog-url URL -payment-url URL' \
        '报告：-report-dir DIR（默认 .cache/reports），只有实际执行才生成报告。'
    exit 0
fi
command -v go >/dev/null 2>&1 || { printf '缺少 Go\n' >&2; exit 1; }
go run ./cmd/verify "$@"
