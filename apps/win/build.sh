#!/bin/bash
# 打 Windows 版的包：apps/win/out/P2P直传-windows-amd64-<版本>.zip，里面只有 P2P直传.exe 和使用说明。
#   朋友解压后双击 exe：自己装进 %LOCALAPPDATA%\Programs\P2P直传\、建开机自启 / 开始菜单 /「发送到」、打开网页界面
#   （lab/p2pcp/desktop_windows.go）。纯 Go 交叉编译，本机不用装别的。
#
#   ./build.sh            会合服务器取 ~/.config/p2pcp/server（和 apps/pc/install.sh、手机 App 一样），写进 exe
#   P2P_SERVER=IP:7001 ./build.sh
set -euo pipefail
cd "$(dirname "$0")"
SRV=${P2P_SERVER:-$(cat ~/.config/p2pcp/server 2>/dev/null || true)}
[ -n "$SRV" ] || { echo "没有会合服务器：P2P_SERVER=IP:7001 ./build.sh，或者先跑 apps/pc/install.sh"; exit 1; }
VER=$(sed -n 's/^const version = "\(.*\)"/\1/p' ../../lab/p2pcp/main.go)
OUT=out
STAGE=$OUT/P2P直传
rm -rf "$STAGE"
mkdir -p "$STAGE"

# -H windowsgui：双击不弹黑框（日志在 %LOCALAPPDATA%\p2pcp\log）
(cd ../../lab && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
	-ldflags "-s -w -H windowsgui -X main.defaultServer=$SRV" -o "../apps/win/$STAGE/P2P直传.exe" ./p2pcp)
sed 's/$/\r/' 使用说明.txt > "$STAGE/使用说明.txt"

ZIP=$OUT/P2P直传-windows-amd64-$VER.zip
rm -f "$ZIP"
(cd "$OUT" && python3 - "P2P直传-windows-amd64-$VER.zip" <<'PY'
# zip 里的中文文件名要打上 UTF-8 标志，Windows 资源管理器才不乱码（Python 的 zipfile 遇到非 ASCII 名字会自动打）
import sys, zipfile, os
with zipfile.ZipFile(sys.argv[1], 'w', zipfile.ZIP_DEFLATED) as z:
    for root, _, files in os.walk('P2P直传'):
        for f in files:
            z.write(os.path.join(root, f))
PY
)
ls -la "$ZIP"
sha256sum "$ZIP"
