// HotifyServer 入口：自建 Go 后端（对标 bark-server）——bark 协议入口 + 自有 /api/v1 内核 + 华为 Push Kit 推送。
// 取代 legacy Python 桥 + Gotify broker（docs/NEXT-Server.md §2：Server 吸收 Gotify 角色）。
// 架构/决策详见 ARCHITECTURE.md + ../docs/NEXT-Server.md。
package main

import (
	"log"
	"os"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/config"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/pushkit"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/server"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/store"
)

func main() {
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

	// 启动确保 key2 已生成（docs §9：server 启动生成 key2，设备构造 share URL 用）
	// key1 不预生成，走空起始 first-set（首设备 register 触发，CP2 实装）
	if _, err := st.EnsureKey2(); err != nil {
		log.Fatalf("ensure key2: %v", err)
	}

	pk := pushkit.New(cfg.PushKit)
	srv := server.New(cfg, st, pk)
	log.Printf("HotifyServer listening on %s (store=%s, pushkit=%t)",
		cfg.Server.Addr, cfg.Store.Type, cfg.PushKit.ProjectID != "")
	if err := srv.Run(); err != nil {
		log.Fatalf("server: %v", err)
	}
}
