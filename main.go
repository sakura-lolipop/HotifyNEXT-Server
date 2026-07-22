// HotifyServer 入口：自建 Go 后端（对标 bark-server）——bark 协议入口 + 自有 /api/v1 内核 + 华为 Push Kit 推送。
// 取代 legacy Python 桥 + Gotify broker（docs/NEXT-Server.md §2：Server 吸收 Gotify 角色）。
// 架构/决策详见 ARCHITECTURE.md + ../docs/NEXT-Server.md。
package main

import (
	"io"
	"log"
	"os"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/config"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/pushkit"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/server"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/store"
)

func main() {
	// 日志同时写 stdout（shell 实时 / systemd journal）+ hotify.log（持久保存，公网排障）。
	// hotify.log 在 cwd（HotifyNEXT-Server/），*.log 已 .gitignore 挡。路径固定（config.log_file 留 Phase 2 如需多日志）。
	logFile, err := os.OpenFile("hotify.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatalf("open hotify.log: %v", err)
	}
	defer logFile.Close() // graceful shutdown（CP6 SIGTERM）时关
	log.SetOutput(io.MultiWriter(os.Stdout, logFile))

	cfg, err := config.Load("config.json")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// 媒体 blob 目录（docs §4b：blob 走文件系统，bbolt 只存 metadata）
	if err := os.MkdirAll(cfg.Store.BlobDir, 0o755); err != nil {
		log.Fatalf("mkdir blob_dir %s: %v", cfg.Store.BlobDir, err)
	}

	// 存储层装配：bbolt 持久化（默认）/ memory 调试（重启丢）
	var st store.Store
	switch cfg.Store.Type {
	case "bbolt":
		bb, err := store.NewBBolt(cfg.Store.Path)
		if err != nil {
			// bbolt Open 失败常见：路径不可写 / 已有实例持锁（bbolt 单进程独占文件锁）
			log.Fatalf("open bbolt %s: %v (path not writable, or another instance already running?)",
				cfg.Store.Path, err)
		}
		defer bb.Close() // 优雅关闭 db 句柄（graceful shutdown 在 CP6 加 SIGTERM 处理）
		st = bb
	case "memory":
		st = store.NewMemory()
		log.Printf("[WARN] store=memory (debug only, restart loses all data)")
	default:
		log.Fatalf("unknown store.type %q", cfg.Store.Type)
	}

	// FIFO 空间阈值清理未实装（CP3c 跨审 D P2：config/ARCHITECTURE 声明"空间阈值+FIFO"但 store 零实装，max_bytes 形同虚设）。
	// 公网部署 bark 写开放下磁盘可能被灌满——启动告警让运维知晓 max_bytes 当前 advisory（TD-13 Phase 2 实装）。
	if cfg.Store.Type == "bbolt" && cfg.Store.MaxBytes > 0 {
		log.Printf("[WARN] FIFO eviction not implemented (TD-13, Phase 2); max_bytes=%d advisory — "+
			"bark write-open can fill disk on public deploy", cfg.Store.MaxBytes)
	}

	// 启动确保 key2 已生成（docs §9：server 启动生成 key2，设备构造 share URL 用）
	// key1 不预生成，走空起始 first-set（首设备 register 触发，CP2 实装）
	if _, err := st.EnsureKey2(); err != nil {
		log.Fatalf("ensure key2: %v", err)
	}

	pusher := pushkit.New(cfg.PushKit)
	srv := server.New(cfg, st, pusher)
	log.Printf("HotifyServer listening on %s (store=%s, pushkit=%t)",
		cfg.Server.Addr, cfg.Store.Type, len(cfg.PushKit.CloudFunctionURLs) > 0)
	if err := srv.Run(); err != nil {
		log.Fatalf("server: %v", err)
	}
}
