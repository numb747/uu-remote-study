package dev.uulab.p2p;

import android.app.Activity;
import android.app.AlertDialog;
import android.content.ClipData;
import android.content.ClipboardManager;
import android.content.ContentValues;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.database.Cursor;
import android.graphics.BitmapFactory;
import android.graphics.Typeface;
import android.graphics.drawable.GradientDrawable;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Environment;
import android.os.Handler;
import android.os.Looper;
import android.os.PowerManager;
import android.provider.MediaStore;
import android.provider.OpenableColumns;
import android.provider.Settings;
import android.view.Gravity;
import android.widget.Button;
import android.widget.EditText;
import android.widget.ImageView;
import android.widget.LinearLayout;
import android.widget.ProgressBar;
import android.widget.ScrollView;
import android.widget.TextView;
import android.widget.Toast;
import android.view.WindowManager;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.File;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.io.OutputStream;
import java.util.ArrayList;
import java.util.List;
import java.util.function.Consumer;

/**
 * P2P 直传：界面只是外壳，真正干活的是打包在 APK 里的 Go 程序 p2pcp（P2p.java 负责启动）。
 *
 *   通讯录：每台设备有自己的设备密钥；「邀请好友」出一次性邀请码（二维码 / 文字，发微信），朋友在 App 里「加好友」粘贴，
 *           或者用系统相机扫码直接跳进来（p2pcp://join/…）。两边互相加上之后，选好友就能发
 *   发送：选好友 → 选文件（或在相册里「分享 → P2P 直传」）→ 复制到缓存目录 → p2pcp send -to 好友
 *   接收：后台服务 P2PService 常驻 p2pcp serve，收完挪进「下载/P2P直传/好友名」
 *   诊断：失败时显示原因码对应的人话和动作（@REASON），「生成诊断报告」（p2pcp report）存进「下载/P2P直传」再分享；
 *         诊断数据上报默认开，第一次打开时说明传了什么，底下可以关（p2pcp -upload on|off，记在 settings.json）
 */
public class MainActivity extends Activity {
    // Tokyo Night
    static final int BG = 0xff1a1b26, CARD = 0xff24283b, FG = 0xffc0caf5, DIM = 0xff565f89,
            BLUE = 0xff7aa2f7, GREEN = 0xff9ece6a, RED = 0xfff7768e;

    final Handler ui = new Handler(Looper.getMainLooper());
    Panel sendPanel, recvPanel;
    TextView logView, meView, uploadView;
    LinearLayout friendsBox;
    final List<String> friends = new ArrayList<>();
    String pendingFriend; // 选文件之前选好的好友
    volatile Process sendProc;     // 正在跑的 p2pcp send（取消时往它的 stdin 写 cancel）
    volatile boolean sendAborted;  // 还在复制到缓存时就点了取消
    AlertDialog inviteDlg;

    /** 一块显示区：标题、状态、进度条、详情。 */
    class Panel {
        TextView state, detail, cancel;
        ProgressBar bar;
        final boolean recv;
        boolean justDone; // 刚显示完成：serve 紧接着报的「等待好友发送」不要盖掉它
        String[] reason;  // 紧跟着 @ERROR 的那条 @REASON：{原因码, 人话, 动作}

