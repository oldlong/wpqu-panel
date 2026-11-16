// 启动期杂务：数据目录解析、日志落盘、JSON 小工具。
package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// decodeJSONObject 读一个 JSON 对象（调用方负责关 Body）。
func decodeJSONObject(resp *http.Response) (map[string]any, bool) {
	var m map[string]any
	if resp == nil || resp.Body == nil {
		return nil, false
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err := dec.Decode(&m); err != nil {
		return nil, false
	}
	return m, true
}

// ---------------------------------------------------------------- 数据目录

// resolveDataDir 决定用哪份档案，必要时把可执行文件旁边的旧档案搬过来。
//
// 优先级的用意：exe 放在哪都能看到同一批服务器。
// 以前数据固定跟 exe 走，把 exe 换个目录（比如从 panel-go 拷到 dist）
// 就等于换了个空面板，用户看到的就是「一台服务器都没有 / 连不上」。
func resolveDataDir(flagVal string) (string, []string) {
	var notes []string

	if flagVal != "" {
		return absPath(flagVal), []string{"数据目录由 -data / PANEL_DATA 指定"}
	}

	userDir := userDataDir()
	exeDir := exeDataDir()

	if hasServersFile(userDir) {
		return userDir, notes
	}
	// 改名前的目录（wp-panel）里如果有档案，整份复制过来。
	// 只复制不删除，出问题还能退回旧目录。
	if legacy := legacyUserDataDir(); legacy != "" && hasServersFile(legacy) && !samePath(legacy, userDir) {
		if err := copyDataDir(legacy, userDir); err == nil {
			notes = append(notes, "已把旧数据目录 "+legacy+" 迁移到 "+userDir)
			return userDir, notes
		}
	}
	if hasServersFile(exeDir) && !samePath(userDir, exeDir) {
		if err := copyDataDir(exeDir, userDir); err == nil {
			notes = append(notes, "已把 "+exeDir+" 的档案迁移到 "+userDir)
			return userDir, notes
		} else {
			notes = append(notes, "迁移档案失败（"+err.Error()+"），继续使用 "+exeDir)
			return exeDir, notes
		}
	}
	if hasServersFile(exeDir) {
		return exeDir, notes
	}
	return userDir, notes
}

func hasServersFile(dir string) bool {
	if dir == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(dir, "servers.json"))
	return err == nil && !st.IsDir()
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func exeDataDir() string {
	exe, err := os.Executable()
	if err != nil {
		if wd, err2 := os.Getwd(); err2 == nil {
			return filepath.Join(wd, "data")
		}
		return "data"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Join(filepath.Dir(exe), "data")
}

// 品牌改名后数据目录也跟着改；旧目录保留一版用于迁移。
const (
	dataBrand   = "wpqu-panel"
	legacyBrand = "wp-panel"
)

// userDataDir 是固定的档案位置，跨 exe 位置稳定。
func userDataDir() string {
	return configBaseDir(dataBrand)
}

// legacyUserDataDir 是改名前的目录（%APPDATA%\wp-panel\data）。
func legacyUserDataDir() string {
	return configBaseDir(legacyBrand)
}

func configBaseDir(brand string) string {
	if runtime.GOOS == "windows" {
		base := os.Getenv("APPDATA")
		if base == "" {
			base = os.Getenv("LOCALAPPDATA")
		}
		if base == "" {
			if home, err := os.UserHomeDir(); err == nil {
				base = filepath.Join(home, "AppData", "Roaming")
			}
		}
		if base != "" {
			return filepath.Join(base, brand, "data")
		}
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, ".config")
		}
	}
	if base == "" {
		return "data"
	}
	return filepath.Join(base, brand, "data")
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// copyDataDir 复制档案到新位置（servers.json / secret.key / 授权码 / logs / backups / plugins）。
// 只复制，不删除源目录——旧位置仍可用，出问题能退回。
func copyDataDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	for _, name := range []string{"servers.json", "secret.key", "licenses.json"} {
		from := filepath.Join(src, name)
		data, err := os.ReadFile(from)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, name), data, 0o600); err != nil {
			return err
		}
	}
	for _, sub := range []string{"logs", "backups", "plugins"} {
		from := filepath.Join(src, sub)
		if st, err := os.Stat(from); err != nil || !st.IsDir() {
			continue
		}
		if err := copyTree(from, filepath.Join(dst, sub)); err != nil {
			return err
		}
	}
	return nil
}

func copyTree(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	for _, e := range entries {
		from := filepath.Join(src, e.Name())
		to := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyTree(from, to); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		if err := os.WriteFile(to, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- 日志

// setupLog 把日志同时写到 stderr 和 data/panel.log。
// 无控制台构建（windowsgui）下 stderr 是空的，只能靠文件排查问题。
func setupLog(dataDir string) {
	log.SetFlags(log.LstdFlags)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return
	}
	path := filepath.Join(dataDir, "panel.log")
	// 超过 2MB 就滚一份，避免长期双击把文件撑大
	if st, err := os.Stat(path); err == nil && st.Size() > 2<<20 {
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
}
