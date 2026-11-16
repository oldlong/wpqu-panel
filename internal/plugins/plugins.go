package plugins

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"wpqu-panel/internal/signing"
)

// ---------------------------------------------------------------- 清单结构

// Manifest 描述一个插件。同一份结构既用于「服务器部署配方」（Install）
// 也用于「面板功能模块」（UI），前者必须有 Install，后者必须有 UI。
type Manifest struct {
	ID          string              `json:"id"`
	Name        map[string]string   `json:"name"`
	Summary     map[string]string   `json:"summary"`
	Description map[string]string   `json:"description"`
	Version     string              `json:"version"`
	Category    string              `json:"category"` // runtime | ai | dev | ops | panel
	Tier        string              `json:"tier"`     // free | paid
	PriceCNY    int                 `json:"priceCny"`
	Author      string              `json:"author"`
	Homepage    string              `json:"homepage"`
	Icon        string              `json:"icon"`   // emoji 或单字母
	Accent      string              `json:"accent"` // 卡片主色
	Highlights  map[string][]string `json:"highlights"`
	Requires    Requirements        `json:"requires"`
	Install     *InstallSpec        `json:"install"`
	UI          *UISpec             `json:"ui"`
	Package     Package             `json:"package"`
}

type Requirements struct {
	OS       []string `json:"os"`       // linux / darwin；空 = 不限
	Arch     []string `json:"arch"`     // amd64 / arm64
	MinPanel string   `json:"minPanel"` // 最低面板版本
	Distro   []string `json:"distro"`   // debian / rhel；空 = 不限
	License  bool     `json:"license"`  // 是否要授权码
	Product  string   `json:"product"`  // 授权绑定的产品 id，默认同 ID
}

type InstallSpec struct {
	Check      string  `json:"check"`      // 远端检测命令，退出码 0 = 已安装
	VersionCmd string  `json:"versionCmd"` // 取版本号，输出一行
	Steps      []Step  `json:"steps"`
	Uninstall  []Step  `json:"uninstall"`
	Params     []Param `json:"params"`
}

type Step struct {
	Title map[string]string `json:"title"`
	File  string            `json:"file"` // 包内相对路径
}

type Param struct {
	Key     string            `json:"key"`
	Label   map[string]string `json:"label"`
	Default string            `json:"default"`
	Hint    map[string]string `json:"hint"`
}

type UISpec struct {
	Entry    string            `json:"entry"`    // 包内相对路径，如 ui/panel.js
	TabLabel map[string]string `json:"tabLabel"` // 页签文字
}

type Package struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Catalog struct {
	Schema  int        `json:"schema"`
	Updated string     `json:"updated"`
	Plugins []Manifest `json:"plugins"`
}

// InstalledInfo 记录一个插件在本机的落地情况。
// 服务器端的安装状态不在这里 —— 那是每台机器各自的，靠远端检测实时判定。
type InstalledInfo struct {
	Version     string `json:"version"`
	InstalledAt string `json:"installedAt"`
}

// ---------------------------------------------------------------- Manager

const DefaultBase = "https://panel.wpqu.com"

type Manager struct {
	DataDir string
	Dir     string // <data>/plugins
	Base    string
	HTTP    *http.Client
	// PublicKey 留空则用内置发布公钥（内网自建插件源可指定）
	PublicKey string

	mu      sync.Mutex
	cache   *Catalog
	cacheAt time.Time
}

