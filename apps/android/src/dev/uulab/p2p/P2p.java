package dev.uulab.p2p;

import android.content.Context;
import android.net.ConnectivityManager;
import android.net.LinkAddress;
import android.net.LinkProperties;

import java.io.BufferedReader;
import java.io.File;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.net.Inet4Address;
import java.net.Inet6Address;
import java.net.InetAddress;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Properties;

/**
 * 启动打包在 APK 里的 Go 程序 p2pcp（lib/arm64-v8a/libp2pcp.so）的公共代码：界面和后台服务都用它。
 * 身份和通讯录放 App 私有目录（-config），画像缓存和预测记录放缓存目录（XDG_CACHE_HOME）。
 */
final class P2p {
    interface Lines {
        void line(String l);
    }

    private P2p() {
    }

    static String server(Context c) {
        Properties p = new Properties();
        try (InputStream is = c.getAssets().open("config.properties")) {
            p.load(is);
        } catch (Exception ignored) {
        }
        return p.getProperty("server", ""); // build.sh 一定会写；万一没有，p2pcp 会报「没有会合服务器」
    }

    static File configDir(Context c) {
        return new File(c.getFilesDir(), "p2pcp");
    }

    /** 拼命令：p2pcp <命令> <公共参数> <其余参数>。Go 的 flag 遇到第一个非 flag 参数就停，所以公共参数紧跟在命令后面。 */
    static List<String> cmd(Context c, String... args) {
        List<String> cmd = new ArrayList<>();
        cmd.add(c.getApplicationInfo().nativeLibraryDir + "/libp2pcp.so");
        cmd.add(args[0]);
        Collections.addAll(cmd, "-config", configDir(c).getPath(), "-server", server(c), "-progress", "-lan", lanIPs(c));
        for (int i = 1; i < args.length; i++) cmd.add(args[i]);
        return cmd;
    }

    static Process start(Context c, String... args) throws java.io.IOException {
        ProcessBuilder pb = new ProcessBuilder(cmd(c, args));
        // 体检的画像缓存和预测记录放 App 的缓存目录：App 进程没有可用的 HOME，不给就每次现测画像，
        // 当发送端时对方总拿不到它的画像（打洞比现测画像快，lab/p2pcp/preflight.go）
        pb.environment().put("XDG_CACHE_HOME", c.getCacheDir().getPath());
        return pb.start();
    }

    /** 跑一条命令直到退出：stdout 的每一行交给 out，stderr（日志）交给 log。返回退出码。 */
    static int run(Context c, Lines out, Lines log, String... args) {
        try {
            Process p = start(c, args);
            Thread t = new Thread(() -> pump(p.getErrorStream(), log));
            t.start();
            pump(p.getInputStream(), out);
            int rc = p.waitFor();
            t.join(500);
            return rc;
        } catch (Exception e) {
            out.line("@ERROR " + e.getMessage());
            return -1;
        }
    }

    static void pump(InputStream in, Lines to) {
        try (BufferedReader r = new BufferedReader(new InputStreamReader(in))) {
            String line;
            while ((line = r.readLine()) != null) if (to != null) to.line(line);
        } catch (Exception ignored) {
        }
    }

    /**
     * Go 在 Android 11+ 的 App 里读不到网卡列表（netlink 被限制），由 Java 取好地址传给它：IPv4 内网地址（同一局域网时走内网）
     * 和全局 IPv6。只取当前默认网络的：刚从 5G 切到 Wi-Fi 时，5G 网卡还会挂一阵，它的地址这时已经用不上了；VPN 的 tun 也不算。
     */
    static String lanIPs(Context c) {
        List<InetAddress> addrs = new ArrayList<>();
        ConnectivityManager cm = c.getSystemService(ConnectivityManager.class);
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

    /** 只看 IPv4 地址集合和"有没有 IPv6"：IPv6 临时地址每天轮换一次，那种变化不值得打断可能正在进行的接收。 */
    static String netKey(Context c) {
        StringBuilder v4 = new StringBuilder();
        boolean v6 = false;
        for (String ip : lanIPs(c).split(",")) {
            if (ip.contains(":")) v6 = true;
            else if (!ip.isEmpty()) v4.append(ip).append(' ');
        }
        return v4.toString().trim() + (v6 ? " +IPv6" : "");
    }
}
