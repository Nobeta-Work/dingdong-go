#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
source "$root/scripts/common.sh"

api_base_url=${API_BASE_URL:-http://localhost:18082}
with_mq=false
while (( $# > 0 )); do
    case "$1" in
        --api-base-url) [[ $# -ge 2 && -n "$2" ]] || die '--api-base-url 缺少值'; api_base_url=$2; shift 2 ;;
        --with-mq) with_mq=true; shift ;;
        -h|--help) printf '用法：bash scripts/check.sh [--api-base-url URL] [--with-mq]\n'; exit 0 ;;
        *) die "未知参数：$1" ;;
    esac
done
require_commands curl jq

services=(trade-service user-service catalog-service payment-service)
urls=("$api_base_url" "${USER_BASE_URL:-http://localhost:18080}" "${CATALOG_BASE_URL:-http://localhost:18081}" "${PAYMENT_BASE_URL:-http://localhost:18083}")
for index in "${!services[@]}"; do
    for path in /livez /readyz; do
        data=$(api "${urls[$index]}" "$path")
        if [[ "$path" == /readyz ]]; then
            actual=$(jq -r '.service' <<<"$data")
            [[ "$actual" == "${services[$index]}" ]] || die "${services[$index]} 端口与服务不对应"
        fi
        printf '%s %s 通过\n' "${services[$index]}" "$path"
    done
done

if [[ "$with_mq" == true ]]; then
    require_commands docker
    (cd "$root" && docker compose run --rm mqcheck)
fi
printf '框架检查通过；此脚本不验收抢购、订单、支付业务。\n'
