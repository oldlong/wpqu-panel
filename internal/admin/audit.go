package admin

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"
)

// AuditEntry 是一条审计记录。
//
// 后台能签升级包、能发授权码、能改官网，等于一个人掌握了整条发布链。
// 这份日志的唯一作用是事后能回答「谁在什么时候把它改成了这样」。
type AuditEntry struct {
	At     string `json:"at"`
	IP     string `json:"ip"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail,omitempty"`
}

// Audit 追加写审计日志（ndjson，一天一行），只读最近若干条。
type Audit struct {
	mu   sync.Mutex
	path string
}

// Add 记一条。写失败只打印，不影响主流程 —— 审计日志不该让业务操作失败。
func (a *Audit) Add(ip, action, target, detail string) {
	e := AuditEntry{
		At:     time.Now().Format(time.RFC3339),
		IP:     ip,
		Action: action,
		Target: target,
		Detail: detail,
	}
	body, err := json.Marshal(e)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(body, '\n'))
}

// Read 返回最近 limit 条，新的在前。
func (a *Audit) Read(limit int) []AuditEntry {
	if limit <= 0 || limit > 2000 {
		limit = 200
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.Open(a.path)
	if err != nil {
		return []AuditEntry{}
	}
	defer f.Close()

	var all []AuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // 半截写入的行直接跳过，不要因为一行坏了就看不了日志
		}
		all = append(all, e)
	}
	// 只保留最后 limit 条后再反转
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	out := make([]AuditEntry, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		out = append(out, all[i])
	}
	return out
}
