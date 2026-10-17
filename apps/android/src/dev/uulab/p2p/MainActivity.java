package dev.uulab.p2p;

import android.app.Activity;
import android.content.ContentValues;
import android.content.Intent;
import android.database.Cursor;
import android.graphics.Color;
import android.graphics.Typeface;
import android.graphics.drawable.GradientDrawable;
import android.net.ConnectivityManager;
import android.net.LinkAddress;
import android.net.LinkProperties;
import android.net.Network;
import android.net.Uri;
import android.os.Bundle;
import android.os.Environment;
import android.os.Handler;
import android.os.Looper;
import android.provider.MediaStore;
import android.provider.OpenableColumns;
import android.view.Gravity;
import android.view.View;
import android.view.WindowManager;
import android.widget.Button;
import android.widget.LinearLayout;
import android.widget.ProgressBar;
import android.widget.ScrollView;
import android.widget.TextView;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.net.Inet4Address;
import java.net.Inet6Address;
import java.net.InetAddress;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Properties;

/**
 * P2P 直传：界面只是外壳，真正干活的是打包在 APK 里的 Go 程序 p2pcp（lib/arm64-v8a/libp2pcp.so）。
 * 界面启动它、解析它 stdout 上的 @STATE/@PROGRESS/@DONE/@ERROR 行来显示进度。
 *
 *   发到电脑：选文件 → 复制到缓存目录 → p2pcp send -room <密钥>.pc
 *   来自电脑：App 开着时常驻 p2pcp daemon -room <密钥>.phone，收完挪进「下载/P2P直传」
 */
public class MainActivity extends Activity {
    // Tokyo Night
    static final int BG = 0xff1a1b26, CARD = 0xff24283b, FG = 0xffc0caf5, DIM = 0xff565f89,
            BLUE = 0xff7aa2f7, GREEN = 0xff9ece6a, RED = 0xfff7768e;

    final Handler ui = new Handler(Looper.getMainLooper());
    String room, server;
    Panel sendPanel, recvPanel;
    TextView logView;
    volatile Process daemon;
    ConnectivityManager.NetworkCallback netWatch;
    volatile boolean alive = true;

    /** 一块显示区：标题、状态、进度条、详情。 */
    class Panel {
        TextView state, detail;
        ProgressBar bar;
        boolean justDone; // 刚显示完成：daemon 紧接着报的「等待对方发送」不要盖掉它

        Panel(LinearLayout parent, String title) {
            LinearLayout card = new LinearLayout(MainActivity.this);
            card.setOrientation(LinearLayout.VERTICAL);
            card.setPadding(dp(18), dp(14), dp(18), dp(14));
            GradientDrawable bg = new GradientDrawable();
            bg.setColor(CARD);
            bg.setCornerRadius(dp(14));
            card.setBackground(bg);
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(-1, -2);
            lp.topMargin = dp(14);
            parent.addView(card, lp);

            card.addView(text(title, 13, DIM, true));
            state = text("—", 17, FG, false);
            state.setPadding(0, dp(6), 0, dp(8));
            card.addView(state);
            bar = new ProgressBar(MainActivity.this, null, android.R.attr.progressBarStyleHorizontal);
            bar.setMax(1000);
            bar.setProgressTintList(android.content.res.ColorStateList.valueOf(BLUE));
            card.addView(bar, new LinearLayout.LayoutParams(-1, dp(8)));
            detail = text("", 13, DIM, false);
            detail.setPadding(0, dp(6), 0, 0);
            card.addView(detail);
        }

