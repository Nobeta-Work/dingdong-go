param(
    [string]$ApiBaseUrl = 'http://localhost:18082',
    [switch]$WithMQ
)

$ErrorActionPreference = 'Stop'

# ApiBaseUrl 指向交易服务；其他端口可用环境变量覆盖，便于 IDE 调试。
$targets = @{
    'trade-service' = $ApiBaseUrl
    'user-service' = $(if ($env:USER_BASE_URL) { $env:USER_BASE_URL } else { 'http://localhost:18080' })
    'catalog-service' = $(if ($env:CATALOG_BASE_URL) { $env:CATALOG_BASE_URL } else { 'http://localhost:18081' })
    'payment-service' = $(if ($env:PAYMENT_BASE_URL) { $env:PAYMENT_BASE_URL } else { 'http://localhost:18083' })
}
if ($env:API_BASE_URL -and -not $PSBoundParameters.ContainsKey('ApiBaseUrl')) {
    $targets['trade-service'] = $env:API_BASE_URL
}

foreach ($entry in $targets.GetEnumerator()) {
    foreach ($path in @('/livez', '/readyz')) {
        $response = Invoke-RestMethod -Uri ($entry.Value.TrimEnd('/') + $path) -TimeoutSec 5
        if ($response.code -ne 'OK' -or -not $response.traceId) {
            throw "$($entry.Key) $path 响应不符合统一协议"
        }
        if ($path -eq '/readyz' -and $response.data.service -ne $entry.Key) {
            throw "$($entry.Key) 端口与服务不对应"
        }
        Write-Host "$($entry.Key) $path 通过 traceId=$($response.traceId)"
    }
}

if ($WithMQ) {
    Push-Location (Split-Path $PSScriptRoot -Parent)
    try {
        docker compose run --rm mqcheck
        if ($LASTEXITCODE -ne 0) { throw 'MQ 生产、消费、重投检查失败' }
    } finally { Pop-Location }
}

Write-Host '框架检查通过；此脚本不验收抢购、订单、支付业务。'
