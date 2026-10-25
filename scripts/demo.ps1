#requires -Version 7.0
param(
    [ValidateSet('Pay', 'Timeout', 'All')][string]$Scenario = 'All',
    [string]$ApiBaseUrl = $(if ($env:API_BASE_URL) { $env:API_BASE_URL } else { 'http://localhost:18082' }),
    [string]$UserBaseUrl = $(if ($env:USER_BASE_URL) { $env:USER_BASE_URL } else { 'http://localhost:18080' }),
    [string]$PaymentBaseUrl = $(if ($env:PAYMENT_BASE_URL) { $env:PAYMENT_BASE_URL } else { 'http://localhost:18083' }),
    [string]$Password = $env:DEMO_PASSWORD,
    [switch]$StartStack,
    [switch]$WithoutPayment
)

$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent

if (-not $Password) {
    $configPath = Join-Path $root '.env'
    if (Test-Path $configPath) {
        $line = Get-Content $configPath | Where-Object { $_ -match '^DEMO_PASSWORD=' } | Select-Object -First 1
        if ($line) { $Password = $line.Substring('DEMO_PASSWORD='.Length).Trim().Trim('"').Trim("'") }
    }
}
if (-not $Password) { throw '请通过 -Password、DEMO_PASSWORD 或仓库 .env 提供种子账号密码' }

# 可选启动已有 Docker 引擎中的容器；脚本不会启动 Docker Desktop，也不会删除卷。
if ($StartStack) {
    Push-Location $root
    try {
        docker compose up -d --build --wait --wait-timeout 180
        if ($LASTEXITCODE -ne 0) { throw '容器启动未就绪' }
        docker compose run --rm seed-user
        if ($LASTEXITCODE -ne 0) { throw '用户初始化失败' }
        docker compose run --rm seed-catalog
        if ($LASTEXITCODE -ne 0) { throw '商品初始化失败' }
    } finally { Pop-Location }
}

function Invoke-Api {
    param([string]$Base, [string]$Path, [string]$Method = 'GET', [string]$Token, $Body, [int]$ExpectedStatus = 200)
    $headers = @{}
    if ($Token) { $headers.Authorization = "Bearer $Token" }
    $parameters = @{
        Uri = $Base.TrimEnd('/') + $Path
        Method = $Method
        Headers = $headers
        TimeoutSec = 10
        SkipHttpErrorCheck = $true
    }
    if ($null -ne $Body) { $parameters.Body = $Body | ConvertTo-Json -Depth 10; $parameters.ContentType = 'application/json; charset=utf-8' }
    $response = Invoke-WebRequest @parameters
    $json = $response.Content | ConvertFrom-Json
    if ([int]$response.StatusCode -ne $ExpectedStatus) { throw "$Method $Path 预期 $ExpectedStatus，实际 $($response.StatusCode)：$($json.message)" }
    if (-not $json.traceId) { throw "$Path 缺少 traceId" }
    if ($ExpectedStatus -lt 400 -and $json.code -ne 'OK') { throw "$Path 业务响应失败：$($json.message)" }
    return $json.data
}

function Login([string]$Username) {
    $login = Invoke-Api -Base $UserBaseUrl -Path '/api/auth/login' -Method POST -Body @{ username = $Username; password = $Password }
    return $login.accessToken
}

function New-Activity([string]$SkuId, [string]$Title) {
    $now = [DateTimeOffset]::UtcNow
    $body = @{ title = $Title; skuId = $SkuId; priceCent = 1990; quota = 1; startsAt = $now.AddMinutes(-1).ToString('o'); endsAt = $now.AddHours(1).ToString('o') }
    $draft = Invoke-Api -Base $ApiBaseUrl -Path '/api/admin/activities' -Method POST -Token $operator -Body $body -ExpectedStatus 201
    $published = Invoke-Api -Base $ApiBaseUrl -Path "/api/admin/activities/$($draft.id)/publish" -Method POST -Token $operator
    $again = Invoke-Api -Base $ApiBaseUrl -Path "/api/admin/activities/$($draft.id)/publish" -Method POST -Token $operator
    if ($published.status -ne 'PUBLISHED' -or $again.usedQuota -ne 0) { throw '发布状态或重复发布额度异常' }
    return $published
}

function Submit-And-Wait($Activity, [string]$Token) {
    $requestId = [guid]::NewGuid().ToString()
    $accepted = Invoke-Api -Base $ApiBaseUrl -Path "/api/activities/$($Activity.id)/requests" -Method POST -Token $Token -Body @{requestId=$requestId} -ExpectedStatus 202
    $repeat = Invoke-Api -Base $ApiBaseUrl -Path "/api/activities/$($Activity.id)/requests" -Method POST -Token $Token -Body @{requestId=$requestId}
    if ($accepted.orderNo -ne $repeat.orderNo) { throw '重复请求改变了稳定订单号' }
    $deadline = [DateTimeOffset]::UtcNow.AddSeconds(40)
    do {
        $result = Invoke-Api -Base $ApiBaseUrl -Path "/api/requests/$requestId" -Token $Token
        if ($result.status -eq 'FAILED') { throw "成单失败：$($result.failureReason)" }
        if ($result.status -eq 'ORDER_CREATED') {
            $createdOrder = Invoke-Api -Base $ApiBaseUrl -Path "/api/orders/$($result.orderNo)" -Token $Token
            $createdOrder | Add-Member -NotePropertyName requestId -NotePropertyValue $requestId
            return $createdOrder
        }
        Start-Sleep -Seconds 1
    } while ([DateTimeOffset]::UtcNow -lt $deadline)
    throw "受理未在 40 秒内成单，requestId=$requestId；脚本超时不会改变业务状态"
}

