package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/scrypt"
)

// scrypt 参数。N=32768 大约占 32 MB 内存、耗时 ~100ms，
// 对单人使用的后台完全够用，同时让离线爆破变得很贵。
const (
	scryptN = 1 << 15
	scryptR = 8
	scryptP = 1
	keyLen  = 32
)

// Auth 是口令记录。只存盐和派生值，不存明文，也不存可逆的加密结果。
type Auth struct {
	User string `json:"user"`
	Salt string `json:"salt"`
	Hash string `json:"hash"`
	N    int    `json:"n"`
	R    int    `json:"r"`
	P    int    `json:"p"`
}

// LoadAuth 读取口令文件，不存在时返回 os.ErrNotExist。
func LoadAuth(path string) (*Auth, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var a Auth
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("口令文件损坏：%w", err)
	}
	if a.N == 0 {
		a.N, a.R, a.P = scryptN, scryptR, scryptP
	}
	return &a, nil
}

// NewAuth 用给定口令生成记录。
func NewAuth(user, password string) (*Auth, error) {
	if len(password) < 8 {
		return nil, errors.New("口令至少 8 位")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	a := &Auth{
		User: user,
		Salt: base64.StdEncoding.EncodeToString(salt),
		N:    scryptN, R: scryptR, P: scryptP,
	}
	sum, err := scrypt.Key([]byte(password), salt, a.N, a.R, a.P, keyLen)
	if err != nil {
		return nil, err
	}
	a.Hash = base64.StdEncoding.EncodeToString(sum)
	return a, nil
}

// Save 写口令文件，权限 0600。
func (a *Auth) Save(path string) error {
	body, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o600)
}

// Verify 比对口令。用 subtle 做常数时间比较，避免把「前几位对了」这种
// 时间差暴露给爆破脚本。
func (a *Auth) Verify(password string) bool {
	salt, err := base64.StdEncoding.DecodeString(a.Salt)
	if err != nil {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(a.Hash)
	if err != nil {
		return false
	}
	got, err := scrypt.Key([]byte(password), salt, a.N, a.R, a.P, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// RandomPassword 生成一个便于抄写的随机口令（去掉易混淆字符）。
func RandomPassword(n int) string {
	const alphabet = "abcdefghjkmnpqrstuvwxyzACDEFGHJKLMNPQRSTUVWXYZ23456789"
	if n < 8 {
		n = 16
	}
	out := make([]byte, n)
	for i := range out {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			out[i] = 'x'
			continue
		}
		out[i] = alphabet[idx.Int64()]
	}
	return string(out)
}

// ---------------------------------------------------------------- 会话

// Sessions 是内存里的登录态表。
//
// 刻意不落盘：重启即全体登出，比「会话文件被拷走就能冒充管理员」划算得多。
type Sessions struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]time.Time
}

// NewSessions 创建会话表。
func NewSessions(ttl time.Duration) *Sessions {
	return &Sessions{ttl: ttl, m: map[string]time.Time{}}
}

// New 签发一个会话，返回 token 与过期时间。
func (s *Sessions) New() (string, time.Time) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	exp := time.Now().Add(s.ttl)
	s.mu.Lock()
	// 顺手清理过期项，避免长期运行后无界增长
	for k, v := range s.m {
		if time.Now().After(v) {
			delete(s.m, k)
		}
	}
	s.m[token] = exp
	s.mu.Unlock()
	return token, exp
}

// Valid 校验并滑动续期。
func (s *Sessions) Valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[token]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.m, token)
		return false
	}
	s.m[token] = time.Now().Add(s.ttl)
	return true
}

// Drop 注销一个会话。
func (s *Sessions) Drop(token string) {
	s.mu.Lock()
	delete(s.m, token)
	s.mu.Unlock()
}

// ---------------------------------------------------------------- 限流

// limiter 按来源 IP 限制登录失败次数。
//
// 没有它，一个 8 位口令在暴露到公网的后台上只是个时间问题。
type limiter struct {
	mu    sync.Mutex
	fails map[string]*attempt
}

type attempt struct {
	count int
	until time.Time
}

func newLimiter() *limiter {
	return &limiter{fails: map[string]*attempt{}}
}

// allow 判断当前 IP 是否还能尝试登录，第二返回值是还需等待多久。
func (l *limiter) allow(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.fails[ip]
	if !ok {
		return true, 0
	}
	if time.Now().Before(a.until) {
		return false, time.Until(a.until).Round(time.Second)
	}
	return true, 0
}

// fail 记一次失败，并按次数递增惩罚：1 分钟、2 分钟、4 分钟……最多 15 分钟。
func (l *limiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.fails[ip]
	if !ok {
		a = &attempt{}
		l.fails[ip] = a
	}
	a.count++
	if a.count < 5 {
		return // 前 4 次不惩罚，避免手误就把自己锁在外面
	}
	penalty := time.Minute << uint(min(a.count-5, 4))
	if penalty > 15*time.Minute {
		penalty = 15 * time.Minute
	}
	a.until = time.Now().Add(penalty)
}

// reset 成功后清零。
func (l *limiter) reset(ip string) {
	l.mu.Lock()
	delete(l.fails, ip)
	l.mu.Unlock()
}
