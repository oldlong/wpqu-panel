// Package updater：面板自升级。
//
// 三个阶段严格分开，任何一步失败都不动现有二进制：
//  1. Check   拉 updates.json，验签，挑出比当前版本新的那一版
//  2. Stage   下载到临时目录，校验 SHA256，再放成待替换文件
//  3. Restart 替换自身并重启（见 apply_windows.go / apply_other.go）
package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"wpqu-panel/internal/signing"
	"wpqu-panel/internal/version"
)

// DefaultBase 更新源。个人搭建时可用 WPQU_UPDATE_BASE 覆盖（也方便本地联调）。
const DefaultBase = "https://panel.wpqu.com"

// maxPackage 升级包大小上限，防止被喂一个巨大的文件把磁盘写满。
const maxPackage = 300 << 20

// Client 拉清单 / 下载。
type Client struct {
	Base    string
	Current string
	HTTP    *http.Client
	DataDir string // 暂存目录
	// PublicKey 留空则用内置发布公钥；内网自建更新源时可以指定自己的公钥
	PublicKey string
}

func New(dataDir, current string) *Client {
	base := strings.TrimRight(os.Getenv("WPQU_UPDATE_BASE"), "/")
	if base == "" {
		base = DefaultBase
	}
	return &Client{
		Base:    base,
		Current: current,
		DataDir: dataDir,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// ---------------------------------------------------------------- 清单结构

type Manifest struct {
	Schema       int       `json:"schema"`
	Channel      string    `json:"channel"`
	Latest       string    `json:"latest"`
	MinSupported string    `json:"minSupported"`
	Releases     []Release `json:"releases"`
}

type Release struct {
	Version  string              `json:"version"`
	Date     string              `json:"date"`
	Critical bool                `json:"critical"`
	Notes    map[string][]string `json:"notes"`
	Files    []Package           `json:"files"`
}

type Package struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// CheckResult 给前端用。
type CheckResult struct {
	Available    bool                `json:"available"`
	Current      string              `json:"current"`
	Latest       string              `json:"latest"`
	Critical     bool                `json:"critical"`
	Date         string              `json:"date"`
	Notes        map[string][]string `json:"notes"`
	Size         int64               `json:"size"`
	Platform     string              `json:"platform"`
	DownloadURL  string              `json:"downloadUrl"`
	SHA256       string              `json:"sha256"`
	MinSupported string              `json:"minSupported"`
	// Skipped 说明为什么没有可用的包（平台不支持 / 未签名构建 / 已是最新）
	Skipped string `json:"skipped,omitempty"`
	// TooOld 表示跨版本过大，只能手动装（预留：MinSupported 高于当前版本）
	TooOld bool `json:"tooOld,omitempty"`
}

// ---------------------------------------------------------------- Check

// Check 拉取并校验清单，返回是否有更新。
func (c *Client) Check() (*CheckResult, error) {
	res := &CheckResult{Current: c.Current, Platform: runtime.GOOS + "/" + runtime.GOARCH}

	if !signing.HasReleaseKey() && c.PublicKey == "" {
		res.Skipped = "此构建未内置发布公钥，无法校验更新包"
		return res, nil
	}

	body, err := c.get("/api/updates.json")
	if err != nil {
		return nil, err
	}
	sig, err := c.get("/api/updates.json.sig")
	if err != nil {
		return nil, fmt.Errorf("取签名失败：%w", err)
	}
	if err := c.verify(body, string(sig)); err != nil {
		return nil, err
	}

	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("更新清单格式错误：%w", err)
	}

	// 从新到旧找第一个比当前新的版本
	var best *Release
	for i := range m.Releases {
		r := &m.Releases[i]
		if !version.Valid(r.Version) || !version.IsNewer(r.Version, c.Current) {
			continue
		}
		if best == nil || version.IsNewer(r.Version, best.Version) {
			best = r
		}
	}
	if best == nil {
		res.Latest = m.Latest
		res.Skipped = "已是最新版本"
		return res, nil
	}

	res.Available = true
	res.Latest = best.Version
	res.Critical = best.Critical
	res.Date = best.Date
	res.Notes = best.Notes
	res.MinSupported = m.MinSupported

	if m.MinSupported != "" && version.IsNewer(m.MinSupported, c.Current) {
		res.TooOld = true
		res.Skipped = "当前版本过旧，跨度过大，请手动下载安装最新版"
		return res, nil
	}

	pkg, ok := pickPackage(best, runtime.GOOS, runtime.GOARCH)
	if !ok {
		res.Available = false
		res.Skipped = fmt.Sprintf("v%s 没有适配 %s/%s 的安装包", best.Version, runtime.GOOS, runtime.GOARCH)
		return res, nil
	}
	res.Size = pkg.Size
	res.SHA256 = pkg.SHA256
	res.DownloadURL = absolute(c.Base, pkg.URL)
	return res, nil
}

func pickPackage(r *Release, goos, goarch string) (Package, bool) {
	arch := goarch
	switch goarch {
	case "386":
		arch = "386"
	case "arm64":
		arch = "arm64"
	case "amd64":
		arch = "amd64"
	}
	for _, p := range r.Files {
		if p.OS == goos && (p.Arch == arch || p.Arch == "all") {
			return p, true
		}
	}
	return Package{}, false
}

func absolute(base, u string) string {
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	if !strings.HasPrefix(u, "/") {
		u = "/" + u
	}
	return base + u
}

// ---------------------------------------------------------------- Stage

// Stage 下载升级包并校验，返回落地后的待替换文件路径。
// onProgress 收到 0..1 的进度与可读说明。
func (c *Client) Stage(r *CheckResult, onProgress func(pct float64, msg string)) (string, error) {
	if !r.Available || r.DownloadURL == "" {
		return "", errors.New("没有可用的升级包")
	}
	tmpDir := filepath.Join(c.DataDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return "", err
	}

	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	staged := filepath.Join(tmpDir, "wpqu-panel-"+r.Latest+ext)
	_ = os.Remove(staged) // 上次中断的残留
	_ = os.Remove(staged + ".part")

	if onProgress != nil {
		onProgress(0, "正在连接更新源…")
	}
	req, err := http.NewRequest("GET", r.DownloadURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "wpqu-panel/"+c.Current)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("下载失败：HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxPackage {
		return "", fmt.Errorf("升级包过大（%d MB）", resp.ContentLength>>20)
	}

	part := staged + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	var written int64
	buf := make([]byte, 64<<10)
	lastReport := time.Now()
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			written += int64(n)
			if written > maxPackage {
				f.Close()
				os.Remove(part)
				return "", errors.New("升级包超过大小上限，已中止")
			}
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(part)
				return "", werr
			}
			h.Write(buf[:n])
			if onProgress != nil && time.Since(lastReport) > 120*time.Millisecond {
				lastReport = time.Now()
				pct := 0.0
				if r.Size > 0 {
					pct = float64(written) / float64(r.Size)
					if pct > 0.99 {
						pct = 0.99
					}
				}
				onProgress(pct, fmt.Sprintf("已下载 %.1f MB", float64(written)/(1<<20)))
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(part)
			return "", fmt.Errorf("下载中断：%w", rerr)
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(part)
		return "", err
	}

	if onProgress != nil {
		onProgress(1, "正在校验完整性…")
	}
	got := hex.EncodeToString(h.Sum(nil))
	want := strings.ToLower(strings.TrimSpace(r.SHA256))
	if want == "" {
		os.Remove(part)
		return "", errors.New("清单里缺少 SHA256，拒绝安装")
	}
	if got != want {
		os.Remove(part)
		return "", fmt.Errorf("校验失败：期望 %s，实际 %s（文件可能损坏或被篡改）", short(want), short(got))
	}
	if err := os.Rename(part, staged); err != nil {
		os.Remove(part)
		return "", err
	}
	if onProgress != nil {
		onProgress(1, "校验通过")
	}
	return staged, nil
}

