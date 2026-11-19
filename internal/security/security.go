// Package security：紧急修复推送（安全公告 + 一键升级）。
//
// 面板从分发站拉一份签名过的 security.json，里面是「公告」和「修复项」。
// 每个修复项自带检测方式与升级步骤，所以出现新漏洞时不必等面板发版：
// 运维在站点上改那份 JSON 并重签名，所有已装面板下次点「紧急修复」就能看到。
//
// 安全边界（很重要）：
//   - 只有 Ed25519 签名校验通过的清单才会被采用，用的是与升级包同一把发布公钥；
//   - 修复脚本来自清单本身，接口只接受 fix id，前端没法拼自己的命令；
//   - minPanel 高于当前面板版本时跳过该项，避免老面板去跑它不认识的格式。
package security

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"wpqu-panel/internal/signing"
	"wpqu-panel/internal/version"
)

const (
	// DefaultBase 与升级源同域：https://panel.wpqu.com
	DefaultBase = "https://panel.wpqu.com"
	feedPath    = "/api/security.json"
	sigPath     = "/api/security.json.sig"
	// cacheTTL 清单缓存时长。公告不常变，但也不想每次刷新都打一次网络。
	cacheTTL = 5 * time.Minute
	maxFeed  = 1 << 20
)

// Feed 是分发站上的紧急修复清单。
type Feed struct {
	Schema  int      `json:"schema"`
	Updated string   `json:"updated"`
	Notices []Notice `json:"notices"`
	Fixes   []Fix    `json:"fixes"`
}

// Notice 是一条公告。面板顶部只展示最近几条。
type Notice struct {
	ID    string            `json:"id"`
	Level string            `json:"level"` // critical | high | info
	Date  string            `json:"date"`
	Title map[string]string `json:"title"`
	Body  map[string]string `json:"body"`
	URL   string            `json:"url"`
}

// Fix 是一个可一键执行的修复项。
type Fix struct {
	ID       string            `json:"id"`
	Level    string            `json:"level"`
	Title    map[string]string `json:"title"`
	Summary  map[string]string `json:"summary"`
	URL      string            `json:"url"`
	MinPanel string            `json:"minPanel"`
	Check    Check             `json:"check"`
	Action   Action            `json:"action"`
}

// Check 描述「这台机器算不算受影响」。
type Check struct {
	Kind     string   `json:"kind"`     // apt | version | command
	Packages []string `json:"packages"` // kind=apt：要检查的包（支持 glob）
	Command  string   `json:"command"`  // kind=version|command
	Below    string   `json:"below"`    // kind=version：输出低于此版本算受影响
	Contains []string `json:"contains"` // kind=version：输出含任一子串算受影响
}

// Action 是一键修复要做的事。
type Action struct {
	Title map[string]string `json:"title"`
	Steps []Step            `json:"steps"`
}

// Step 是一步操作：用 apt 装包，或跑一段签名过的 shell。
type Step struct {
	Kind     string   `json:"kind"`     // apt | sh
	Label    string   `json:"label"`    // 回显给用户的一句话
	Packages []string `json:"packages"` // kind=apt
	Install  bool     `json:"install"`  // true=允许新装；默认只升级已装的
	Script   string   `json:"script"`   // kind=sh
}

// Update 是一行「包：旧 → 新」。
type Update struct {
	Package string `json:"package"`
	From    string `json:"from"`
	To      string `json:"to"`
}

// Result 是单个修复项在一台服务器上的检测结论。
type Result struct {
	ID       string   `json:"id"`
	Status   string   `json:"status"` // affected | ok | skip | unknown
	Detail   string   `json:"detail"` // 人看的一句话
	Version  string   `json:"version"`
	Target   string   `json:"target"`
	Updates  []Update `json:"updates,omitempty"`
	Affected bool     `json:"affected"`
}

// Client 拉清单并缓存。Base 可被 WPQU_UPDATE_BASE 覆盖，便于本地联调。
type Client struct {
	Base      string
	Current   string
	HTTP      *http.Client
	PublicKey string

	mu      sync.Mutex
	feed    *Feed
	fetched time.Time
}

