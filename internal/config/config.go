// 配置加载：从 config.json 读（缺失文件用默认值，便于调试期无配置启动）。
// config.json 含云函数入口 URL + AUTH_TOKEN（CP4 云函数中转；私钥锁云函数不在 Server）→ .gitignore 挡，不入库。
// 模板见 ../../config.example.json。
//
// ⚠️ 改 config 需重启——无热更新（docs/NEXT-Server.md §8 管理走 CLI/SSH，重启生效）。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/pushkit"
)

type Config struct {
	Server  ServerConfig   `json:"server"`
	PushKit pushkit.Config `json:"pushkit"`
	Store   StoreConfig    `json:"store"`
}

type ServerConfig struct {
	Addr string `json:"addr"` // 监听地址，如 ":25240"
}

// StoreConfig 存储层配置（docs/NEXT-Server.md §4/§4b）。
//   - type: "bbolt"（默认，持久化）/ "memory"（调试，重启丢）
//   - path: bbolt db 文件路径
//   - blob_dir: 媒体 blob 文件系统目录（§4b，blob 走文件系统不进 bbolt）
//   - max_bytes: FIFO 空间阈值（§4，超了从最老 HLC 删）
type StoreConfig struct {
	Type     string `json:"type"`      // bbolt | memory
	Path     string `json:"path"`      // bbolt db 文件（默认 "hotify.db"）
	BlobDir  string `json:"blob_dir"`  // 媒体 blob 目录（默认 "./blobs/"）
	MaxBytes int64  `json:"max_bytes"` // FIFO 阈值字节（默认 1024MB）
}

func defaults() Config {
	return Config{
		Server: ServerConfig{Addr: ":25240"},
		Store: StoreConfig{
			Type:     "bbolt",
			Path:     "hotify.db",
			BlobDir:  "./blobs/",
			MaxBytes: 1024 * 1024 * 1024,
		},
	}
}

// Load 读 path；文件不存在则返默认值（调试期允许，PushKit 留空即只存不推）。
// 返回前必跑 validate（早失败：bbolt 路径不可写 / blob_dir 不可写 / max_bytes<=0 → 启动 fatal）。
// PushKit 配置校验留 CP4（pushkit 实装时），CP1 只校验存储层。
func Load(path string) (*Config, error) {
	cfg := defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if e := validate(&cfg); e != nil {
				return nil, fmt.Errorf("default config invalid: %w", e)
			}
			return &cfg, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("config invalid: %w", err)
	}
	return &cfg, nil
}

// validate 启动校验（早失败原则）。bbolt 模式校验 path 目录可写 + blob_dir 可写 + max_bytes>0。
func validate(cfg *Config) error {
	switch cfg.Store.Type {
	case "bbolt", "memory":
	default:
		return fmt.Errorf("store.type %q invalid (want bbolt|memory)", cfg.Store.Type)
	}
	if cfg.Store.Type == "bbolt" {
		if cfg.Store.Path == "" {
			return fmt.Errorf("store.path empty (bbolt requires path)")
		}
		if e := checkDirWritable(filepath.Dir(cfg.Store.Path)); e != nil {
			return fmt.Errorf("store.path dir: %w", e)
		}
		if cfg.Store.MaxBytes <= 0 {
			return fmt.Errorf("store.max_bytes must be > 0 (got %d)", cfg.Store.MaxBytes)
		}
	}
	if cfg.Store.BlobDir == "" {
		return fmt.Errorf("store.blob_dir empty")
	}
	if e := checkDirWritable(cfg.Store.BlobDir); e != nil {
		return fmt.Errorf("store.blob_dir: %w", e)
	}
	return nil
}

// checkDirWritable 校验目录可写（建临时文件探测）。
// 目录不存在不阻塞（main.go MkdirAll 建），递归校验父目录可写。
func checkDirWritable(dir string) error {
	if dir == "" || dir == "." {
		// 当前工作目录：直接建临时文件探测（filepath.Dir(".") 仍是 "."，避免无限递归）
		return probeWritable(".")
	}
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return checkDirWritable(filepath.Dir(dir)) // 目录不存在→校验父目录
		}
		return err
	}
	return probeWritable(dir)
}

// probeWritable 在 dir 建临时文件探测可写。
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".hotify-writable-check-*")
	if err != nil {
		return err
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	return nil
}