// Cleanup 删掉暂存的升级包（用户取消或安装失败后调用）。
func (c *Client) Cleanup() {
	dir := filepath.Join(c.DataDir, "tmp")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "wpqu-panel-") {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// ---------------------------------------------------------------- 工具

func (c *Client) get(path string) ([]byte, error) { return c.getURL(absolute(c.Base, path)) }

func (c *Client) verify(data []byte, sig string) error {
	if c.PublicKey != "" {
		return signing.VerifyWith(c.PublicKey, data, sig)
	}
	return signing.VerifyRelease(data, sig)
}

func (c *Client) getURL(u string) ([]byte, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "wpqu-panel/"+c.Current)
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接更新源失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, fmt.Errorf("更新源上找不到 %s", u)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("更新源返回 HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

// isDevBuild 判断当前进程是不是 go run 出来的临时二进制。
// 这种情况替换自身毫无意义，直接拒绝并给出说明。
func isDevBuild() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	exe = filepath.ToSlash(exe)
	return strings.Contains(exe, "/go-build") || strings.HasPrefix(filepath.Base(exe), "__debug")
}

// CanSelfUpdate 告诉前端能不能自动替换（开发态 / 权限不足时降级为手动下载）。
func CanSelfUpdate() (bool, string) {
	if isDevBuild() {
		return false, "当前是开发模式（go run）启动的，无法自动替换，请手动下载"
	}
	exe, err := os.Executable()
	if err != nil {
		return false, "无法定位当前程序路径：" + err.Error()
	}
	dir := filepath.Dir(exe)
	probe := filepath.Join(dir, ".wpqu-write-test")
	if err := os.WriteFile(probe, []byte("1"), 0o600); err != nil {
		return false, "程序所在目录不可写（" + dir + "），请用管理员权限运行，或手动下载替换"
	}
	_ = os.Remove(probe)
	return true, ""
}

// Log 记录升级动作，出问题时能回看。
func Log(dataDir, format string, a ...any) {
	f, err := os.OpenFile(filepath.Join(dataDir, "update.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf(format, a...)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
}
