// config validate 单元测试（CP1-6）：启动校验早失败——bbolt 路径/blob_dir/max_bytes/type。
package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate_PathEmpty(t *testing.T) {
	cfg := defaults()
	cfg.Store.Path = ""
	if err := validate(&cfg); err == nil || !strings.Contains(err.Error(), "path") {
		t.Errorf("empty path should fail: got %v", err)
	}
}

func TestValidate_MaxBytesZero(t *testing.T) {
	cfg := defaults()
	cfg.Store.MaxBytes = 0
	if err := validate(&cfg); err == nil || !strings.Contains(err.Error(), "max_bytes") {
		t.Errorf("max_bytes=0 should fail: got %v", err)
	}
}

func TestValidate_BlobDirEmpty(t *testing.T) {
	cfg := defaults()
	cfg.Store.BlobDir = ""
	if err := validate(&cfg); err == nil || !strings.Contains(err.Error(), "blob_dir") {
		t.Errorf("empty blob_dir should fail: got %v", err)
	}
}

func TestValidate_TypeInvalid(t *testing.T) {
	cfg := defaults()
	cfg.Store.Type = "sqlite" // 已废弃（旧 TODO 值），应拒
	if err := validate(&cfg); err == nil || !strings.Contains(err.Error(), "type") {
		t.Errorf("bad type should fail: got %v", err)
	}
}

func TestValidate_DefaultsOK(t *testing.T) {
	// 默认配置应通过校验（bbolt path + blob_dir 指向临时可写目录）
	cfg := defaults()
	cfg.Store.Path = filepath.Join(t.TempDir(), "test.db")
	cfg.Store.BlobDir = t.TempDir()
	if err := validate(&cfg); err != nil {
		t.Errorf("valid config should pass: %v", err)
	}
}

func TestValidate_MemorySkipsPathCheck(t *testing.T) {
	// memory 模式不校验 path（调试用，无 db 文件）
	cfg := defaults()
	cfg.Store.Type = "memory"
	cfg.Store.Path = ""
	cfg.Store.BlobDir = t.TempDir()
	if err := validate(&cfg); err != nil {
		t.Errorf("memory mode should skip path check: %v", err)
	}
}
