#!/usr/bin/env bash
# 公共函数只定义行为，source 时不会访问服务、启动容器或读取配置。

die() {
    printf '错误：%s\n' "$*" >&2
    exit 1
}

require_commands() {
    local command_name
    for command_name in "$@"; do
        command -v "$command_name" >/dev/null 2>&1 || die "缺少命令：$command_name"
    done
}

# api 只把 data 写到标准输出，错误写到标准错误，便于调用方用命令替换接收 JSON。
# curl 不启用自动重试，防止写请求在结果未知时被脚本擅自重放。
api() {
    local base=$1 path=$2 method=${3:-GET} token=${4:-} body=${5:-} expected=${6:-200}
    local response http_status json
    local args=(--silent --show-error --max-time 10 --request "$method")
    if [[ -n "$token" ]]; then
        args+=(--header "Authorization: Bearer $token")
    fi
    if [[ -n "$body" ]]; then
        args+=(--header 'Content-Type: application/json; charset=utf-8' --data-raw "$body")
    fi
    response=$(curl "${args[@]}" --write-out $'\n%{http_code}' "${base%/}$path") || return 1
    http_status=${response##*$'\n'}
    json=${response%$'\n'*}
    if [[ "$http_status" != "$expected" ]]; then
        printf '%s %s 预期 HTTP %s，实际 %s\n' "$method" "$path" "$expected" "$http_status" >&2
        return 1
    fi
    # 不输出登录响应，避免错误路径将令牌等敏感字段写入日志。
    if ! jq -e '(.traceId | type == "string" and length > 0)' <<<"$json" >/dev/null; then
        printf '%s 响应缺少合法 traceId\n' "$path" >&2
        return 1
    fi
    if (( expected < 400 )) && ! jq -e '.code == "OK"' <<<"$json" >/dev/null; then
        printf '%s 业务响应失败\n' "$path" >&2
        return 1
    fi
    jq -c '.data' <<<"$json"
}

assert_json() {
    local json=$1 expression=$2 message=$3
    jq -e "$expression" <<<"$json" >/dev/null || die "$message"
}

new_request_id() {
    # requestId 允许短横线和十六进制；读取系统随机源，不依赖额外的 UUID 工具。
    local random
    random=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n') || return 1
    printf 'demo-%s\n' "$random"
}