        Panel(LinearLayout parent, String title, boolean recv) {
            this.recv = recv;
            LinearLayout card = card(parent);
            LinearLayout head = new LinearLayout(MainActivity.this);
            head.setGravity(Gravity.CENTER_VERTICAL);
            head.addView(text(title, 13, DIM, true), new LinearLayout.LayoutParams(0, -2, 1));
            // 取消：传输进行中才显示（手动取消，lab/p2pcp/cancel.go）
            cancel = text("取消", 14, RED, false);
            cancel.setPadding(dp(14), dp(4), dp(14), dp(4));
            GradientDrawable cb = new GradientDrawable();
            cb.setStroke(dp(1), RED);
            cb.setCornerRadius(dp(8));
            cancel.setBackground(cb);
            cancel.setVisibility(android.view.View.GONE);
            cancel.setOnClickListener(v -> onCancel(this));
            head.addView(cancel);
            card.addView(head);
            state = text("—", 16, FG, false);
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

        void showCancel(boolean on) {
            cancel.setVisibility(on ? android.view.View.VISIBLE : android.view.View.GONE);
        }

        void handle(String line) {
            String[] f = line.split(" ", 6);
            switch (f[0]) {
                case "@STATE":
                    if (recv && line.startsWith("@STATE 等待好友发送")) showCancel(false);
                    if (justDone && line.startsWith("@STATE 等待好友发送")) return;
                    justDone = false;
                    state.setText(line.substring(7));
                    state.setTextColor(FG);
                    break;
                case "@PROGRESS": { // @PROGRESS dir done total mbps name
                    if (recv) showCancel(true);
                    long done = Long.parseLong(f[2]), total = Long.parseLong(f[3]);
                    bar.setProgress(total > 0 ? (int) (done * 1000 / total) : 0);
                    detail.setText(String.format("%.1f / %.1f MB　·　%s Mbps", done / 1e6, total / 1e6, f[4]));
                    break;
                }
                case "@DONE": { // @DONE dir bytes secs mbps path[\t存到哪]
                    String[] tail = f[5].split("\t", 2);
                    bar.setProgress(1000);
                    state.setText((f[1].equals("send") ? "已发送 " : "已收到 ") + new File(tail[0]).getName());
                    state.setTextColor(GREEN);
                    String d = String.format("%.1f MB，用时 %.1f 秒，平均 %s Mbps", Long.parseLong(f[2]) / 1e6, Double.parseDouble(f[3]), f[4]);
                    if (tail.length > 1) d += "\n已存到 " + tail[1];
                    detail.setText(d);
                    justDone = true;
                    if (recv) showCancel(false);
                    break;
                }
                case "@CANCELED": { // @CANCELED me|peer 文字
                    boolean peer = line.startsWith("@CANCELED peer");
                    state.setText(peer ? "对方取消了" : "已取消");
                    state.setTextColor(DIM);
                    detail.setOnClickListener(null);
                    detail.setText(recv && !peer ? "收了一半的部分已删掉" : peer ? "要的话请对方重新发" : "");
                    justDone = true; // 别被紧接着的「等待好友发送」盖掉
                    showCancel(false);
                    break;
                }
                case "@REASON": { // @REASON <码> <人话>。<动作>
                    String[] r = line.split(" ", 3);
                    if (r.length < 3) break;
                    int dot = r[2].indexOf('。');
                    reason = dot < 0 ? new String[]{r[1], r[2], ""} : new String[]{r[1], r[2].substring(0, dot), r[2].substring(dot + 1)};
                    break;
                }
                case "@ERROR": {
                    String err = line.length() > 7 ? line.substring(7) : "";
                    if (recv) showCancel(false);
                    state.setTextColor(RED);
                    detail.setOnClickListener(null);
                    if (reason == null) { // 旧版 p2pcp，或者 App 自己报的错
                        state.setText("失败：" + err);
                        break;
                    }
                    String[] r = reason;
                    reason = null;
                    state.setText("失败：" + r[1]);
                    detail.setText(r[2] + "\n点这里看详情");
                    detail.setOnClickListener(v -> new AlertDialog.Builder(MainActivity.this)
                            .setTitle(r[1])
                            .setMessage(err + "\n\n原因码 " + r[0] + "\n\n还是不行的话，点下面的「生成诊断报告」发给我。")
                            .setPositiveButton("知道了", null)
                            .show());
                    break;
                }
            }
        }
    }

