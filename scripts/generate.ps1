param([string]$Protoc = 'protoc')
$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
Push-Location $root
$previousGoBin = $env:GOBIN
$previousPath = $env:PATH
try {
    $version = & $Protoc --version
    if ($LASTEXITCODE -ne 0 -or $version -ne 'libprotoc 29.3') { throw '请安装 protoc 29.3，或用 -Protoc 指定路径' }
    # 将固定版本插件安装到仓库 bin，不覆盖用户全局工具；生成代码应提交版本控制。
    $env:GOBIN = Join-Path $root 'bin'
    New-Item -ItemType Directory -Force $env:GOBIN | Out-Null
    go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.5
    if ($LASTEXITCODE -ne 0) { throw '安装 protoc-gen-go 失败' }
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
    if ($LASTEXITCODE -ne 0) { throw '安装 protoc-gen-go-grpc 失败' }
    $env:PATH = "$env:GOBIN;$previousPath"
    & $Protoc -I api/proto --go_out=. --go_opt=module=github.com/Nobeta-Work/dingdong-go `
        --go-grpc_out=. --go-grpc_opt=module=github.com/Nobeta-Work/dingdong-go `
        api/proto/catalog/v1/catalog.proto api/proto/payment/v1/payment.proto
    if ($LASTEXITCODE -ne 0) { throw '生成 Protobuf 代码失败' }
} finally {
    $env:GOBIN = $previousGoBin
    $env:PATH = $previousPath
    Pop-Location
}
