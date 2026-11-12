#!/usr/bin/env bash
# 构建 CommandCode Quota 插件动态库。
#
# 产物: dist/commandcode-quota.so
# 要求: Go >= 1.26.11、gcc、glibc 工具链(Alpine/musl 编译的 .so 无法被 Debian 镜像 dlopen)
set -euo pipefail

VERSION="${PLUGIN_VERSION:-1.0.0}"
ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT"

export CGO_ENABLED=1
GOOS="${GOOS:-linux}"
GOARCH="${GOARCH:-amd64}"

echo "==> go vet"
go vet ./...

echo "==> go test"
go test ./internal/quota/

echo "==> build ${GOOS}/${GOARCH} v${VERSION}"
mkdir -p dist
CGO_ENABLED=1 GOOS="$GOOS" GOARCH="$GOARCH" \
  go build -buildmode=c-shared -trimpath \
  -ldflags "-s -w -X main.pluginVersion=${VERSION}" \
  -o dist/commandcode-quota.so ./cmd/commandcode-quota/

rm -f dist/commandcode-quota.h

echo "==> 产物"
ls -lh dist/commandcode-quota.so
echo "==> 导出符号"
nm -D --defined-only dist/commandcode-quota.so | grep -i cliproxy || true