    void onCancel(Panel p) {
        if (!p.recv) {
            sendAborted = true; // 还在复制的话，复制循环看到它就停
            p.state.setText("正在取消…");
            p.state.setTextColor(DIM);
            Process sp = sendProc;
            bg(() -> P2p.command(sp, "cancel"));
            return;
        }
        new AlertDialog.Builder(this)
                .setTitle("取消接收？")
                .setMessage("收了一半的部分会删掉，对方要重新发。")
                .setPositiveButton("取消接收", (d, w) -> {
                    p.state.setText("正在取消…");
                    bg(P2PService::cancelRecv);
                })
                .setNegativeButton("继续收", null)
                .show();
    }

    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        getWindow().addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON); // 发送时熄屏会被冻结
        getWindow().setStatusBarColor(BG);
        getWindow().setNavigationBarColor(BG);

        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setPadding(dp(20), dp(28), dp(20), dp(20));
        root.setBackgroundColor(BG);

        root.addView(text("P2P 直传", 26, BLUE, true));
        meView = text("本机：…", 13, DIM, false);
        meView.setPadding(0, dp(4), 0, 0);
        meView.setOnClickListener(v -> renameMe());
        root.addView(meView);

        LinearLayout row = new LinearLayout(this);
        row.setOrientation(LinearLayout.HORIZONTAL);
        root.addView(row, lpTop(16));
        row.addView(button("邀请好友", BLUE, v -> invite()), new LinearLayout.LayoutParams(0, dp(52), 1));
        LinearLayout.LayoutParams gap = new LinearLayout.LayoutParams(0, dp(52), 1);
        gap.leftMargin = dp(12);
        row.addView(button("加好友", GREEN, v -> addFriend()), gap);

        LinearLayout fc = card(root);
        fc.addView(text("好友（点一下选文件发给他，长按改名 / 删除）", 13, DIM, true));
        friendsBox = new LinearLayout(this);
        friendsBox.setOrientation(LinearLayout.VERTICAL);
        fc.addView(friendsBox);

        sendPanel = new Panel(root, "发送", false);
        sendPanel.state.setText("选一个好友，或在相册里「分享 → P2P 直传」");
        sendPanel.state.setTextSize(14);
        recvPanel = new Panel(root, "接收（后台常驻，存到「下载/P2P直传/好友名」）", true);
        recvPanel.state.setText(P2PService.lastState);
        // 国产安卓熄屏后会冻结后台 App（vivo 实测：前台服务、电池白名单都挡不住），要用户在系统设置里放行
        TextView bgTip = text("熄屏后收不到？点这里，在「电池」里打开「允许后台高耗电」（不同手机叫法不同：后台运行、自启动、不限制）", 13, BLUE, false);
        bgTip.setPadding(0, dp(8), 0, 0);
        bgTip.setOnClickListener(v -> startActivity(new Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:" + getPackageName()))));
        root.addView(bgTip);

        root.addView(text("日志", 12, DIM, true), lpTop(16));
        logView = text("", 11, DIM, false);
        logView.setTypeface(Typeface.MONOSPACE);
        root.addView(logView);

        LinearLayout dc = card(root);
        dc.addView(text("诊断", 13, DIM, true));
        TextView report = text("生成诊断报告（打不通、收不到时用：现测一次网络，加上最近的连接记录和日志，好友名、文件名换成代号，可以直接发微信）", 14, BLUE, false);
        report.setPadding(0, dp(8), 0, dp(8));
        report.setOnClickListener(v -> makeReport());
        dc.addView(report);
        uploadView = text("诊断数据上报：…", 14, FG, false);
        uploadView.setPadding(0, dp(8), 0, dp(4));
        uploadView.setOnClickListener(v -> toggleUpload());
        dc.addView(uploadView);
        refreshUpload();

        TextView stop = text("停止后台接收并退出", 13, DIM, false);
        stop.setPadding(0, dp(18), 0, 0);
        stop.setOnClickListener(v -> {
            stopService(new Intent(this, P2PService.class));
            finishAndRemoveTask();
        });
        root.addView(stop);

        ScrollView sv = new ScrollView(this);
        sv.setBackgroundColor(BG);
        sv.addView(root);
        setContentView(sv);

        if (Build.VERSION.SDK_INT >= 33 && checkSelfPermission("android.permission.POST_NOTIFICATIONS") != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(new String[]{"android.permission.POST_NOTIFICATIONS"}, 2);
        }
        askIgnoreBattery();
        uploadNotice();
        P2PService.listener = this::onService;
        new Thread(() -> {
            ensureName();
            refreshMe();
            refreshFriends();
            startForegroundService(new Intent(this, P2PService.class)); // 身份有了再起 serve
        }).start();
        handleIntent(getIntent());
    }

    @Override
    protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        handleIntent(intent);
    }

