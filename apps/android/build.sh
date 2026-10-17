#!/bin/bash
# 不用 Gradle / Android Studio，直接用 build-tools 打 APK：
#   Go 交叉编译 p2pcp → lib/arm64-v8a/libp2pcp.so
#   aapt2 编译资源 + 链接清单 → javac → d8 → 塞进 dex 和 so → zipalign → apksigner
#
#   ./build.sh            构建 out/p2p-direct.apk
#   ./build.sh install    构建并 adb install
#
# 依赖：/opt/android-sdk/build-tools（pacman: android-sdk-build-tools）、JDK、Go、ImageMagick；
# android.jar 第一次运行时下载到 ~/.cache/uu-remote-study/android-35/。
# 房间密钥取 ~/.config/p2pcp/room（电脑端 install.sh 生成），会写进 APK 的 assets。
set -euo pipefail
cd "$(dirname "$0")"
BT=${BT:-/opt/android-sdk/build-tools/37.0.0}
CACHE=~/.cache/uu-remote-study
JAR=$CACHE/android-35/android.jar
OUT=out
ROOM_FILE=~/.config/p2pcp/room
SERVER=${SERVER:-8.140.18.211:7001}

if [ ! -f "$JAR" ]; then
	mkdir -p "$CACHE"
	curl -sSfL -o "$CACHE/platform-35.zip" https://dl.google.com/android/repository/platform-35_r02.zip
	(cd "$CACHE" && unzip -q -o platform-35.zip)
fi
[ -f "$ROOM_FILE" ] || { echo "缺少 $ROOM_FILE，先运行 apps/pc/install.sh"; exit 1; }

rm -rf "$OUT" && mkdir -p "$OUT"/{res,classes,stage/lib/arm64-v8a,assets}

# 1. Go 核心：Android 上的 Go 静态可执行文件，伪装成 .so 才能被安装器解压到可执行目录
(cd ../../lab && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$OLDPWD/$OUT/stage/lib/arm64-v8a/libp2pcp.so" ./p2pcp)

# 2. 资源：图标（Tokyo Night 底 + P2P 字样）
mkdir -p "$OUT/resin/mipmap-xxxhdpi"
magick -size 192x192 xc:none -fill '#1a1b26' -draw 'roundrectangle 0,0 191,191 40,40' \
	-fill '#7aa2f7' -font DejaVu-Sans-Bold -pointsize 58 -gravity center -annotate +0-14 'P2P' \
	-fill '#9ece6a' -pointsize 34 -annotate +0+44 '⇄' "$OUT/resin/mipmap-xxxhdpi/ic_launcher.png"
"$BT/aapt2" compile --dir "$OUT/resin" -o "$OUT/res/res.zip"
printf 'room=%s\nserver=%s\n' "$(cat "$ROOM_FILE")" "$SERVER" > "$OUT/assets/config.properties"
"$BT/aapt2" link -o "$OUT/base.apk" -I "$JAR" --manifest AndroidManifest.xml \
	--min-sdk-version 29 --target-sdk-version 34 --version-code "$(date +%s)" --version-name 0.1 \
	-A "$OUT/assets" "$OUT/res/res.zip"

# 3. Java → dex
javac --release 17 -nowarn -cp "$JAR" -d "$OUT/classes" $(find src -name '*.java')
"$BT/d8" --lib "$JAR" --min-api 29 --output "$OUT/stage" $(find "$OUT/classes" -name '*.class')

# 4. 拼包、对齐、签名
python3 - "$OUT" <<'EOF'
import sys, zipfile, os
out = sys.argv[1]
with zipfile.ZipFile(f"{out}/base.apk", "a", zipfile.ZIP_DEFLATED) as z:
    z.write(f"{out}/stage/classes.dex", "classes.dex")
    z.write(f"{out}/stage/lib/arm64-v8a/libp2pcp.so", "lib/arm64-v8a/libp2pcp.so")
EOF
"$BT/zipalign" -f -p 4 "$OUT/base.apk" "$OUT/aligned.apk"
KS=$CACHE/debug.keystore
[ -f "$KS" ] || keytool -genkeypair -keystore "$KS" -storepass android -keypass android -alias debug \
	-keyalg RSA -keysize 2048 -validity 10000 -dname "CN=uulab debug" >/dev/null 2>&1
"$BT/apksigner" sign --ks "$KS" --ks-pass pass:android --out "$OUT/p2p-direct.apk" "$OUT/aligned.apk"
ls -la "$OUT/p2p-direct.apk"

if [ "${1:-}" = install ]; then
	adb install -r "$OUT/p2p-direct.apk"
fi
