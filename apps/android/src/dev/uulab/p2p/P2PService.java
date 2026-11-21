package dev.uulab.p2p;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.ContentValues;
import android.content.Intent;
import android.content.pm.ServiceInfo;
import android.net.ConnectivityManager;
import android.net.LinkProperties;
import android.net.Network;
import android.net.Uri;
import android.os.Environment;
import android.os.IBinder;
import android.provider.MediaStore;

import java.io.File;
import java.io.FileInputStream;
import java.io.InputStream;
import java.io.OutputStream;

/**
 * 后台接收：前台服务里常驻 p2pcp serve（同时听所有好友，也替「邀请好友」等对方来加），关掉界面也能收。
 * 收完的文件从缓存挪进「下载/P2P直传/<好友名>/」。界面开着时，stdout 的机器行转给它（listener）。
 *
 * 后台每 20 秒注册一次（-reg 20s）：会合服务器只给 30 秒内注册过的配对，NAT 映射最短也有 60 秒；
 * 比原来的 5 秒省电，代价是好友来发时最多晚 20 秒才配上。
 */
public class P2PService extends Service {
    static volatile P2p.Lines listener; // 界面开着时非空
    static volatile String lastState = "启动中…";

    volatile boolean alive = true;
    volatile Process serve;
    ConnectivityManager.NetworkCallback netWatch;
    NotificationManager nm;

    @Override
    public void onCreate() {
        super.onCreate();
        nm = getSystemService(NotificationManager.class);
        nm.createNotificationChannel(new NotificationChannel("serve", "后台接收", NotificationManager.IMPORTANCE_LOW));
        nm.createNotificationChannel(new NotificationChannel("got", "收到文件 / 新好友", NotificationManager.IMPORTANCE_DEFAULT));
        startForeground(1, ongoing(lastState), ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC);
        new Thread(this::loop).start();
        watchNetwork();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        return START_STICKY;
    }

    @Override
    public void onDestroy() {
        alive = false;
        if (netWatch != null) getSystemService(ConnectivityManager.class).unregisterNetworkCallback(netWatch);
        Process p = serve;
        if (p != null) p.destroy();
        super.onDestroy();
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }

    void loop() {
        File inbox = new File(getCacheDir(), "inbox");
        inbox.mkdirs();
        while (alive) {
            try {
                Process p = P2p.start(this, "serve", "-reg", "20s", "-o", inbox.getAbsolutePath());
                serve = p;
                new Thread(() -> P2p.pump(p.getErrorStream(), l -> forward("#" + l))).start();
                P2p.pump(p.getInputStream(), this::handle);
                p.waitFor();
            } catch (Exception e) {
                forward("@ERROR " + e.getMessage());
            }
            sleep(2000); // 进程意外退出、或者换网被杀，就重启
        }
    }

    void handle(String l) {
        if (l.startsWith("@STATE ")) {
            lastState = l.substring(7);
            nm.notify(1, ongoing(lastState));
        } else if (l.startsWith("@DONE recv ")) {
            String path = l.split(" ", 6)[5];
            String friend = new File(path).getParentFile().getName();
            String saved = publish(path, friend);
            if (saved != null) {
                notifyGot("收到「" + friend + "」发来的文件", saved);
                l = l + "\t" + saved; // 界面显示存到了哪
            }
        } else if (l.startsWith("@PAIRED ")) {
            notifyGot("新好友", l.substring(8));
        }
        forward(l);
    }

    void forward(String l) {
        P2p.Lines to = listener;
        if (to != null) to.line(l);
    }

    /** 收完的文件从缓存挪进公共「下载/P2P直传/好友名」（MediaStore，不需要存储权限），返回给人看的位置 */
    String publish(String path, String friend) {
        File f = new File(path);
        String rel = Environment.DIRECTORY_DOWNLOADS + "/P2P直传/" + friend;
        try {
            ContentValues v = new ContentValues();
            v.put(MediaStore.Downloads.DISPLAY_NAME, f.getName());
            v.put(MediaStore.Downloads.RELATIVE_PATH, rel);
            v.put(MediaStore.Downloads.IS_PENDING, 1);
            Uri u = getContentResolver().insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, v);
            try (InputStream is = new FileInputStream(f); OutputStream os = getContentResolver().openOutputStream(u)) {
                byte[] buf = new byte[1 << 20];
                int n;
                while ((n = is.read(buf)) > 0) os.write(buf, 0, n);
            }
            v.clear();
            v.put(MediaStore.Downloads.IS_PENDING, 0);
            getContentResolver().update(u, v, null, null);
            f.delete();
            return "下载/P2P直传/" + friend + "/" + f.getName();
        } catch (Exception e) {
            forward("@ERROR 保存失败：" + e.getMessage());
            return null;
        }
    }

    Notification ongoing(String text) {
        return new Notification.Builder(this, "serve")
                .setSmallIcon(android.R.drawable.stat_sys_download_done)
                .setContentTitle("P2P 直传 · 后台接收")
                .setContentText(text)
                .setOngoing(true)
                .setContentIntent(openApp())
                .build();
    }

    void notifyGot(String title, String text) {
        nm.notify((int) (System.currentTimeMillis() & 0xffffff) + 2, new Notification.Builder(this, "got")
                .setSmallIcon(android.R.drawable.stat_sys_download_done)
                .setContentTitle(title)
                .setContentText(text)
                .setAutoCancel(true)
                .setContentIntent(openApp())
                .build());
    }

    PendingIntent openApp() {
        return PendingIntent.getActivity(this, 0, new Intent(this, MainActivity.class), PendingIntent.FLAG_IMMUTABLE);
    }

    /** 换网（Wi-Fi ↔ 5G、地址变了）后重启 serve：-lan 是启动时算的，旧地址会让对方白打一个候选，IPv6 socket 也会绑不上。 */
    void watchNetwork() {
        ConnectivityManager cm = getSystemService(ConnectivityManager.class);
        netWatch = new ConnectivityManager.NetworkCallback() {
            String last = P2p.netKey(P2PService.this);

            @Override
            public void onLinkPropertiesChanged(Network n, LinkProperties lp) {
                check();
            }

            @Override
            public void onLost(Network n) {
                check();
            }

            void check() {
                String k = P2p.netKey(P2PService.this);
                if (k.equals(last)) return;
                forward("#网络变了（" + last + " → " + k + "），重启接收");
                last = k;
                Process p = serve;
                if (p != null) p.destroy(); // loop 2 秒后用新地址重启它
            }
        };
        cm.registerDefaultNetworkCallback(netWatch);
    }

    static void sleep(long ms) {
        try {
            Thread.sleep(ms);
        } catch (InterruptedException ignored) {
        }
    }
}
