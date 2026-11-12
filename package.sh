#!/usr/bin/env bash
# 打包插件商店需要的 release 资产。
#
# 产出:
#   commandcode-quota_<ver>_linux_amd64.zip   (zip 根目录直接是 .so,不能套子目录)
#   checksums.txt                              (sha256, 商店安装时校验)
set -euo pipefail

VERSION="${PLUGIN_VERSION:-1.0.0}"
ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT"

SO="dist/commandcode-quota.so"
[ -f "$SO" ] || { echo "缺少 $SO,先跑 build.sh"; exit 1; }

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

# 商店要求 zip 内文件名与插件 ID 对应
cp "$SO" "$STAGE/commandcode-quota.so"
ZIP="dist/commandcode-quota_${VERSION}_linux_amd64.zip"
rm -f "$ZIP"
( cd "$STAGE" && zip -q -9 "$ROOT/$ZIP" commandcode-quota.so )

rm -f dist/checksums.txt
( cd dist && sha256sum "$(basename "$ZIP")" > checksums.txt )

echo "==> 产物"
ls -lh dist/*.zip dist/checksums.txt
echo "==> 内容"
unzip -l "$ZIP"
echo "==> checksums"
cat dist/checksums.txt