func NewManager(dataDir string) *Manager {
	base := strings.TrimRight(os.Getenv("WPQU_UPDATE_BASE"), "/")
	if base == "" {
		base = DefaultBase
	}
	dir := filepath.Join(dataDir, "plugins")
	return &Manager{
		DataDir: dataDir,
		Dir:     dir,
		Base:    base,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,63}$`)

// ValidID 防路径穿越：插件 id 会直接当目录名用。
func ValidID(id string) bool { return idRe.MatchString(id) }

func (m *Manager) localPath(id string) string { return filepath.Join(m.Dir, id) }

// LocalPath 返回插件在本机的目录（未安装时也返回路径）。
func (m *Manager) LocalPath(id string) string { return m.localPath(id) }

// ---------------------------------------------------------------- 目录

// FetchCatalog 拉取并校验插件索引。
func (m *Manager) FetchCatalog() (*Catalog, error) {
	body, err := m.get("/api/plugins/index.json")
	if err != nil {
		return nil, err
	}
	sig, err := m.get("/api/plugins/index.json.sig")
	if err != nil {
		return nil, fmt.Errorf("取索引签名失败：%w", err)
	}
	if err := m.verifyRelease(body, string(sig)); err != nil {
		return nil, err
	}
	var c Catalog
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, fmt.Errorf("插件索引格式错误：%w", err)
	}
	// 丢掉 id 不合法的条目，别让一个坏条目拖垮整个市场
	kept := c.Plugins[:0]
	for _, p := range c.Plugins {
		if ValidID(p.ID) {
			kept = append(kept, p)
		}
	}
	c.Plugins = kept
	sort.SliceStable(c.Plugins, func(i, j int) bool {
		if c.Plugins[i].Category != c.Plugins[j].Category {
			return c.Plugins[i].Category < c.Plugins[j].Category
		}
		return c.Plugins[i].ID < c.Plugins[j].ID
	})
	return &c, nil
}

// Catalog 带 5 分钟缓存：进页面、点详情、开安装弹窗都会读它，
// 每次都联网既慢又容易被更新源限流。
func (m *Manager) Catalog() (*Catalog, error) {
	m.mu.Lock()
	if m.cache != nil && time.Since(m.cacheAt) < 5*time.Minute {
		c := m.cache
		m.mu.Unlock()
		return c, nil
	}
	m.mu.Unlock()

	c, err := m.FetchCatalog()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.cache, m.cacheAt = c, time.Now()
	m.mu.Unlock()
	return c, nil
}

// Find 在目录里找一个插件；目录拉不到时退回本地已安装的清单，
// 这样断网也能卸载/查看已装插件。
func (m *Manager) Find(id string) (*Manifest, error) {
	if !ValidID(id) {
		return nil, errors.New("插件 id 不合法")
	}
	if c, err := m.Catalog(); err == nil {
		for i := range c.Plugins {
			if c.Plugins[i].ID == id {
				return &c.Plugins[i], nil
			}
		}
	}
	raw, err := os.ReadFile(filepath.Join(m.localPath(id), "plugin.json"))
	if err != nil {
		return nil, fmt.Errorf("找不到插件 %s", id)
	}
	var mf Manifest
	if err := json.Unmarshal(raw, &mf); err != nil {
		return nil, errors.New("本地插件清单损坏")
	}
	return &mf, nil
}

// ---------------------------------------------------------------- 安装包

// Ensure 确保插件包已下载并解压到本地，返回其目录。
// 已经装过同版本就直接复用，不重复下载。
func (m *Manager) Ensure(p Manifest) (string, error) {
	if !ValidID(p.ID) {
		return "", errors.New("插件 id 不合法")
	}
	dest := m.localPath(p.ID)
	if inst, ok := m.Installed()[p.ID]; ok && inst.Version == p.Version {
		if _, err := os.Stat(filepath.Join(dest, "plugin.json")); err == nil {
			return dest, nil
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// 下载
	if p.Package.URL == "" {
		return "", errors.New("插件缺少下载地址")
	}
	url := absolute(m.Base, p.Package.URL)
	body, err := m.getURL(url)
	if err != nil {
		return "", err
	}
	if p.Package.SHA256 != "" {
		sum := sha256.Sum256(body)
		got := hex.EncodeToString(sum[:])
		if !strings.EqualFold(got, p.Package.SHA256) {
			return "", fmt.Errorf("插件包校验失败（期望 %s…，实际 %s…）", short(p.Package.SHA256), short(got))
		}
	}
	// 包签名
	sig, err := m.getURL(url + ".sig")
	if err != nil {
		return "", fmt.Errorf("取插件包签名失败：%w", err)
	}
	if err := m.verifyRelease(body, string(sig)); err != nil {
		return "", err
	}

	// 解压到临时目录，成功后整体换上去，避免更新到一半留下残缺目录
	stage := filepath.Join(m.Dir, ".staging-"+p.ID)
	_ = os.RemoveAll(stage)
	if err := unzip(body, stage); err != nil {
		os.RemoveAll(stage)
		return "", err
	}
	// 校验包内清单与索引一致
	raw, err := os.ReadFile(filepath.Join(stage, "plugin.json"))
	if err != nil {
		os.RemoveAll(stage)
		return "", errors.New("插件包内缺少 plugin.json")
	}
	var inner Manifest
	if err := json.Unmarshal(raw, &inner); err != nil {
		os.RemoveAll(stage)
		return "", errors.New("插件包内的 plugin.json 无法解析")
	}
	if inner.ID != p.ID {
		os.RemoveAll(stage)
		return "", fmt.Errorf("插件包 id（%s）与索引（%s）不一致", inner.ID, p.ID)
	}

	if err := os.RemoveAll(dest); err != nil {
		os.RemoveAll(stage)
		return "", err
	}
	if err := os.Rename(stage, dest); err != nil {
		os.RemoveAll(stage)
		return "", err
	}

	m.recordInstalled(p.ID, p.Version)
	return dest, nil
}

// ReadFile 读插件包内的文件（安装脚本 / UI 入口）。
func (m *Manager) ReadFile(id, rel string) ([]byte, error) {
	if !ValidID(id) {
		return nil, errors.New("插件 id 不合法")
	}
	clean := filepath.Clean("/" + rel)
	if strings.Contains(clean, "..") {
		return nil, errors.New("非法路径")
	}
	full := filepath.Join(m.localPath(id), filepath.FromSlash(clean))
	if !strings.HasPrefix(full, m.localPath(id)+string(os.PathSeparator)) {
		return nil, errors.New("非法路径")
	}
	return os.ReadFile(full)
}

// IsInstalled 判断本地是否已解压。
func (m *Manager) IsInstalled(id string) bool {
	_, err := os.Stat(filepath.Join(m.localPath(id), "plugin.json"))
	return err == nil
}

// Remove 删除本地插件目录（面板侧），不涉及服务器。
func (m *Manager) Remove(id string) error {
	if !ValidID(id) {
		return errors.New("插件 id 不合法")
	}
	if err := os.RemoveAll(m.localPath(id)); err != nil {
		return err
	}
	inst := m.Installed()
	delete(inst, id)
	return m.writeInstalled(inst)
}

// ---------------------------------------------------------------- 本机安装记录

func (m *Manager) installedFile() string { return filepath.Join(m.Dir, "installed.json") }

func (m *Manager) Installed() map[string]InstalledInfo {
	out := map[string]InstalledInfo{}
	raw, err := os.ReadFile(m.installedFile())
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

func (m *Manager) recordInstalled(id, version string) {
	inst := m.Installed()
	inst[id] = InstalledInfo{Version: version, InstalledAt: time.Now().Format("2006-01-02 15:04:05")}
	_ = m.writeInstalled(inst)
}

func (m *Manager) writeInstalled(inst map[string]InstalledInfo) error {
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(inst, "", "  ")
	tmp := m.installedFile() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.installedFile())
}

// ---------------------------------------------------------------- 授权码存储

type licenseStore struct {
	// pluginID -> 授权码原文（保存原文而不是解析结果，方便日后换钥后重新校验）
	Keys map[string]string `json:"keys"`
}

func (m *Manager) licenseFile() string { return filepath.Join(m.DataDir, "licenses.json") }

// SaveLicense 校验通过后落盘。
func (m *Manager) SaveLicense(pluginID, code string) (*License, error) {
	lic, err := ParseLicense(code)
	if err != nil {
		return nil, err
	}
	if err := lic.Check(pluginID, time.Now()); err != nil {
		return nil, err
	}
	store := m.loadLicenses()
	store.Keys[pluginID] = strings.TrimSpace(code)
	if err := m.writeLicenses(store); err != nil {
		return nil, err
	}
	return lic, nil
}

// License 取已保存的授权码并重新校验（到期了就会报错）。
func (m *Manager) License(pluginID string) (*License, error) {
	store := m.loadLicenses()
	code, ok := store.Keys[pluginID]
	if !ok || code == "" {
		return nil, errors.New("尚未输入授权码")
	}
	lic, err := ParseLicense(code)
	if err != nil {
		return nil, err
	}
	if err := lic.Check(pluginID, time.Now()); err != nil {
		return nil, err
	}
	return lic, nil
}

// ClearLicense 用户换机或换码时清掉本地记录。
func (m *Manager) ClearLicense(pluginID string) {
	store := m.loadLicenses()
	delete(store.Keys, pluginID)
	_ = m.writeLicenses(store)
}

func (m *Manager) loadLicenses() licenseStore {
	s := licenseStore{Keys: map[string]string{}}
	if raw, err := os.ReadFile(m.licenseFile()); err == nil {
		_ = json.Unmarshal(raw, &s)
	}
	if s.Keys == nil {
		s.Keys = map[string]string{}
	}
	return s
}

func (m *Manager) writeLicenses(s licenseStore) error {
	raw, _ := json.MarshalIndent(s, "", "  ")
	tmp := m.licenseFile() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.licenseFile())
}

// ---------------------------------------------------------------- HTTP / zip

func (m *Manager) get(path string) ([]byte, error) { return m.getURL(absolute(m.Base, path)) }

func (m *Manager) verifyRelease(data []byte, sig string) error {
	if m.PublicKey != "" {
		return signing.VerifyWith(m.PublicKey, data, sig)
	}
	return signing.VerifyRelease(data, sig)
}

func (m *Manager) getURL(u string) ([]byte, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "wpqu-panel/plugin")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接插件源失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, fmt.Errorf("插件源上找不到 %s", u)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("插件源返回 HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// unzip 解压到目录，拒绝越界路径与超大包。
func unzip(data []byte, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("插件包不是合法的 zip：%w", err)
	}
	if len(zr.File) > 800 {
		return errors.New("插件包文件过多")
	}
	var total int64
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		if strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
			return fmt.Errorf("插件包含非法路径：%s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(filepath.Join(dest, filepath.FromSlash(name)), 0o700); err != nil {
				return err
			}
			continue
		}
		total += int64(f.UncompressedSize64)
		if total > 64<<20 {
			return errors.New("插件包解压后过大")
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(out, io.LimitReader(rc, 8<<20)); err != nil {
			out.Close()
			rc.Close()
			return err
		}
		out.Close()
		rc.Close()
	}
	return nil
}

func absolute(base, u string) string {
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	if !strings.HasPrefix(u, "/") {
		u = "/" + u
	}
	return strings.TrimRight(base, "/") + u
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}
