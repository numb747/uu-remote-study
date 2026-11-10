#!/bin/bash
# 安装 P2P 直传电脑端：
#   ~/.local/bin/p2pcp、~/.local/bin/p2p-direct
#   ~/.config/p2pcp/room              房间密钥（第一次生成；手机 App 构建时读它）
#   ~/.config/p2pcp/server            会合服务器地址（第一次安装时由 P2P_SERVER 给出；手机 App 构建时也读它）
#   systemd 用户服务 p2p-direct       常驻接收
#   启动器「发送到手机（P2P 直传）」、Dolphin 右键「P2P 直传到手机」、图标 p2p-direct
#
#   P2P_SERVER=1.2.3.4:7001 ./install.sh   第一次安装（服务器端跑 punch server -listen :7001 -relay :3478）
#   ./install.sh                           更新
#   ./install.sh uninstall                 卸载（保留房间密钥、服务器地址和已收到的文件）
set -euo pipefail
cd "$(dirname "$0")"
APPS=~/.local/share/applications
MENUS=~/.local/share/kio/servicemenus
ICON=~/.local/share/icons/hicolor/192x192/apps/p2p-direct.png
UNIT=~/.config/systemd/user/p2p-direct.service

if [ "${1:-}" = uninstall ]; then
	systemctl --user disable --now p2p-direct.service 2>/dev/null || true
	rm -f ~/.local/bin/p2pcp ~/.local/bin/p2p-direct "$UNIT" "$APPS/p2p-direct-send.desktop" "$MENUS/p2p-direct.desktop" "$ICON"
	systemctl --user daemon-reload
	echo "已卸载"
	exit 0
fi

mkdir -p ~/.local/bin ~/.config/p2pcp "$APPS" "$MENUS" "$(dirname "$ICON")" "$(dirname "$UNIT")"
[ -f ~/.config/p2pcp/room ] || head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n' > ~/.config/p2pcp/room
chmod 600 ~/.config/p2pcp/room
[ -n "${P2P_SERVER:-}" ] && echo "$P2P_SERVER" > ~/.config/p2pcp/server
[ -s ~/.config/p2pcp/server ] || { echo "第一次安装要给出会合服务器：P2P_SERVER=IP:7001 ./install.sh"; exit 1; }

(cd ../../lab && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o ~/.local/bin/p2pcp ./p2pcp)
install -m 755 p2p-direct ~/.local/bin/p2p-direct

# 图标和手机 App 用同一个
magick -size 192x192 xc:none -fill '#1a1b26' -draw 'roundrectangle 0,0 191,191 40,40' \
	-fill '#7aa2f7' -font DejaVu-Sans-Bold -pointsize 58 -gravity center -annotate +0-14 'P2P' \
	-fill '#9ece6a' -pointsize 34 -annotate +0+44 '⇄' "$ICON"
gtk-update-icon-cache -q ~/.local/share/icons/hicolor 2>/dev/null || true

cat > "$UNIT" <<EOF
[Unit]
Description=P2P 直传：常驻接收手机发来的文件
After=graphical-session.target

[Service]
ExecStart=%h/.local/bin/p2p-direct daemon
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
EOF

cat > "$APPS/p2p-direct-send.desktop" <<EOF
[Desktop Entry]
Type=Application
Name=发送到手机（P2P 直传）
Comment=打洞直连，手机在 5G / 外面也能收；手机要开着 P2P 直传 App
Icon=p2p-direct
Exec=$HOME/.local/bin/p2p-direct send
Terminal=false
Categories=Network;FileTransfer;
Keywords=p2p;phone;send;手机;传;
EOF

# Dolphin 的 PATH 里没有 ~/.local/bin，Exec 用绝对路径；KF6 要求用户目录下的服务菜单可执行
cat > "$MENUS/p2p-direct.desktop" <<EOF
[Desktop Entry]
Type=Service
MimeType=application/octet-stream;
Actions=p2pSend;
X-KDE-Priority=TopLevel

[Desktop Action p2pSend]
Name=P2P 直传到手机
Icon=p2p-direct
Exec=$HOME/.local/bin/p2p-direct send %F
EOF
chmod +x "$MENUS/p2p-direct.desktop"

systemctl --user daemon-reload
systemctl --user enable p2p-direct.service >/dev/null 2>&1
systemctl --user restart p2p-direct.service
sleep 1
systemctl --user is-active p2p-direct.service
echo "已安装。接收目录 ~/Downloads/P2P直传"
