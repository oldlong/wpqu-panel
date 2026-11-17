// Package admin：panel.wpqu.com 的管理后台。
//
// 它只做两件事：改分发站的内容，以及按插件发授权码。两件事都要碰私钥，
// 所以整个服务默认只监听 127.0.0.1，不对外暴露 —— 要远程用就 SSH 端口转发，
// 或者用 Caddy 反代再加一层认证，配置见 docs/管理后台说明.md。
package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config 是管理后台的全部可变配置，存成一个 JSON 文件。
type Config struct {
	// Listen 监听地址。默认只监听回环，改之前先想清楚谁能访问到私钥。
	Listen string `json:"listen"`
	// SiteDir 分发站目录（panel.wpqu.com 的根）。
	SiteDir string `json:"siteDir"`
	// SrcDir 插件源码目录。
	SrcDir string `json:"srcDir"`
	// DistDir 交叉编译产物目录，发布升级时从这里取二进制。
	DistDir string `json:"distDir"`
	// KeysDir 存放 release.key / license.key 的目录。**不要**放进 SiteDir 里对外可访问的子目录。
	KeysDir string `json:"keysDir"`
	// DataDir 口令、发码台账、审计日志、文件备份都放这里。
	DataDir string `json:"dataDir"`
	// SessionHours 登录态有效期（滑动过期），默认 12 小时。
	SessionHours int `json:"sessionHours"`
	// BehindProxy 为真时信任 X-Forwarded-Proto，用于反代后把 Cookie 标成 Secure。
	BehindProxy bool `json:"behindProxy"`
}

// DefaultConfig 返回一套以「仓库根目录」为基准的默认配置。
func DefaultConfig() Config {
	return Config{
		Listen:       "127.0.0.1:7790",
		SiteDir:      "../site",
		SrcDir:       "../plugins-src",
		DistDir:      "../dist",
		KeysDir:      "../site/.keys",
		DataDir:      "./admin-data",
		SessionHours: 12,
	}
}

// Keep 是 Locate 的「这几项别动」位掩码：命令行显式指定的目录优先级最高，
// 哪怕它看起来不像分发站，也不该被探测结果覆盖。
type Keep int

const (
	KeepSite Keep = 1 << iota
	KeepSrc
	KeepDist
	KeepKeys
)

// Locate 在配置里的目录明显不对时，按可执行文件所在位置重新找一套。
//
// 双击启动时工作目录就是 exe 所在目录（比如 dist/admin），配置里那句
// `../site` 就会指向 dist/site —— 一个不存在的地方，然后启动失败，
// 而控制台窗口在用户看清报错之前已经关了。所以这里做一次探测：
// 从 exe 目录往上找「看起来像分发站」的目录，再找它旁边的 plugins-src 与 dist。
//
// 用户自己写死的路径只要指向真实存在的目录就不动，探测只是兜底；
// keep 里的项更是完全不碰。
// 返回一句句人话，调用方打出来，让用户知道实际用的是哪个目录。
func (c *Config) Locate(exeDir string, keep Keep) []string {
	var notes []string
	replace := func(label string, dst *string, nv string) {
		if nv == "" || nv == *dst {
			return
		}
		notes = append(notes, fmt.Sprintf("%s：%s → %s", label, *dst, nv))
		*dst = nv
	}

	if keep&KeepSite == 0 && !IsSiteDir(c.SiteDir) {
		if d, ok := FindSiteDir(exeDir); ok {
			replace("站点", &c.SiteDir, d)
		}
	}
	// 源码与产物通常是分发站的兄弟目录，所以从分发站往上一级开始找，
	// 找不到再退回 exe 目录那条线。
	base := filepath.Dir(c.SiteDir)
	if keep&KeepSrc == 0 && !IsSrcDir(c.SrcDir) {
		if d, ok := findNear(base, "plugins-src", IsSrcDir); ok {
			replace("插件源", &c.SrcDir, d)
		} else if d, ok := findNear(exeDir, "plugins-src", IsSrcDir); ok {
			replace("插件源", &c.SrcDir, d)
		}
	}
	if keep&KeepDist == 0 && !IsDistDir(c.DistDir) {
		if d, ok := findNear(base, "dist", IsDistDir); ok {
			replace("构建", &c.DistDir, d)
		} else if d, ok := findNear(exeDir, "dist", IsDistDir); ok {
			replace("构建", &c.DistDir, d)
		}
	}
	// 私钥跟分发站走：keys 目录不存在时按 siteDir/.keys 推一次
	if keep&KeepKeys == 0 && !isDir(c.KeysDir) {
		if k := filepath.Join(c.SiteDir, ".keys"); isDir(k) {
			replace("私钥", &c.KeysDir, k)
		}
	}
	return notes
}

