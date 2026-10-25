#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
source "$root/scripts/common.sh"

scenario=all
api_base_url=${API_BASE_URL:-http://localhost:18082}
user_base_url=${USER_BASE_URL:-http://localhost:18080}
payment_base_url=${PAYMENT_BASE_URL:-http://localhost:18083}
password=${DEMO_PASSWORD:-}
start_stack=false
without_payment=false

while (( $# > 0 )); do
    case "$1" in
        --scenario|--api-base-url|--user-base-url|--payment-base-url|--password)
            [[ $# -ge 2 && -n "$2" ]] || die "$1 缺少值"
            case "$1" in
                --scenario) scenario=$2 ;;
                --api-base-url) api_base_url=$2 ;;
                --user-base-url) user_base_url=$2 ;;
                --payment-base-url) payment_base_url=$2 ;;
                --password) password=$2 ;;
            esac
            shift 2 ;;
        --start-stack) start_stack=true; shift ;;
        --without-payment) without_payment=true; shift ;;
        -h|--help)
            printf '%s\n' '用法：bash scripts/demo.sh [--scenario all|pay|timeout] [--without-payment] [--start-stack]' \
                '地址参数：--api-base-url URL --user-base-url URL --payment-base-url URL' \
                '密码参数：--password VALUE，也可使用 DEMO_PASSWORD 或仓库 .env'
            exit 0 ;;
        *) die "未知参数：$1" ;;
    esac
done
case "$scenario" in
    all|All) scenario=all ;;
    pay|Pay) scenario=pay ;;
    timeout|Timeout) scenario=timeout ;;
    *) die '--scenario 仅支持 all、pay 或 timeout' ;;
esac
require_commands curl jq od tr date sleep