// New 按当前面板版本构造客户端。
func New(current string) *Client {
	base := strings.TrimRight(os.Getenv("WPQU_UPDATE_BASE"), "/")
	if base == "" {
		base = DefaultBase
	}
	return &Client{
		Base:    base,
		Current: current,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Feed 取清单。force=false 时命中缓存直接返回（5 分钟内）。
// 分发站还没放这份文件（404）时返回空清单而不报错——功能上线前就是这个状态。
func (c *Client) Feed(force bool) (*Feed, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.feed != nil && time.Since(c.fetched) < cacheTTL {
		return c.feed, nil
	}
	body, err := c.get(feedPath)
	if err != nil {
		return nil, err
	}
	if body == nil {
		f := &Feed{}
		c.feed, c.fetched = f, time.Now()
		return f, nil
	}
	sig, err := c.get(sigPath)
	if err != nil {
		return nil, fmt.Errorf("取清单签名失败：%w", err)
	}
	if sig == nil {
		return nil, errors.New("分发站上有清单但缺签名，已拒绝使用")
	}
	if err := c.verify(body, string(sig)); err != nil {
		return nil, err
	}
	var f Feed
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("紧急修复清单格式错误：%w", err)
	}
	c.feed, c.fetched = &f, time.Now()
	return &f, nil
}

// Lookup 按 id 找修复项（只返回当前面板能跑的）。
func (c *Client) Lookup(id string) (*Fix, error) {
	f, err := c.Feed(false)
	if err != nil {
		return nil, err
	}
	usables := f.Usable(c.Current)
	for i := range usables {
		if usables[i].ID == id {
			return &usables[i], nil
		}
	}
	return nil, errors.New("清单里没有这条修复（可能已下线或需要更新面板），刷新后再试")
}

// FetchedAt 返回本次清单的拉取时间（零值表示还没拉过）。
func (c *Client) FetchedAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fetched
}

