// CyIME Sync —— Windows 剪贴板同步电脑端。
//
// 单文件 exe、无安装器、无需管理员权限；托盘常驻，双击打开内置设置页。
// 所有运行期数据（配置、证书、日志）都在 exe 同级的 data/ 目录内。
package main

import (
	"fmt"
	"log"
	"net"
	"os"

	"cyime-sync/internal/appdir"
	"cyime-sync/internal/clipboard"
	"cyime-sync/internal/config"
	"cyime-sync/internal/logging"
	"cyime-sync/internal/pairing"
	"cyime-sync/internal/server"
	"cyime-sync/internal/syncer"
	"cyime-sync/internal/tray"
	"cyime-sync/internal/winutil"
)

// version 用于日志与排障时对照。
const version = "1.5.0"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("cyime-sync ")

	winutil.SetProcessDpiAware()

	root, err := appdir.Root()
	if err != nil {
		fatal("无法定位数据目录", err)
	}
	logDir, err := appdir.Sub(root, appdir.LogDirName)
	if err != nil {
		fatal("无法创建日志目录", err)
	}
	writer, err := logging.Open(logDir, logging.DefaultOptions())
	if err != nil {
		fatal("无法创建日志文件", err)
	}
	log.SetOutput(writer)
	stopJanitor := writer.Janitor(logging.DefaultCleanInterval)

	code := 0
	// 单实例：已在运行时唤起已有实例的设置页后直接退出。
	if running, err := winutil.AcquireSingleInstance(); err != nil {
		fatal("启动失败", err)
	} else if running {
		if err := winutil.NotifyExistingInstance(); err != nil {
			winutil.MessageBox("CyIME 剪贴板同步",
				"程序已在运行，但无法唤起设置页。\n请双击通知区域图标打开。\n\n详情："+err.Error())
		}
	} else {
		code = serve(root)
	}

	stopJanitor()
	_ = writer.Close()
	os.Exit(code)
}

// fatal 在图形界面下弹窗提示（windowsgui 子系统没有控制台输出）。
func fatal(title string, err error) {
	log.Printf("%s: %v", title, err)
	winutil.MessageBox("CyIME 剪贴板同步", title+"：\n"+err.Error())
	os.Exit(1)
}

func serve(root string) int {
	log.Printf("版本: %s", version)
	log.Printf("数据目录: %s", root)
	if msg, err := appdir.MigrateLegacy(root); err != nil {
		log.Printf("迁移旧数据失败: %v", err)
	} else if msg != "" {
		log.Print(msg)
	}

	store, err := config.Open(root)
	if err != nil {
		// 可恢复问题（文件损坏已重建、字段被修复），记录但不阻塞启动。
		log.Printf("配置提示: %v", err)
	}
	cfg := store.Get()
	log.Printf("配置路径: %s", store.Path())

	// 让注册表与配置保持一致（换过目录/移动过 exe 时自启动项需要更新）。
	if cfg.AutoStart {
		if err := winutil.SetAutoStart(true); err != nil {
			log.Printf("写入开机自启动失败: %v", err)
		}
	}

	clip, err := clipboard.New()
	if err != nil {
		fatal("剪贴板不可用", err)
	}

	ring := syncer.NewRing(200)
	engine := syncer.New(clip, ring)
	engine.SetPushLimitFunc(func() int { return store.Get().MaxPushChars })
	engine.SetForceLimitFunc(func() int { return store.Get().ForcePushChars })
	engine.SetSkipGuessFunc(func() bool { return store.Get().SkipSuspectedOTP })
	engine.SetEnabled(cfg.Enabled)
	engine.Start()
	defer engine.Stop()

	// 配对是唯一的连接方式（扫码 + TLS + 逐设备令牌），因此初始化失败属于致命错误：
	// 没有证书就无法与手机建立信任，继续运行只会让用户看到"一直连不上"。
	tlsDir, err := appdir.Sub(root, appdir.TLSDirName)
	if err != nil {
		fatal("无法创建证书目录", err)
	}
	pm, err := pairing.New(store, tlsDir)
	if err != nil {
		fatal("初始化配对证书失败", err)
	}
	log.Printf("证书指纹: %s", pm.Fingerprint())

	mgr, err := server.New(server.Options{
		Config:         store,
		Engine:         engine,
		Version:        version,
		Pairing:        pm,
		ApplyAutoStart: winutil.SetAutoStart,
		Logf:           log.Printf,
	})
	if err != nil {
		fatal("初始化 HTTP 服务失败", err)
	}
	if err := mgr.Start(); err != nil {
		// 端口占用等情况下不退出进程：托盘仍可用，用户可改端口。
		log.Printf("HTTP 服务启动失败: %v", err)
	}
	defer mgr.Close()

	openSettings := func() {
		addr := mgr.Addr()
		if addr == "" {
			addr = store.Get().Listen()
		}
		if err := winutil.OpenURL("http://" + browserHost(addr)); err != nil {
			winutil.MessageBox("无法打开设置页", err.Error())
		}
	}

	err = tray.Run(tray.Options{
		Tooltip: func() string {
			c := store.Get()
			state := "同步中"
			if !c.Enabled {
				state = "已停用"
			}
			return fmt.Sprintf("CyIME 剪贴板同步 · %s · 端口 %d", state, c.Port)
		},
		OnOpenSettings: openSettings,
		OnForcePush: func() {
			res, err := engine.ForcePush()
			if err != nil {
				log.Printf("强制推送失败: %v", err)
				return
			}
			log.Printf("强制推送: %s", res.Status)
		},
	})
	if err != nil {
		log.Printf("托盘启动失败: %v", err)
		winutil.MessageBox("CyIME 剪贴板同步", "托盘图标创建失败：\n"+err.Error())
		return 1
	}

	log.Printf("正在退出")
	return 0
}

// browserHost 把通配监听地址换成本机回环地址，供浏览器打开设置页。
func browserHost(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if host == "0.0.0.0" || host == "::" || host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