    @Override
    protected void onDestroy() {
        P2PService.listener = null;
        super.onDestroy();
    }

    /** 标准安卓的「忽略电池优化」：没有的话熄屏一阵子后系统会断掉后台联网（vivo 等还要另外放行，见 bgTip） */
    void askIgnoreBattery() {
        PowerManager pm = getSystemService(PowerManager.class);
        if (pm.isIgnoringBatteryOptimizations(getPackageName())) return;
        try {
            startActivity(new Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, Uri.parse("package:" + getPackageName())));
        } catch (Exception ignored) {
        }
    }

    /** 后台服务（serve）的输出：# 开头是日志，其余是机器行 */
    void onService(String l) {
        post(() -> {
            if (l.startsWith("#")) {
                appendLog(l.substring(1));
            } else if (l.startsWith("@CRASH ")) {
                appendLog("后台接收上次崩溃了，现场会放进诊断报告");
            } else if (l.startsWith("@PAIRED ")) {
                toast("加上新好友了");
                if (inviteDlg != null) inviteDlg.dismiss();
                bg(this::refreshFriends);
            } else {
                recvPanel.handle(l);
            }
        });
    }

    // ---------------- 通讯录 ----------------

    /** 第一次用：设备名默认取系统里的「设备名称」（比型号好认），之后可以点本机名改 */
    void ensureName() {
        if (new File(P2p.configDir(this), "identity.json").exists()) return;
        String name = Settings.Global.getString(getContentResolver(), Settings.Global.DEVICE_NAME);
        if (name == null || name.isEmpty()) name = Build.MODEL;
        P2p.run(this, l -> {
        }, this::log, "id", "-name", name);
    }

    void refreshMe() {
        P2p.run(this, l -> {
            if (!l.startsWith("@ID ")) return;
            try {
                JSONObject o = new JSONObject(l.substring(4));
                post(() -> meView.setText("本机：" + o.optString("name") + " · ID " + o.optString("id") + "（点这里改名）"));
            } catch (Exception ignored) {
            }
        }, this::log, "id");
    }

    void refreshFriends() {
        List<String[]> rows = new ArrayList<>();
        P2p.run(this, l -> {
            if (!l.startsWith("@CONTACTS ")) return;
            try {
                JSONArray a = new JSONArray(l.substring(10).equals("null") ? "[]" : l.substring(10));
                for (int i = 0; i < a.length(); i++) rows.add(new String[]{a.getJSONObject(i).getString("name"), a.getJSONObject(i).getString("id")});
            } catch (Exception ignored) {
            }
        }, this::log, "contacts");
        post(() -> {
            friends.clear();
            friendsBox.removeAllViews();
            for (String[] r : rows) {
                friends.add(r[0]);
                LinearLayout item = new LinearLayout(this);
                item.setOrientation(LinearLayout.HORIZONTAL);
                item.setGravity(Gravity.CENTER_VERTICAL);
                item.setPadding(0, dp(10), 0, dp(10));
                TextView name = text(r[0], 17, FG, false);
                item.addView(name, new LinearLayout.LayoutParams(0, -2, 1));
                item.addView(text(r[1], 12, DIM, false));
                item.setOnClickListener(v -> pickFor(r[0]));
                item.setOnLongClickListener(v -> {
                    friendMenu(r[0]);
                    return true;
                });
                friendsBox.addView(item);
            }
            if (rows.isEmpty()) {
                TextView t = text("还没有好友：点「邀请好友」把邀请码发给朋友，或者点「加好友」粘贴朋友给你的邀请码。", 14, FG, false);
                t.setPadding(0, dp(8), 0, 0);
                friendsBox.addView(t);
            }
        });
    }

    void invite() {
        File png = new File(getCacheDir(), "invite.png");
        bg(() -> {
            String[] code = {null};
            P2p.run(this, l -> {
                if (l.startsWith("@INVITE ")) code[0] = l.substring(8);
            }, this::log, "invite", "-qr", png.getPath());
            if (code[0] == null) {
                post(() -> toast("生成邀请码失败，看日志"));
                return;
            }
            post(() -> showInvite(code[0], png));
        });
    }

    void showInvite(String code, File png) {
        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(20), dp(12), dp(20), 0);
        ImageView img = new ImageView(this);
        img.setImageBitmap(BitmapFactory.decodeFile(png.getPath()));
        img.setAdjustViewBounds(true);
        box.addView(img, new LinearLayout.LayoutParams(-1, -2));
        TextView t = text("朋友当面就用相机扫码；不在身边就把邀请码发给他，他在 P2P 直传里点「加好友」。24 小时内有效，用一次就作废。\n\n" + code, 13, FG, false);
        t.setTextIsSelectable(true);
        t.setPadding(0, dp(10), 0, 0);
        box.addView(t);
        inviteDlg = new AlertDialog.Builder(this)
                .setTitle("邀请好友")
                .setView(box)
                .setPositiveButton("复制邀请码", (d, w) -> {
                    getSystemService(ClipboardManager.class).setPrimaryClip(ClipData.newPlainText("P2P 直传邀请码", "加我 P2P 直传好友：" + code));
                    toast("已复制，去微信里粘贴发给朋友");
                })
                .setNeutralButton("分享…", (d, w) -> {
                    Intent s = new Intent(Intent.ACTION_SEND).setType("text/plain").putExtra(Intent.EXTRA_TEXT, "加我 P2P 直传好友：" + code);
                    startActivity(Intent.createChooser(s, "发送邀请码"));
                })
                .setNegativeButton("关闭", null)
                .show();
    }

    /** 加好友：剪贴板里有邀请码就直接用，没有就让用户粘贴 */
    void addFriend() {
        String clip = "";
        ClipData c = getSystemService(ClipboardManager.class).getPrimaryClip();
        if (c != null && c.getItemCount() > 0 && c.getItemAt(0).getText() != null) clip = c.getItemAt(0).getText().toString();
        if (clip.contains("p2pcp://join/")) {
            join(clip);
            return;
        }
        EditText in = new EditText(this);
        in.setHint("p2pcp://join/…");
        new AlertDialog.Builder(this)
                .setTitle("粘贴朋友发来的邀请码")
                .setView(in)
                .setPositiveButton("加好友", (d, w) -> join(in.getText().toString()))
                .setNegativeButton("取消", null)
                .show();
    }

    void join(String text) {
        sendPanel.state.setText("正在加好友…");
        sendPanel.state.setTextColor(FG);
        bg(() -> {
            int rc = P2p.run(this, l -> post(() -> sendPanel.handle(l)), this::log, "join", text);
            if (rc == 0) {
                post(() -> toast("加上好友了"));
                refreshFriends();
            }
        });
    }

    void friendMenu(String name) {
        new AlertDialog.Builder(this)
                .setTitle(name)
                .setItems(new String[]{"改名", "删除"}, (d, which) -> {
                    if (which == 0) {
                        ask("给「" + name + "」改名", name, n -> bg(() -> {
                            P2p.run(this, l -> {
                            }, this::log, "contacts", "rename", name, n);
                            refreshFriends();
                        }));
                    } else {
                        new AlertDialog.Builder(this)
                                .setMessage("删除好友「" + name + "」？删了之后他发来的文件这边不再接收，要重新加好友才行。")
                                .setPositiveButton("删除", (d2, w) -> bg(() -> {
                                    P2p.run(this, l -> {
                                    }, this::log, "contacts", "rm", name);
                                    refreshFriends();
                                }))
                                .setNegativeButton("取消", null)
                                .show();
                    }
                })
                .show();
    }

    void renameMe() {
        String cur = meView.getText().toString().replaceFirst("^本机：", "").replaceFirst(" · ID.*$", "");
        ask("本机的名字（好友看到的就是它）", cur, n -> bg(() -> {
            P2p.run(this, l -> {
            }, this::log, "id", "-name", n);
            refreshMe();
            toast2("改好了；已经加了的好友那边显示的还是旧名字，他们可以长按改名");
        }));
    }

    // ---------------- 诊断 ----------------

    /** 诊断报告：p2pcp report 写在缓存目录，复制进「下载/P2P直传」（MediaStore 的 Uri 可以直接分享，不用 FileProvider） */
    void makeReport() {
        toast("正在生成诊断报告，要现测一次网络，十几秒……");
        bg(() -> {
            String[] path = {null};
            P2p.run(this, l -> {
                if (l.startsWith("@REPORT ")) path[0] = l.substring(8);
            }, this::log, "report");
            if (path[0] == null) {
                post(() -> toast("生成失败，看日志"));
                return;
            }
            File f = new File(path[0]);
            Uri u = null;
            try {
                ContentValues v = new ContentValues();
                v.put(MediaStore.Downloads.DISPLAY_NAME, "P2P直传诊断报告-" + f.getName().replaceFirst("^report-", ""));
                v.put(MediaStore.Downloads.MIME_TYPE, "text/plain");
                v.put(MediaStore.Downloads.RELATIVE_PATH, Environment.DIRECTORY_DOWNLOADS + "/P2P直传");
                u = getContentResolver().insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, v);
                try (InputStream is = new java.io.FileInputStream(f); OutputStream os = getContentResolver().openOutputStream(u)) {
                    byte[] buf = new byte[1 << 16];
                    int n;
                    while ((n = is.read(buf)) > 0) os.write(buf, 0, n);
                }
            } catch (Exception e) {
                log("保存诊断报告失败：" + e.getMessage());
                post(() -> toast("保存诊断报告失败，看日志"));
                return;
            }
            Uri share = u;
            post(() -> {
                toast2("诊断报告已存到「下载/P2P直传」");
                Intent s = new Intent(Intent.ACTION_SEND).setType("text/plain").putExtra(Intent.EXTRA_STREAM, share)
                        .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION);
                startActivity(Intent.createChooser(s, "发送诊断报告"));
            });
        });
    }

    /** 上报开关存在 p2pcp 的 settings.json（{"upload": false}），没有这个字段 = 默认开 */
    boolean uploadOn() {
        try {
            byte[] b = java.nio.file.Files.readAllBytes(new File(P2p.configDir(this), "settings.json").toPath());
            return new JSONObject(new String(b, java.nio.charset.StandardCharsets.UTF_8)).optBoolean("upload", true);
        } catch (Exception e) {
            return true;
        }
    }

    void refreshUpload() {
        boolean on = uploadOn();
        uploadView.setText("诊断数据上报：" + (on ? "开（点这里关掉）" : "关（点这里打开）"));
    }

    void toggleUpload() {
        boolean on = uploadOn();
        bg(() -> {
            P2p.run(this, l -> {
            }, this::log, "id", "-upload", on ? "off" : "on");
            post(this::refreshUpload);
        });
    }

    /** 第一次打开：一句话说清上报了什么（DESIGN 决策 22） */
    void uploadNotice() {
        android.content.SharedPreferences sp = getSharedPreferences("p2p", MODE_PRIVATE);
        if (sp.getBoolean("uploadNotice", false)) return;
        sp.edit().putBoolean("uploadNotice", true).apply();
        new AlertDialog.Builder(this)
                .setTitle("诊断数据上报")
                .setMessage("每次连接结束后，P2P 直传会把这次连接的结果发给会合服务器，用来改进打洞：两边的网络类型和公网 IP、"
                        + "有没有直连、用了多久、失败原因。不含文件名、好友名和任何密钥。\n\n可以随时在最下面「诊断」里关掉。")
                .setPositiveButton("知道了", null)
                .setNegativeButton("关掉上报", (d, w) -> toggleUpload())
                .show();
    }

    // ---------------- 发送 ----------------

    void handleIntent(Intent in) {
        if (in == null) return;
        if (Intent.ACTION_VIEW.equals(in.getAction()) && in.getData() != null) { // 相机扫码打开的 p2pcp://join/…
            String code = in.getData().toString();
            in.setAction(null);
            new AlertDialog.Builder(this)
                    .setMessage("用这个邀请码加好友？")
                    .setPositiveButton("加好友", (d, w) -> join(code))
                    .setNegativeButton("取消", null)
                    .show();
            return;
        }
        List<Uri> uris = new ArrayList<>();
        if (Intent.ACTION_SEND.equals(in.getAction())) {
            Uri u = in.getParcelableExtra(Intent.EXTRA_STREAM, Uri.class);
            if (u != null) uris.add(u);
        } else if (Intent.ACTION_SEND_MULTIPLE.equals(in.getAction())) {
            ArrayList<Uri> l = in.getParcelableArrayListExtra(Intent.EXTRA_STREAM, Uri.class);
            if (l != null) uris.addAll(l);
        }
        in.setAction(null); // 转屏重建时别再发一遍
        if (!uris.isEmpty()) bg(() -> {
            refreshFriends(); // 分享直接拉起 App 时好友列表可能还没读出来
            post(() -> chooseFriend(f -> sendUris(uris, f)));
        });
    }

    void chooseFriend(Consumer<String> then) {
        if (friends.isEmpty()) {
            toast("先加好友");
        } else if (friends.size() == 1) {
            then.accept(friends.get(0));
        } else {
            String[] names = friends.toArray(new String[0]);
            new AlertDialog.Builder(this).setTitle("发给谁").setItems(names, (d, w) -> then.accept(names[w])).show();
        }
    }

    void pickFor(String friend) {
        pendingFriend = friend;
        Intent i = new Intent(Intent.ACTION_OPEN_DOCUMENT);
        i.addCategory(Intent.CATEGORY_OPENABLE);
        i.setType("*/*");
        i.putExtra(Intent.EXTRA_ALLOW_MULTIPLE, true);
        startActivityForResult(i, 1);
    }

    @Override
    protected void onActivityResult(int req, int res, Intent data) {
        if (req != 1 || res != RESULT_OK || data == null || pendingFriend == null) return;
        List<Uri> uris = new ArrayList<>();
        if (data.getClipData() != null) {
            for (int i = 0; i < data.getClipData().getItemCount(); i++) uris.add(data.getClipData().getItemAt(i).getUri());
        } else if (data.getData() != null) {
            uris.add(data.getData());
        }
        sendUris(uris, pendingFriend);
    }

    void sendUris(List<Uri> uris, String friend) {
        sendAborted = false;
        sendPanel.showCancel(true);
        bg(() -> {
            File outbox = new File(getCacheDir(), "outbox");
            outbox.mkdirs();
            // p2pcp 要真实路径（多条连接各读一段），所以先从 content:// 复制到缓存，本机复制很快。
            // 全部复制完再一次交给 p2pcp：一次打洞发完所有文件，中途断了它自己重连续传
            List<String> args = new ArrayList<>(List.of("send", "-to", friend));
            List<File> tmps = new ArrayList<>();
            for (Uri u : uris) {
                if (sendAborted) break;
                String name = displayName(u);
                post(() -> {
                    sendPanel.state.setText("准备 " + name + "（发给 " + friend + "）");
                    sendPanel.state.setTextColor(FG);
                    sendPanel.bar.setProgress(0);
                    sendPanel.detail.setText("");
                });
                File tmp = new File(outbox, name);
                try (InputStream is = getContentResolver().openInputStream(u); OutputStream os = new FileOutputStream(tmp)) {
                    byte[] buf = new byte[1 << 20];
                    int n;
                    while (!sendAborted && (n = is.read(buf)) > 0) os.write(buf, 0, n);
                } catch (Exception e) {
                    post(() -> sendPanel.handle("@ERROR 读取文件失败：" + e.getMessage()));
                    continue;
                }
                if (sendAborted) {
                    tmp.delete();
                    break;
                }
                tmps.add(tmp);
                args.add(tmp.getAbsolutePath());
            }
            if (sendAborted) {
                post(() -> sendPanel.handle("@CANCELED me 已取消"));
            } else if (!tmps.isEmpty()) {
                P2p.run(this, l -> post(() -> sendPanel.handle(l)), this::log, p -> {
                    sendProc = p;
                    if (sendAborted) P2p.command(p, "cancel"); // 进程刚起来时点的取消
                }, args.toArray(new String[0]));
                sendProc = null;
            }
            for (File f : tmps) f.delete();
            post(() -> sendPanel.showCancel(false));
        });
    }

    // ---------------- 小工具 ----------------

    void log(String l) {
        post(() -> appendLog(l));
    }

    void appendLog(String l) {
        // 日志行形如 "08:36:33.859654 [0] 打通 ..."，去掉微秒；只留最后 14 行
        String s = l.length() > 16 && l.charAt(8) == '.' ? l.substring(0, 8) + l.substring(15) : l;
        String[] lines = (logView.getText() + s + "\n").split("\n");
        StringBuilder sb = new StringBuilder();
        for (int i = Math.max(0, lines.length - 14); i < lines.length; i++) sb.append(lines[i]).append('\n');
        logView.setText(sb);
    }

    void ask(String title, String init, Consumer<String> then) {
        EditText in = new EditText(this);
        in.setText(init);
        in.setSelectAllOnFocus(true);
        new AlertDialog.Builder(this)
                .setTitle(title)
                .setView(in)
                .setPositiveButton("确定", (d, w) -> {
                    String v = in.getText().toString().trim();
                    if (!v.isEmpty()) then.accept(v);
                })
                .setNegativeButton("取消", null)
                .show();
    }

    String displayName(Uri u) {
        try (Cursor c = getContentResolver().query(u, new String[]{OpenableColumns.DISPLAY_NAME}, null, null, null)) {
            if (c != null && c.moveToFirst()) return c.getString(0).replace('/', '_');
        } catch (Exception ignored) {
        }
        return "file-" + System.currentTimeMillis();
    }

    void bg(Runnable r) {
        new Thread(r).start();
    }

    void post(Runnable r) {
        ui.post(r);
    }

    void toast(String s) {
        Toast.makeText(this, s, Toast.LENGTH_SHORT).show();
    }

    void toast2(String s) {
        post(() -> Toast.makeText(this, s, Toast.LENGTH_LONG).show());
    }

    LinearLayout card(LinearLayout parent) {
        LinearLayout card = new LinearLayout(this);
        card.setOrientation(LinearLayout.VERTICAL);
        card.setPadding(dp(18), dp(14), dp(18), dp(14));
        GradientDrawable bg = new GradientDrawable();
        bg.setColor(CARD);
        bg.setCornerRadius(dp(14));
        card.setBackground(bg);
        parent.addView(card, lpTop(14));
        return card;
    }

    Button button(String s, int color, android.view.View.OnClickListener l) {
        Button b = new Button(this);
        b.setText(s);
        b.setTextSize(16);
        b.setTextColor(BG);
        b.setAllCaps(false);
        GradientDrawable d = new GradientDrawable();
        d.setColor(color);
        d.setCornerRadius(dp(14));
        b.setBackground(d);
        b.setOnClickListener(l);
        return b;
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