func (c *Client) get(path string) ([]byte, error) {
	req, err := http.NewRequest("GET", c.Base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "wpqu-panel/"+c.Current)
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接分发站失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, nil // 还没上线
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("分发站返回 HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxFeed))
}

func (c *Client) verify(data []byte, sig string) error {
	if c.PublicKey != "" {
		return signing.VerifyWith(c.PublicKey, data, sig)
	}
	if !signing.HasReleaseKey() {
		return errors.New("此构建没有内置发布公钥，无法校验紧急修复清单")
	}
	return signing.VerifyRelease(data, sig)
}

// ---------------------------------------------------------------- 清单筛选

// Usable 过滤出当前面板能处理的修复项：有步骤、版本够、级别高的排前面。
func (f *Feed) Usable(current string) []Fix {
	out := make([]Fix, 0, len(f.Fixes))
	for _, x := range f.Fixes {
		if x.ID == "" || len(x.Action.Steps) == 0 {
			continue
		}
		if x.MinPanel != "" && version.IsNewer(x.MinPanel, current) {
			continue
		}
		out = append(out, x)
	}
	sort.SliceStable(out, func(i, j int) bool { return levelRank(out[i].Level) < levelRank(out[j].Level) })
	return out
}

func levelRank(l string) int {
	switch strings.ToLower(l) {
	case "critical":
		return 0
	case "high":
		return 1
	case "warn":
		return 2
	default:
		return 3
	}
}

// ---------------------------------------------------------------- 远端脚本

// ScanScript 把所有检测项拼成一条远端脚本。
// 输出按制表符分隔，前缀 SC 便于和别的输出区分：
//
//	SC	<id>	APT	<包>	<旧版本>	<新版本>
//	SC	<id>	VER	<退出码>	<输出>
//	SC	<id>	CMD	<退出码>
func ScanScript(fixes []Fix) string {
	var b strings.Builder
	b.WriteString("export LC_ALL=C\n")
	needApt := false
	for _, f := range fixes {
		if f.Check.Kind == "apt" && len(f.Check.Packages) > 0 {
			needApt = true
		}
	}
	if needApt {
		// 先刷索引，否则「有没有修复版」永远是旧的
		b.WriteString("if command -v apt-get >/dev/null 2>&1; then apt-get update -qq >/dev/null 2>&1 || true; fi\n")
	}
	for _, f := range fixes {
		id := safeID(f.ID)
		switch f.Check.Kind {
		case "apt":
			if len(f.Check.Packages) == 0 {
				continue
			}
			pkgs := make([]string, 0, len(f.Check.Packages))
			for _, p := range f.Check.Packages {
				pkgs = append(pkgs, shellQuote(p))
			}
			fmt.Fprintf(&b,
				"apt-get -s --only-upgrade install -y %s 2>/dev/null | "+
					"awk -v id=%s '/^Inst /{ s=$0; sub(/^Inst [^ ]+ \\[/,\"\",s); sub(/\\].*$/,\"\",s); old=s; "+
					"t=$0; sub(/^Inst [^ ]+ \\[[^]]*\\] \\(/,\"\",t); split(t,a,\" \"); "+
					"printf \"SC\\t%%s\\tAPT\\t%%s\\t%%s\\t%%s\\n\", id, $2, old, a[1] }'\n",
				strings.Join(pkgs, " "), shellQuote(id))
		case "version":
			if strings.TrimSpace(f.Check.Command) == "" {
				continue
			}
			fmt.Fprintf(&b,
				"v=$(%s 2>/dev/null); rc=$?; v=$(printf '%%s' \"$v\" | head -1); "+
					"printf 'SC\\t%%s\\tVER\\t%%d\\t%%s\\n' %s \"$rc\" \"$v\"\n",
				f.Check.Command, shellQuote(id))
		case "command":
			if strings.TrimSpace(f.Check.Command) == "" {
				continue
			}
			fmt.Fprintf(&b, "%s >/dev/null 2>&1; printf 'SC\\t%%s\\tCMD\\t%%d\\n' %s \"$?\"\n",
				f.Check.Command, shellQuote(id))
		}
	}
	return b.String()
}

// safeID 做一次白名单检查：id 会进 awk 的 -v 与 printf，只允许常见字符。
func safeID(id string) string {
	if id == "" {
		return "invalid_id"
	}
	for _, r := range id {
		ok := r == '-' || r == '_' || r == '.' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return "invalid_id"
		}
	}
	return id
}

// ParseScan 把脚本输出解析成 id -> 结论。
func ParseScan(fixes []Fix, out string) map[string]Result {
	type row struct{ kind, a, b, c string }
	rows := map[string][]row{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "SC\t") {
			continue
		}
		p := strings.Split(line, "\t")
		if len(p) < 4 {
			continue
		}
		r := row{kind: p[2]}
		switch p[2] {
		case "APT":
			if len(p) >= 6 {
				r.a, r.b, r.c = p[3], p[4], p[5]
			}
		case "VER":
			r.a = p[3]
			if len(p) >= 5 {
				r.b = p[4]
			}
		case "CMD":
			r.a = p[3]
		}
		rows[p[1]] = append(rows[p[1]], r)
	}

	res := make(map[string]Result, len(fixes))
	for _, f := range fixes {
		r := Result{ID: f.ID}
		got := rows[f.ID]
		switch f.Check.Kind {
		case "apt":
			var ups []Update
			for _, x := range got {
				if x.kind == "APT" && x.a != "" {
					ups = append(ups, Update{Package: x.a, From: x.b, To: x.c})
				}
			}
			r.Updates = ups
			if len(ups) > 0 {
				r.Affected, r.Status = true, "affected"
				r.Detail = aptDetail(ups)
				r.Version, r.Target = ups[0].From, ups[0].To
			} else if len(got) == 0 {
				r.Status, r.Detail = "ok", "没有待升级的包"
			} else {
				r.Status, r.Detail = "ok", "已是最新"
			}
		case "version":
			ver, rc := "", 1
			for _, x := range got {
				if x.kind == "VER" {
					ver, rc = strings.TrimSpace(x.b), atoi(x.a)
				}
			}
			r.Version = ver
			if rc != 0 || ver == "" {
				r.Status, r.Detail = "skip", "读不到版本（可能没装）"
				break
			}
			r.Target = f.Check.Below
			if matchVersion(f.Check, ver) {
				r.Affected, r.Status = true, "affected"
				r.Detail = ver + " → " + f.Check.Below
			} else {
				r.Status, r.Detail = "ok", ver+"，不需要处理"
			}
		case "command":
			rc := 1
			for _, x := range got {
				if x.kind == "CMD" {
					rc = atoi(x.a)
				}
			}
			if rc == 0 {
				r.Affected, r.Status, r.Detail = true, "affected", "命令检出风险"
			} else {
				r.Status, r.Detail = "ok", "未检出"
			}
		default:
			r.Status, r.Detail = "unknown", "清单里的检测方式不认识"
		}
		res[f.ID] = r
	}
	return res
}