        void handle(String line) {
            String[] f = line.split(" ", 6);
            switch (f[0]) {
                case "@STATE":
                    if (justDone && line.endsWith("等待对方发送")) {
                        justDone = false;
                        return;
                    }
                    justDone = false;
                    state.setText(line.substring(7));
                    state.setTextColor(FG);
                    break;
                case "@PROGRESS": { // @PROGRESS dir done total mbps name
                    long done = Long.parseLong(f[2]), total = Long.parseLong(f[3]);
                    bar.setProgress(total > 0 ? (int) (done * 1000 / total) : 0);
                    detail.setText(String.format("%.1f / %.1f MB　·　%s Mbps", done / 1e6, total / 1e6, f[4]));
                    break;
                }
                case "@DONE": { // @DONE dir bytes secs mbps path
                    bar.setProgress(1000);
                    state.setText((f[1].equals("send") ? "已发送 " : "已收到 ") + new File(f[5]).getName());
                    state.setTextColor(GREEN);
                    detail.setText(String.format("%.1f MB，用时 %.1f 秒，平均 %s Mbps", Long.parseLong(f[2]) / 1e6, Double.parseDouble(f[3]), f[4]));
                    justDone = true;
                    break;
                }
                case "@ERROR":
                    state.setText("失败：" + line.substring(7));
                    state.setTextColor(RED);
                    break;
            }
        }
    }

    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        getWindow().addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON); // 熄屏后进程会被冻结，传输中保持亮屏
        getWindow().setStatusBarColor(BG);
        getWindow().setNavigationBarColor(BG);
        loadConfig();

        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setPadding(dp(20), dp(28), dp(20), dp(20));
        root.setBackgroundColor(BG);

        root.addView(text("P2P 直传", 26, BLUE, true));
        root.addView(text("打洞直连 · 不经服务器中转 · 会合 " + server.split(":")[0], 12, DIM, false));

        Button pick = new Button(this);
        pick.setText("选择文件，发到电脑");
        pick.setTextSize(16);
        pick.setTextColor(BG);
        pick.setAllCaps(false);
        GradientDrawable pb = new GradientDrawable();
        pb.setColor(BLUE);
        pb.setCornerRadius(dp(14));
        pick.setBackground(pb);
        LinearLayout.LayoutParams plp = new LinearLayout.LayoutParams(-1, dp(56));
        plp.topMargin = dp(20);
        root.addView(pick, plp);
        pick.setOnClickListener(v -> {
            Intent i = new Intent(Intent.ACTION_OPEN_DOCUMENT);
            i.addCategory(Intent.CATEGORY_OPENABLE);
            i.setType("*/*");
            i.putExtra(Intent.EXTRA_ALLOW_MULTIPLE, true);
            startActivityForResult(i, 1);
        });

        sendPanel = new Panel(root, "发到电脑");
        sendPanel.state.setText("点上面的按钮选文件，或在相册里「分享 → 直传到电脑」");
        sendPanel.state.setTextSize(14);
        recvPanel = new Panel(root, "来自电脑（App 开着就能收，存到「下载/P2P直传」）");

        root.addView(text("日志", 12, DIM, true), lpTop(16));
        logView = text("", 11, DIM, false);
        logView.setTypeface(Typeface.MONOSPACE);
        root.addView(logView);

        ScrollView sv = new ScrollView(this);
        sv.setBackgroundColor(BG);
        sv.addView(root);
        setContentView(sv);

        startDaemon();
        watchNetwork();
        handleShare(getIntent());
    }

    @Override
    protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        handleShare(intent);
    }

    @Override
    protected void onDestroy() {
        alive = false;
        if (netWatch != null) getSystemService(ConnectivityManager.class).unregisterNetworkCallback(netWatch);
        if (daemon != null) daemon.destroy();
        super.onDestroy();
    }

    @Override
    protected void onActivityResult(int req, int res, Intent data) {
        if (req != 1 || res != RESULT_OK || data == null) return;
        List<Uri> uris = new ArrayList<>();
        if (data.getClipData() != null) {
            for (int i = 0; i < data.getClipData().getItemCount(); i++) uris.add(data.getClipData().getItemAt(i).getUri());
        } else if (data.getData() != null) {
            uris.add(data.getData());
        }
        sendUris(uris);
    }

    void handleShare(Intent in) {
        if (in == null) return;
        List<Uri> uris = new ArrayList<>();
        if (Intent.ACTION_SEND.equals(in.getAction())) {
            Uri u = in.getParcelableExtra(Intent.EXTRA_STREAM, Uri.class);
            if (u != null) uris.add(u);
        } else if (Intent.ACTION_SEND_MULTIPLE.equals(in.getAction())) {
            ArrayList<Uri> l = in.getParcelableArrayListExtra(Intent.EXTRA_STREAM, Uri.class);
            if (l != null) uris.addAll(l);
        }
        in.setAction(null); // 转屏重建时别再发一遍
        if (!uris.isEmpty()) sendUris(uris);
    }

    // ---------------- 发送 ----------------

    void sendUris(List<Uri> uris) {
        new Thread(() -> {
            File outbox = new File(getCacheDir(), "outbox");
            outbox.mkdirs();
            for (Uri u : uris) {
                String name = displayName(u);
                post(() -> {
                    sendPanel.state.setText("准备 " + name);
                    sendPanel.state.setTextColor(FG);
                    sendPanel.bar.setProgress(0);
                    sendPanel.detail.setText("");
                });
                // p2pcp 要一个真实路径（多条连接各读一段），所以先从 content:// 复制到缓存，本机复制很快
                File tmp = new File(outbox, name);
                try (InputStream is = getContentResolver().openInputStream(u); OutputStream os = new FileOutputStream(tmp)) {
                    copy(is, os);
                } catch (Exception e) {
                    post(() -> sendPanel.handle("@ERROR 读取文件失败：" + e.getMessage()));
                    continue;
                }
                run(sendPanel, "send", "-room", room + ".pc", tmp.getAbsolutePath());
                tmp.delete();
            }
        }).start();
    }

    // ---------------- 接收 ----------------

    void startDaemon() {
        new Thread(() -> {
            File inbox = new File(getCacheDir(), "inbox");
            inbox.mkdirs();
            while (alive) {
                run(recvPanel, "daemon", "-room", room + ".phone", "-o", inbox.getAbsolutePath());
                sleep(2000); // 进程意外退出就重启
            }
        }).start();
    }

    /**
     * 换网（Wi-Fi ↔ 5G、地址变了）后重启 daemon：-lan 是启动时算的，旧地址会让对方白打一个候选，IPv6 socket 也会绑不上。
     * 只看 IPv4 地址集合和"有没有 IPv6"，IPv6 临时地址每天轮换一次，那种变化不值得打断可能正在进行的接收。
     * Go 那边另有保活（会合回执断了就换 socket），这里只负责让地址列表跟上。
     */
    void watchNetwork() {
        ConnectivityManager cm = getSystemService(ConnectivityManager.class);
        netWatch = new ConnectivityManager.NetworkCallback() {
            String last = netKey();

            @Override
            public void onLinkPropertiesChanged(Network n, LinkProperties lp) {
                check();
            }

            @Override
            public void onLost(Network n) {
                check();
            }

            void check() {
                String k = netKey();
                if (k.equals(last)) return;
                post(() -> appendLog("网络变了（" + last + " → " + k + "），重启接收"));
                last = k;
                Process d = daemon;
                if (d != null) d.destroy(); // startDaemon 的循环 2 秒后用新地址重启它
            }
        };
        cm.registerDefaultNetworkCallback(netWatch);
    }

    String netKey() {
        StringBuilder v4 = new StringBuilder();
        boolean v6 = false;
        for (String ip : lanIPs().split(",")) {
            if (ip.contains(":")) v6 = true;
            else if (!ip.isEmpty()) v4.append(ip).append(' ');
        }
        return v4.toString().trim() + (v6 ? " +IPv6" : "");
    }

    /** 收完的文件从缓存挪进公共「下载/P2P直传」（MediaStore，不需要存储权限）。 */
    void publish(String path) {
        File f = new File(path);
        try {
            ContentValues v = new ContentValues();
            v.put(MediaStore.Downloads.DISPLAY_NAME, f.getName());
            v.put(MediaStore.Downloads.RELATIVE_PATH, Environment.DIRECTORY_DOWNLOADS + "/P2P直传");
            v.put(MediaStore.Downloads.IS_PENDING, 1);
            Uri u = getContentResolver().insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, v);
            try (InputStream is = new FileInputStream(f); OutputStream os = getContentResolver().openOutputStream(u)) {
                copy(is, os);
            }
            v.clear();
            v.put(MediaStore.Downloads.IS_PENDING, 0);
            getContentResolver().update(u, v, null, null);
            f.delete();
            post(() -> recvPanel.detail.append("\n已存到 下载/P2P直传/" + f.getName()));
        } catch (Exception e) {
            post(() -> recvPanel.handle("@ERROR 保存失败：" + e.getMessage()));
        }
    }

    // ---------------- 运行 p2pcp ----------------

    /** 启动 p2pcp 并把 stdout 的机器行交给 panel，stderr 进日志。阻塞到进程退出。 */
    void run(Panel panel, String... args) {
        List<String> cmd = new ArrayList<>();
        cmd.add(getApplicationInfo().nativeLibraryDir + "/libp2pcp.so");
        cmd.add(args[0]);
        Collections.addAll(cmd, "-server", server, "-progress", "-lan", lanIPs());
        for (int i = 1; i < args.length; i++) cmd.add(args[i]);
        try {
            Process p = new ProcessBuilder(cmd).start();
            if (args[0].equals("daemon")) daemon = p;
            new Thread(() -> pump(p.getErrorStream(), null)).start();
            pump(p.getInputStream(), panel);
            p.waitFor();
        } catch (Exception e) {
            post(() -> panel.handle("@ERROR " + e.getMessage()));
        }
    }

    void pump(InputStream in, Panel panel) {
        try (BufferedReader r = new BufferedReader(new InputStreamReader(in))) {
            String line;
            while ((line = r.readLine()) != null) {
                final String l = line;
                if (panel == null) {
                    post(() -> appendLog(l));
                    continue;
                }
                post(() -> panel.handle(l));
                if (panel == recvPanel && l.startsWith("@DONE ")) publish(l.split(" ", 6)[5]);
            }
        } catch (Exception ignored) {
        }
    }

    void appendLog(String l) {
        // 日志行形如 "08:36:33.859654 [0] 打通 ..."，去掉微秒；只留最后 14 行
        String s = l.length() > 16 && l.charAt(8) == '.' ? l.substring(0, 8) + l.substring(15) : l;
        String all = logView.getText() + s + "\n";
        String[] lines = all.split("\n");
        int from = Math.max(0, lines.length - 14);
        StringBuilder sb = new StringBuilder();
        for (int i = from; i < lines.length; i++) sb.append(lines[i]).append('\n');
        logView.setText(sb);
    }

    // ---------------- 小工具 ----------------

    void loadConfig() {
        Properties p = new Properties();
        try (InputStream is = getAssets().open("config.properties")) {
            p.load(is);
        } catch (Exception ignored) {
        }
        room = p.getProperty("room", "p2pcp");
        server = p.getProperty("server", "8.140.18.211:7001,8.140.18.211:7000");
    }

    /**
     * Go 在 Android 11+ 的 App 里读不到网卡列表（netlink 被限制），由 Java 取好地址传给它：IPv4 内网地址（同一局域网时走内网）
     * 和全局 IPv6。只取当前默认网络的：刚从 5G 切到 Wi-Fi 时，5G 网卡还会挂一阵，它的地址这时已经用不上了；VPN 的 tun 也不算。
     */
    String lanIPs() {
        List<InetAddress> addrs = new ArrayList<>();
        ConnectivityManager cm = getSystemService(ConnectivityManager.class);
        LinkProperties lp = cm.getLinkProperties(cm.getActiveNetwork());
        if (lp != null) {
            for (LinkAddress la : lp.getLinkAddresses()) addrs.add(la.getAddress());
        }
        StringBuilder sb = new StringBuilder();
        for (InetAddress a : addrs) {
            // 不要链路本地和 ULA fc00::/7
            boolean v6 = a instanceof Inet6Address && !a.isLinkLocalAddress() && (a.getAddress()[0] & 0xfe) != 0xfc;
            if (a instanceof Inet4Address || v6) {
                String s = a.getHostAddress();
                int pct = s.indexOf('%');
                if (sb.length() > 0) sb.append(',');
                sb.append(pct < 0 ? s : s.substring(0, pct));
            }
        }
        return sb.toString();
    }

    String displayName(Uri u) {
        try (Cursor c = getContentResolver().query(u, new String[]{OpenableColumns.DISPLAY_NAME}, null, null, null)) {
            if (c != null && c.moveToFirst()) return c.getString(0).replace('/', '_');
        } catch (Exception ignored) {
        }
        return "file-" + System.currentTimeMillis();
    }

    static void copy(InputStream is, OutputStream os) throws java.io.IOException {
        byte[] buf = new byte[1 << 20];
        int n;
        while ((n = is.read(buf)) > 0) os.write(buf, 0, n);
    }

    static void sleep(long ms) {
        try {
            Thread.sleep(ms);
        } catch (InterruptedException ignored) {
        }
    }

    void post(Runnable r) {
        ui.post(r);
    }

    TextView text(String s, int sp, int color, boolean bold) {
        TextView t = new TextView(this);
        t.setText(s);
        t.setTextSize(sp);
        t.setTextColor(color);
        if (bold) t.setTypeface(Typeface.DEFAULT_BOLD);
        return t;
    }

    LinearLayout.LayoutParams lpTop(int top) {
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(-1, -2);
        lp.topMargin = dp(top);
        return lp;
    }

    int dp(int v) {
        return Math.round(v * getResources().getDisplayMetrics().density);
    }
}