// IsSiteDir 判断一个目录像不像分发站根：有插件索引，或者既有首页又有插件包目录。
func IsSiteDir(p string) bool {
	if !isDir(p) {
		return false
	}
	if fileExists(filepath.Join(p, "api", "plugins", "index.json")) {
		return true
	}
	return fileExists(filepath.Join(p, "index.html")) && isDir(filepath.Join(p, "plugins"))
}

// IsSrcDir 判断是不是插件源码根：至少有一个子目录里带 plugin.json。
func IsSrcDir(p string) bool {
	if !isDir(p) {
		return false
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if e.IsDir() && fileExists(filepath.Join(p, e.Name(), "plugin.json")) {
			return true
		}
	}
	return false
}

// IsDistDir 判断是不是构建产物目录：有三个平台的产物，或者有 admin/ 子目录。
func IsDistDir(p string) bool {
	if !isDir(p) {
		return false
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		return false
	}
	n := 0
	for _, e := range ents {
		name := e.Name()
		if name == "admin" && e.IsDir() {
			return true
		}
		if strings.HasPrefix(name, "wpqu-panel-") {
			n++
		}
	}
	return n > 0
}

// FindSiteDir 从 start 开始向上找分发站根，最多看五层。
// 既要认 start 本身，也要认 start/site —— exe 放在仓库里时属于后者。
func FindSiteDir(start string) (string, bool) {
	for i, dir := 0, start; i < 6; i++ {
		if IsSiteDir(dir) {
			return dir, true
		}
		if s := filepath.Join(dir, "site"); IsSiteDir(s) {
			return s, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}

func findNear(start, name string, pred func(string) bool) (string, bool) {
	for i, dir := 0, start; i < 6; i++ {
		if p := filepath.Join(dir, name); pred(p) {
			return p, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// LoadConfig 读取配置。文件不存在时返回默认配置与 created=true。
//
// 不在读的时候顺手写文件：命令行参数（-site、-keys 等）是在这之后才覆盖的，
// 先写一遍会把「默认值」固化下来，用户以为 -site 生效了，实际下次启动读到的是别的目录。
// 由调用方在覆盖完成后调用 Save。
//
// 相对路径一律相对于配置文件所在目录解析，这样用 systemd 起服务时
// 不依赖 WorkingDirectory 是否写对 —— 那是排查起来很费劲的一类问题。
func LoadConfig(path string) (Config, bool, error) {
	cfg := DefaultConfig()
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		cfg.resolveFrom(filepath.Dir(path))
		return cfg, true, nil
	case err != nil:
		return cfg, false, fmt.Errorf("读配置失败：%w", err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, false, fmt.Errorf("配置文件不是合法 JSON：%w", err)
	}
	cfg.resolveFrom(filepath.Dir(path))
	return cfg, false, nil
}

// Save 写回配置文件（含默认注释性字段，便于手工编辑）。
func (c Config) Save(path string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	body, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o600)
}

// resolveFrom 把相对路径转成绝对路径。
//
// base 必须先绝对化：否则用相对路径指定 -config 时，写进文件的仍然是
// 相对路径，下次启动再按配置文件目录解析一次，就会变成 a/tmp/b/tmp/c。
func (c *Config) resolveFrom(base string) {
	if abs, err := filepath.Abs(base); err == nil {
		base = abs
	}
	// 配置里把某一项写成空串（手改时很容易把整行删掉只留 ""），
	// 就按默认值补齐：空路径后面会在 mkdir 那里报一句没人看得懂的错。
	def := DefaultConfig()
	if strings.TrimSpace(c.SiteDir) == "" {
		c.SiteDir = def.SiteDir
	}
	if strings.TrimSpace(c.SrcDir) == "" {
		c.SrcDir = def.SrcDir
	}
	if strings.TrimSpace(c.DistDir) == "" {
		c.DistDir = def.DistDir
	}
	if strings.TrimSpace(c.KeysDir) == "" {
		c.KeysDir = def.KeysDir
	}
	if strings.TrimSpace(c.DataDir) == "" {
		c.DataDir = def.DataDir
	}
	// 绝对路径也要 Clean：手改配置时写 `C:/Users/…` 或 `C://Users//…` 很常见，
	// 不归一化的话 Windows 上会在显示和拼接时出现 `C://Users//` 这种怪样子。
	abs := func(p string) string {
		if p == "" {
			return p
		}
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
		return filepath.Clean(filepath.Join(base, p))
	}
	c.SiteDir = abs(c.SiteDir)
	c.SrcDir = abs(c.SrcDir)
	c.DistDir = abs(c.DistDir)
	c.KeysDir = abs(c.KeysDir)
	c.DataDir = abs(c.DataDir)
	if c.SessionHours <= 0 {
		c.SessionHours = 12
	}
	if strings.TrimSpace(c.Listen) == "" {
		c.Listen = "127.0.0.1:7790"
	}
}

// keyPath 返回某个私钥的完整路径。
func (c Config) keyPath(name string) string {
	return filepath.Join(c.KeysDir, name+".key")
}