# 只读取指定配置项，不 source .env，避免把配置文本当作命令执行。
if [[ -z "$password" && -f "$root/.env" ]]; then
    while IFS= read -r line || [[ -n "$line" ]]; do
        line=${line%$'\r'}
        if [[ "$line" == DEMO_PASSWORD=* ]]; then
            password=${line#DEMO_PASSWORD=}
            if [[ "$password" == \"*\" || "$password" == \'*\' ]]; then
                password=${password:1:${#password}-2}
            fi
            break
        fi
    done < "$root/.env"
fi
[[ -n "$password" ]] || die '请通过 --password、DEMO_PASSWORD 或仓库 .env 提供种子账号密码'

# 只有显式指定 --start-stack 才启动容器；不启动引擎、不删除数据卷。
if [[ "$start_stack" == true ]]; then
    require_commands docker
    (
        cd "$root"
        docker compose up -d --build --wait --wait-timeout 180
        docker compose run --rm seed-user
        docker compose run --rm seed-catalog
    )
fi

login() {
    local body response
    body=$(jq -cn --arg username "$1" --arg password "$password" '{username:$username,password:$password}') || return 1
    response=$(api "$user_base_url" /api/auth/login POST '' "$body") || return 1
    jq -er '.accessToken | select(type == "string" and length > 0)' <<<"$response"
}

new_activity() {
    local sku_id=$1 title=$2 body draft activity_id published again
    # jq 的 UTC 时间函数同时适用于 Linux、macOS 和 Git Bash，不依赖 GNU date 扩展。
    body=$(jq -cn --arg sku "$sku_id" --arg title "$title" \
        '{title:$title,skuId:$sku,priceCent:1990,quota:1,startsAt:(now-60|todateiso8601),endsAt:(now+3600|todateiso8601)}') || return 1
    draft=$(api "$api_base_url" /api/admin/activities POST "$operator" "$body" 201) || return 1
    activity_id=$(jq -er '.id' <<<"$draft") || return 1
    published=$(api "$api_base_url" "/api/admin/activities/$activity_id/publish" POST "$operator") || return 1
    again=$(api "$api_base_url" "/api/admin/activities/$activity_id/publish" POST "$operator") || return 1
    assert_json "$published" '.status == "PUBLISHED"' '发布状态异常'
    assert_json "$again" '.status == "PUBLISHED" and .usedQuota == 0' '重复发布改变了状态或额度'
    printf '%s\n' "$published"
}

submit_and_wait() {
    local activity_id=$1 token=$2 request_id body accepted repeated order_no result state created_order deadline
    request_id=$(new_request_id) || return 1
    body=$(jq -cn --arg id "$request_id" '{requestId:$id}') || return 1
    accepted=$(api "$api_base_url" "/api/activities/$activity_id/requests" POST "$token" "$body" 202) || return 1
    repeated=$(api "$api_base_url" "/api/activities/$activity_id/requests" POST "$token" "$body") || return 1
    order_no=$(jq -er '.orderNo' <<<"$accepted") || return 1
    [[ "$order_no" == "$(jq -r '.orderNo' <<<"$repeated")" ]] || die '重复请求改变了稳定订单号'
    deadline=$(( $(date +%s) + 40 ))
    while :; do
        result=$(api "$api_base_url" "/api/requests/$request_id" GET "$token") || return 1
        state=$(jq -r '.status' <<<"$result") || return 1
        [[ "$state" != FAILED ]] || die "成单失败：$(jq -r '.failureReason' <<<"$result")"
        if [[ "$state" == ORDER_CREATED ]]; then
            created_order=$(api "$api_base_url" "/api/orders/$order_no" GET "$token") || return 1
            jq -c --arg id "$request_id" '. + {requestId:$id}' <<<"$created_order"
            return
        fi
        (( $(date +%s) < deadline )) || die "受理未在 40 秒内成单，requestId=$request_id；脚本超时不改变业务状态"
        sleep 1
    done
}

wait_order() {
    local order_no=$1 token=$2 state=$3 inventory=$4 deadline=$5 result
    while :; do
        result=$(api "$api_base_url" "/api/orders/$order_no" GET "$token") || return 1
        if jq -e --arg state "$state" --arg inventory "$inventory" \
            '.status == $state and .inventoryStatus == $inventory' <<<"$result" >/dev/null; then
            printf '%s\n' "$result"
            return
        fi
        (( $(date +%s) < deadline )) || die "订单 $order_no 未收敛到 $state/$inventory"
        sleep 1
    done
}

operator=$(login operator)
user1=$(login user01)
user2=$(login user02)
api "$user_base_url" /api/auth/me GET "$user1" >/dev/null
api "$user_base_url" /api/auth/me GET '' '' 401 >/dev/null
api "$user_base_url" /api/auth/me GET invalid-token '' 401 >/dev/null
api "$api_base_url" /api/admin/activities POST "$user1" '{}' 403 >/dev/null
results='[]'

if [[ "$scenario" == pay || "$scenario" == all ]]; then
    activity=$(new_activity 1 "支付演示-$(new_request_id)")
    activity_id=$(jq -r '.id' <<<"$activity")
    order=$(submit_and_wait "$activity_id" "$user1")
    order_no=$(jq -r '.orderNo' <<<"$order")
    api "$api_base_url" "/api/orders/$order_no" GET "$user2" '' 403 >/dev/null
    api "$api_base_url" "/api/orders/$order_no/payment" POST "$user2" '' 403 >/dev/null
    pay=$(api "$api_base_url" "/api/orders/$order_no/payment" POST "$user1")
    repeated_pay=$(api "$api_base_url" "/api/orders/$order_no/payment" POST "$user1")
    payment_no=$(jq -r '.paymentNo' <<<"$pay")
    [[ "$payment_no" == "$(jq -r '.paymentNo' <<<"$repeated_pay")" ]] || die '支付单创建不幂等'
    api "$payment_base_url" "/api/payments/$payment_no/simulate-success" POST "$user2" '' 403 >/dev/null
    api "$payment_base_url" "/api/payments/$payment_no" GET "$user2" '' 403 >/dev/null
    api "$payment_base_url" "/api/payments/$payment_no/simulate-success" POST "$user1" >/dev/null
    api "$payment_base_url" "/api/payments/$payment_no/simulate-success" POST "$user1" >/dev/null
    settled=$(wait_order "$order_no" "$user1" PAID CONFIRMED "$(( $(date +%s) + 30 ))")
    assert_json "$settled" '.fulfillable == true' '已确认库存的支付订单未标记为可履约'
    replay_body=$(jq -c '{requestId}' <<<"$order")
    replay=$(api "$api_base_url" "/api/activities/$activity_id/requests" POST "$user1" "$replay_body")
    assert_json "$replay" '.status == "ORDER_CREATED"' '成单后重放请求状态不稳定'
    [[ "$(jq -r '.orderNo' <<<"$replay")" == "$order_no" ]] || die '成单后重放改变了订单号'
    api "$api_base_url" /api/activities/999999999/requests POST "$user1" "$replay_body" 409 >/dev/null
    new_body=$(jq -cn --arg id "$(new_request_id)" '{requestId:$id}')
    api "$api_base_url" "/api/activities/$activity_id/requests" POST "$user1" "$new_body" 409 >/dev/null
    result=$(jq -c --arg activity "$activity_id" '{scenario:"Pay",activityId:$activity,orderNo,status,inventoryStatus}' <<<"$settled")
    results=$(jq -cn --argjson results "$results" --argjson result "$result" '$results + [$result]')
fi

if [[ "$scenario" == timeout || "$scenario" == all ]]; then
    activity=$(new_activity 2 "超时演示-$(new_request_id)")
    activity_id=$(jq -r '.id' <<<"$activity")
    order=$(submit_and_wait "$activity_id" "$user2")
    order_no=$(jq -r '.orderNo' <<<"$order")
    payment_no=''
    if [[ "$without_payment" == false ]]; then
        pay=$(api "$api_base_url" "/api/orders/$order_no/payment" POST "$user2")
        payment_no=$(jq -r '.paymentNo' <<<"$pay")
    fi
    # 服务端输出 UTC/Z；jq 解析前移除可选的小数秒，避免依赖平台的日期解析参数。
    deadline=$(jq -r '.expiresAt | sub("\\.[0-9]+Z$";"Z") | fromdateiso8601 + 30' <<<"$order")
    settled=$(wait_order "$order_no" "$user2" CANCELED RELEASED "$deadline")
    if [[ -n "$payment_no" ]]; then
        api "$payment_base_url" "/api/payments/$payment_no/simulate-success" POST "$user2" '' 409 >/dev/null
    fi
    participation=$(api "$api_base_url" "/api/activities/$activity_id/participation" GET "$user2")
    [[ "$(jq -r '.orderNo' <<<"$participation")" == "$order_no" ]] || die '超时关闭后活动资格被错误恢复'
    new_body=$(jq -cn --arg id "$(new_request_id)" '{requestId:$id}')
    api "$api_base_url" "/api/activities/$activity_id/requests" POST "$user2" "$new_body" 409 >/dev/null
    result=$(jq -c --arg activity "$activity_id" '{scenario:"Timeout",activityId:$activity,orderNo,status,inventoryStatus}' <<<"$settled")
    results=$(jq -cn --argjson results "$results" --argjson result "$result" '$results + [$result]')
fi

jq . <<<"$results"
printf '接口演示完成。请在业务静止窗口运行 diagnose，独立核对数据库事实。\n' >&2