func aptDetail(up []Update) string {
	if len(up) == 1 {
		return up[0].From + " → " + up[0].To
	}
	return fmt.Sprintf("%s → %s（另外 %d 个包）", up[0].From, up[0].To, len(up)-1)
}

func matchVersion(c Check, got string) bool {
	if c.Below != "" && ComparePkg(got, c.Below) < 0 {
		return true
	}
	for _, s := range c.Contains {
		if s != "" && strings.Contains(got, s) {
			return true
		}
	}
	return false
}

// ComparePkg 逐段比数字，够用即可：1.26.3-3+deb13u6 会判小于 1.26.3-3+deb13u10。
// 发行版版本号的完整语义交给 apt 自己，这里只用于「低于某版本」的阈值提示。
func ComparePkg(a, b string) int {
	as, bs := numGroups(a), numGroups(b)
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func numGroups(s string) []int {
	var out []int
	cur, has := 0, false
	for _, r := range s {
		if r >= '0' && r <= '9' {
			cur = cur*10 + int(r-'0')
			has = true
			continue
		}
		if has {
			out = append(out, cur)
			cur, has = 0, false
		}
	}
	if has {
		out = append(out, cur)
	}
	return out
}

// Script 把修复步骤拼成一段可执行脚本。命令全部来自签名清单，不接受外部输入。
func (f Fix) Script() string {
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString("export DEBIAN_FRONTEND=noninteractive\n")
	b.WriteString("export LC_ALL=C\n")
	// 先刷一次索引：漏洞刚爆出来时本机 apt 索引往往还是旧的，
	// 不 update 直接 --only-upgrade 会「没有可升级的包」，等于修了个寂寞。
	needApt := false
	for _, s := range f.Action.Steps {
		if s.Kind == "apt" && len(s.Packages) > 0 {
			needApt = true
			break
		}
	}
	if needApt {
		b.WriteString("echo '刷新软件源'\n")
		b.WriteString("apt-get update -qq >/dev/null 2>&1 || true\n")
	}
	for i, s := range f.Action.Steps {
		switch s.Kind {
		case "apt":
			if len(s.Packages) == 0 {
				continue
			}
			args := []string{"apt-get", "install", "-y"}
			if !s.Install {
				args = append(args, "--only-upgrade")
			}
			for _, p := range s.Packages {
				args = append(args, shellQuote(p))
			}
			fmt.Fprintf(&b, "echo '[%d/%d] 升级 %s'\n", i+1, len(f.Action.Steps), strings.Join(s.Packages, " "))
			b.WriteString(strings.Join(args, " ") + "\n")
		case "sh":
			if strings.TrimSpace(s.Script) == "" {
				continue
			}
			label := strings.TrimSpace(s.Label)
			if label == "" {
				label = "执行修复步骤"
			}
			fmt.Fprintf(&b, "echo '[%d/%d] %s'\n", i+1, len(f.Action.Steps), label)
			b.WriteString(strings.TrimSpace(s.Script) + "\n")
		}
	}
	return b.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}
