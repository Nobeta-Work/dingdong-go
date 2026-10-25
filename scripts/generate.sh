#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
source "$root/scripts/common.sh"
protoc=${PROTOC:-protoc}
while (( $# > 0 )); do
    case "$1" in
        --protoc) [[ $# -ge 2 && -n "$2" ]] || die '--protoc 缺少路径'; protoc=$2; shift 2 ;;
        -h|--help) printf '用法：bash scripts/generate.sh [--protoc /path/to/protoc]\n'; exit 0 ;;
        *) die "未知参数：$1" ;;
    esac
done
require_commands go "$protoc"
version=$("$protoc" --version)
version=${version%$'\r'}
[[ "$version" == 'libprotoc 29.3' ]] || die '请使用 protoc 29.3，或用 --protoc 指定路径'

mkdir -p "$root/bin"
# 插件只安装到仓库 bin；Git Bash 下需将 GOBIN 转成 Windows 原生绝对路径供 Go 使用。
export GOBIN="$root/bin"
if command -v cygpath >/dev/null 2>&1; then
    GOBIN=$(cygpath -w "$root/bin")
fi
export PATH="$root/bin:$PATH"
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.5
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
cd "$root"
"$protoc" -I api/proto --go_out=. --go_opt=module=github.com/Nobeta-Work/dingdong-go \
    --go-grpc_out=. --go-grpc_opt=module=github.com/Nobeta-Work/dingdong-go \
    api/proto/catalog/v1/catalog.proto api/proto/payment/v1/payment.proto