function Wait-Order([string]$OrderNo, [string]$Token, [string]$State, [string]$Inventory, [DateTimeOffset]$Deadline) {
    do {
        $order = Invoke-Api -Base $ApiBaseUrl -Path "/api/orders/$OrderNo" -Token $Token
        if ($order.status -eq $State -and $order.inventoryStatus -eq $Inventory) { return $order }
        Start-Sleep -Seconds 1
    } while ([DateTimeOffset]::UtcNow -lt $Deadline)
    throw "订单 $OrderNo 未收敛到 $State/$Inventory，实际 $($order.status)/$($order.inventoryStatus)"
}

$operator = Login 'operator'
$user1 = Login 'user01'
$user2 = Login 'user02'
$null = Invoke-Api -Base $UserBaseUrl -Path '/api/auth/me' -Token $user1
$null = Invoke-Api -Base $UserBaseUrl -Path '/api/auth/me' -ExpectedStatus 401
$null = Invoke-Api -Base $UserBaseUrl -Path '/api/auth/me' -Token 'invalid-token' -ExpectedStatus 401
$null = Invoke-Api -Base $ApiBaseUrl -Path '/api/admin/activities' -Method POST -Token $user1 -Body @{} -ExpectedStatus 403
$results = @()

if ($Scenario -in @('Pay', 'All')) {
    $activity = New-Activity '1' "支付演示-$([guid]::NewGuid().ToString('N').Substring(0,8))"
    $order = Submit-And-Wait $activity $user1
    $null = Invoke-Api -Base $ApiBaseUrl -Path "/api/orders/$($order.orderNo)" -Token $user2 -ExpectedStatus 403
    $null = Invoke-Api -Base $ApiBaseUrl -Path "/api/orders/$($order.orderNo)/payment" -Method POST -Token $user2 -ExpectedStatus 403
    $pay = Invoke-Api -Base $ApiBaseUrl -Path "/api/orders/$($order.orderNo)/payment" -Method POST -Token $user1
    $repeatPay = Invoke-Api -Base $ApiBaseUrl -Path "/api/orders/$($order.orderNo)/payment" -Method POST -Token $user1
    if ($pay.paymentNo -ne $repeatPay.paymentNo) { throw '支付单创建不幂等' }
    $null = Invoke-Api -Base $PaymentBaseUrl -Path "/api/payments/$($pay.paymentNo)/simulate-success" -Method POST -Token $user2 -ExpectedStatus 403
    $null = Invoke-Api -Base $PaymentBaseUrl -Path "/api/payments/$($pay.paymentNo)" -Token $user2 -ExpectedStatus 403
    $null = Invoke-Api -Base $PaymentBaseUrl -Path "/api/payments/$($pay.paymentNo)/simulate-success" -Method POST -Token $user1
    $null = Invoke-Api -Base $PaymentBaseUrl -Path "/api/payments/$($pay.paymentNo)/simulate-success" -Method POST -Token $user1
    $settled = Wait-Order $order.orderNo $user1 'PAID' 'CONFIRMED' ([DateTimeOffset]::UtcNow.AddSeconds(30))
    if (-not $settled.fulfillable) { throw '已确认库存的支付订单未标记为可履约' }
    $replay = Invoke-Api -Base $ApiBaseUrl -Path "/api/activities/$($activity.id)/requests" -Method POST -Token $user1 -Body @{requestId=$order.requestId}
    if ($replay.orderNo -ne $order.orderNo -or $replay.status -ne 'ORDER_CREATED') { throw '成单后重放请求结果不稳定' }
    $null = Invoke-Api -Base $ApiBaseUrl -Path '/api/activities/999999999/requests' -Method POST -Token $user1 -Body @{requestId=$order.requestId} -ExpectedStatus 409
    $null = Invoke-Api -Base $ApiBaseUrl -Path "/api/activities/$($activity.id)/requests" -Method POST -Token $user1 -Body @{requestId=[guid]::NewGuid().ToString()} -ExpectedStatus 409
    $results += @{ scenario='Pay'; activityId=$activity.id; orderNo=$order.orderNo; status=$settled.status; inventoryStatus=$settled.inventoryStatus }
}

if ($Scenario -in @('Timeout', 'All')) {
    $activity = New-Activity '2' "超时演示-$([guid]::NewGuid().ToString('N').Substring(0,8))"
    $order = Submit-And-Wait $activity $user2
    $pay = $null
    if (-not $WithoutPayment) { $pay = Invoke-Api -Base $ApiBaseUrl -Path "/api/orders/$($order.orderNo)/payment" -Method POST -Token $user2 }
    $settled = Wait-Order $order.orderNo $user2 'CANCELED' 'RELEASED' ([DateTimeOffset]::Parse($order.expiresAt).AddSeconds(30))
    if ($pay) { $null = Invoke-Api -Base $PaymentBaseUrl -Path "/api/payments/$($pay.paymentNo)/simulate-success" -Method POST -Token $user2 -ExpectedStatus 409 }
    $participation = Invoke-Api -Base $ApiBaseUrl -Path "/api/activities/$($activity.id)/participation" -Token $user2
    if ($participation.orderNo -ne $order.orderNo) { throw '超时关闭后活动资格被错误恢复' }
    $null = Invoke-Api -Base $ApiBaseUrl -Path "/api/activities/$($activity.id)/requests" -Method POST -Token $user2 -Body @{requestId=[guid]::NewGuid().ToString()} -ExpectedStatus 409
    $results += @{ scenario='Timeout'; activityId=$activity.id; orderNo=$order.orderNo; status=$settled.status; inventoryStatus=$settled.inventoryStatus }
}

$results | ConvertTo-Json -Depth 5
Write-Host '接口演示完成。请在业务静止窗口运行 diagnose，独立核对数据库事实。'
