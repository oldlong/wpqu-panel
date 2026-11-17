package admin

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"wpqu-panel/internal/licensing"
)

// Entry 是发码台账里的一行。
//
// 授权码本身是自包含的（载荷 + 签名），台账不是「验证」用的，装不装得上
// 只取决于码本身；台账用来回答「我卖给谁了、什么时候到期、要不要续」。
// 所以它必须记下客户的字样和备注，光有码是不够的。
type Entry struct {
	ID         string `json:"id"`
	Code       string `json:"code"`
	Product    string `json:"product"`
	Expire     string `json:"expire"`
	Seats      int    `json:"seats"`
	Name       string `json:"name"`
	Note       string `json:"note,omitempty"`
	IssuedAt   string `json:"issuedAt"`
	Void       bool   `json:"void"`
	VoidReason string `json:"voidReason,omitempty"`
	VoidedAt   string `json:"voidedAt,omitempty"`
}

// Ledger 是发码台账，追加写 ndjson。
type Ledger struct {
	mu      sync.Mutex
	path    string
	entries []Entry
}

// LoadLedger 读取台账；文件不存在时返回空台账。
func LoadLedger(path string) (*Ledger, error) {
	l := &Ledger{path: path}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // 损坏行跳过，不要让整个台账读不出来
		}
		l.entries = append(l.entries, e)
	}
	return l, sc.Err()
}

// Add 追加一条并落盘。
func (l *Ledger) Add(e Entry) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(body, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	l.entries = append(l.entries, e)
	return nil
}

// All 返回台账副本（按签发顺序，旧的在前）。
func (l *Ledger) All() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Entry, len(l.entries))
	copy(out, l.entries)
	return out
}

// Get 按 id 取一条。
func (l *Ledger) Get(id string) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// Void 标记作废。
//
// 注意：这**不会**让码失效。离线验签没法吊销，作废只是台账上的一个标记，
// 用来避免「同一个客户续费时又发一张」之类的重复。界面上必须把这点说清楚。
func (l *Ledger) Void(id, reason string) (Entry, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.entries {
		if l.entries[i].ID != id {
			continue
		}
		l.entries[i].Void = true
		l.entries[i].VoidReason = reason
		l.entries[i].VoidedAt = time.Now().Format(time.RFC3339)
		if err := l.rewriteLocked(); err != nil {
			return Entry{}, false, err
		}
		return l.entries[i], true, nil
	}
	return Entry{}, false, nil
}

// rewriteLocked 全量重写台账（仅在作废时使用，条目数是几十到几千的量级）。
func (l *Ledger) rewriteLocked() error {
	tmp := l.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	for _, e := range l.entries {
		body, err := json.Marshal(e)
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		if _, err := f.Write(append(body, '\n')); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, l.path)
}

// statusOf 给界面用的状态文字。
func statusOf(e Entry, now time.Time) string {
	if e.Void {
		return "已作废"
	}
	lic := licensing.License{Product: e.Product, Expire: e.Expire, Seats: e.Seats, Name: e.Name, ID: e.ID}
	if lic.Expired(now) {
		return "已过期"
	}
	if d := lic.DaysLeft(now); d >= 0 && d <= 30 {
		return "即将到期"
	}
	if e.Expire == "" {
		return "永久"
	}
	return "有效"
}
