package store

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"
)

// 本地加密（vault）
//
// 用户可以对**某台服务器**单独设一个密码。设了之后，这台服务器的 SSH 凭据改用
// 「密码派生出来的密钥」加密，data/secret.key 解不开它 —— 也就是说，拿到整个
// data 目录的人也读不到凭据，必须有密码。
//
// 密码本身不落盘（只存 salt 与一个验证器），所以忘了就真的找不回来。
// 按需求不校验密码强度（几位数字也放行），安全性靠「连续 5 次错误锁 1 小时」兜着：
// 计数与锁定时间都写进 servers.json，重启面板不会清零。
//
// 解锁后的密钥只在内存里留 30 分钟（每次使用续期），面板一重启就要重新输。

const (
	// VaultMaxFails 连续输错几次触发锁定
	VaultMaxFails = 5
	// VaultLockDuration 一次锁定多久
	VaultLockDuration = time.Hour
	// vaultKeyTTL 解锁后密钥在内存里的存活时间（每次使用会续期）
	vaultKeyTTL  = 30 * time.Minute
	vaultSaltLen = 16
	// vaultVerifier 验证器明文：能把它解开就说明密码对，不必碰真凭据
	vaultVerifier = "wpqu-vault-v1"
	// scrypt 参数：32MB / 单次约几十毫秒，本地面板够用
	vaultScryptN = 32768
	vaultScryptR = 8
	vaultScryptP = 1
)

// VaultMeta 是 servers.json 里 vault 字段。
type VaultMeta struct {
	Enabled     bool      `json:"enabled"`
	Salt        string    `json:"salt"`
	Verifier    SecretBox `json:"verifier"`
	Secret      SecretBox `json:"secret"`
	Fails       int       `json:"fails"`
	LockedUntil string    `json:"lockedUntil,omitempty"`
	SetAt       string    `json:"setAt"`
	UpdatedAt   string    `json:"updatedAt"`
}

// VaultInfo 是对外暴露的状态（不含任何密钥材料）。
type VaultInfo struct {
	Enabled     bool   `json:"enabled"`
	Unlocked    bool   `json:"unlocked"`
	Fails       int    `json:"fails"`
	Remaining   int    `json:"remaining"`
	LockedUntil string `json:"lockedUntil,omitempty"`
	LockSeconds int    `json:"lockSeconds"`
}

// VaultNeedPassword 表示这台服务器设了密码、当前还没解锁。
type VaultNeedPassword struct {
	ServerID    string
	Name        string
	LockedUntil string
}

func (e *VaultNeedPassword) Error() string {
	if e.LockedUntil != "" {
		return "该服务器已设置本地密码，并且处于锁定状态，请稍后再试"
	}
	return "该服务器已设置本地密码，请输入密码解锁"
}

// VaultBadPassword 表示密码不对（还没到锁定次数）。
type VaultBadPassword struct{ Remaining int }

func (e *VaultBadPassword) Error() string {
	return fmt.Sprintf("密码不正确，还可以尝试 %d 次", e.Remaining)
}

// VaultLockedOut 表示已触发锁定。
type VaultLockedOut struct{ Seconds int }

func (e *VaultLockedOut) Error() string {
	return fmt.Sprintf("连续输错 %d 次，已锁定，%s后再试", VaultMaxFails, humanWait(e.Seconds))
}

func humanWait(sec int) string {
	if sec <= 0 {
		return "1 分钟"
	}
	if sec < 60 {
		return fmt.Sprintf("%d 秒", sec)
	}
	mins := (sec + 59) / 60
	if mins < 60 {
		return fmt.Sprintf("%d 分钟", mins)
	}
	return fmt.Sprintf("%d 小时", (mins+59)/60)
}

func (m *VaultMeta) lockSeconds(now time.Time) int {
	if m == nil || m.LockedUntil == "" {
		return 0
	}
	until, err := time.Parse(time.RFC3339, m.LockedUntil)
	if err != nil {
		return 0
	}
	d := until.Sub(now)
	if d <= 0 {
		return 0
	}
	return int(d.Seconds() + 0.999)
}

func (m *VaultMeta) remaining() int {
	if m.Fails >= VaultMaxFails {
		return 0
	}
	n := VaultMaxFails - m.Fails
	if n < 0 {
		return 0
	}
	return n
}

// ---------------------------------------------------------------- 密钥

// deriveKey 用 scrypt 从密码 + salt 派生 32 字节密钥。
func deriveKey(password, saltB64 string) ([]byte, error) {
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return nil, fmt.Errorf("salt 损坏：%w", err)
	}
	return scrypt.Key([]byte(password), salt, vaultScryptN, vaultScryptR, vaultScryptP, keyLen)
}

func newSalt() (string, error) {
	b := make([]byte, vaultSaltLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// sealWithKey 用指定密钥加密（输出与 secret 字段同样的 {iv,tag,data}）。
func sealWithKey(key []byte, plaintext string) (*SecretBox, error) {
	aead, err := aeadOf(key)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, aead.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	data := aead.Seal(nil, iv, []byte(plaintext), nil)
	return &SecretBox{
		IV:   base64.StdEncoding.EncodeToString(iv),
		Tag:  base64.StdEncoding.EncodeToString(data[len(data)-aead.Overhead():]),
		Data: base64.StdEncoding.EncodeToString(data[:len(data)-aead.Overhead()]),
	}, nil
}

// openWithKey 用指定密钥解密。
func openWithKey(key []byte, box *SecretBox) (string, error) {
	aead, err := aeadOf(key)
	if err != nil {
		return "", err
	}
	iv, err := base64.StdEncoding.DecodeString(box.IV)
	if err != nil {
		return "", err
	}
	data, err := base64.StdEncoding.DecodeString(box.Data)
	if err != nil {
		return "", err
	}
	tag, err := base64.StdEncoding.DecodeString(box.Tag)
	if err != nil {
		return "", err
	}
	if len(tag) != aead.Overhead() {
		return "", fmt.Errorf("tag 长度异常")
	}
	plain, err := aead.Open(nil, iv, append(data, tag...), nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// ---------------------------------------------------------------- 会话密钥

type vaultSession struct {
	key     []byte
	expires time.Time
}

// putVaultKey 记住解锁后的密钥（TTL 30 分钟，后续使用会续期）。
func (s *Store) putVaultKey(id string, key []byte) {
	s.vaultMu.Lock()
	defer s.vaultMu.Unlock()
	if s.vaultKeys == nil {
		s.vaultKeys = map[string]vaultSession{}
	}
	cp := make([]byte, len(key))
	copy(cp, key)
	s.vaultKeys[id] = vaultSession{key: cp, expires: time.Now().Add(vaultKeyTTL)}
}

// vaultKey 取内存里的密钥；过期的顺手清掉。
func (s *Store) vaultKey(id string) []byte {
	s.vaultMu.Lock()
	defer s.vaultMu.Unlock()
	sess, ok := s.vaultKeys[id]
	if !ok {
		return nil
	}
	if time.Now().After(sess.expires) {
		delete(s.vaultKeys, id)
		return nil
	}
	sess.expires = time.Now().Add(vaultKeyTTL)
	s.vaultKeys[id] = sess
	return sess.key
}

func (s *Store) dropVaultKey(id string) {
	s.vaultMu.Lock()
	defer s.vaultMu.Unlock()
	delete(s.vaultKeys, id)
}

// LockVault 忘掉解锁状态（面板里点「立即锁定」）。
func (s *Store) LockVault(id string) { s.dropVaultKey(id) }

// ---------------------------------------------------------------- 读记录

// findRecord 在原始列表里找一条记录，返回下标与解析结果。
// 调用方必须已持有 s.mu。
func findRecord(raws []json.RawMessage, id string) (int, *ServerRecord, bool) {
	for i, raw := range raws {
		r, err := decode(raw)
		if err != nil || r.ID != id {
			continue
		}
		return i, r, true
	}
	return -1, nil, false
}

// saveRecord 把一条记录写回并落盘（调用方持锁）。
func (s *Store) saveRecord(raws []json.RawMessage, idx int, r *ServerRecord) error {
	out, err := encodeFull(r)
	if err != nil {
		return err
	}
	raws[idx] = out
	return s.writeRaw(raws)
}

// recordCreds 用机器密钥解开凭据（老路径）。
func (s *Store) recordCreds(r *ServerRecord) (*Credentials, error) {
	if len(r.Secret) == 0 {
		return nil, nil
	}
	var box SecretBox
	if err := json.Unmarshal(r.Secret, &box); err != nil || box.Data == "" {
		return nil, nil
	}
	plain, err := s.Decrypt(&box)
	if err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal([]byte(plain), &creds); err != nil {
		return nil, err
	}
	return &creds, nil
}

// vaultCreds 用密码派生的密钥解开凭据。
func vaultCreds(key []byte, m *VaultMeta) (*Credentials, error) {
	plain, err := openWithKey(key, &m.Secret)
	if err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal([]byte(plain), &creds); err != nil {
		return nil, err
	}
	return &creds, nil
}

func encodeCreds(c *Credentials) string {
	raw, _ := json.Marshal(c)
	return string(raw)
}

// ---------------------------------------------------------------- 校验

// vaultCheck 校验密码。失败时把失败计数写盘并返回对应的错误类型。
// 成功时不动记录，由调用方决定后续（重置计数 / 换密码 / 关闭加密）。
// 调用方必须已持有 s.mu。
func (s *Store) vaultCheck(raws []json.RawMessage, idx int, r *ServerRecord, password string) ([]byte, error) {
	m := r.Vault
	if m == nil || !m.Enabled {
		return nil, errors.New("该服务器没有设置本地密码")
	}
	now := time.Now()
	if sec := m.lockSeconds(now); sec > 0 {
		return nil, &VaultLockedOut{Seconds: sec}
	}
	key, err := deriveKey(password, m.Salt)
	if err != nil {
		return nil, err
	}
	if plain, err := openWithKey(key, &m.Verifier); err != nil ||
		subtle.ConstantTimeCompare([]byte(plain), []byte(vaultVerifier)) != 1 {
		m.Fails++
		if m.Fails >= VaultMaxFails {
			// 锁定时把计数清零：一小时后重新给满 5 次机会
			m.Fails = 0
			m.LockedUntil = now.Add(VaultLockDuration).UTC().Format(time.RFC3339)
			_ = s.saveRecord(raws, idx, r)
			return nil, &VaultLockedOut{Seconds: int(VaultLockDuration.Seconds())}
		}
		m.UpdatedAt = now.UTC().Format(time.RFC3339)
		_ = s.saveRecord(raws, idx, r)
		return nil, &VaultBadPassword{Remaining: m.remaining()}
	}
	return key, nil
}

// ---------------------------------------------------------------- 对外操作

// VaultInfo 返回某台服务器的加密状态；服务器不存在时返回 nil。
func (s *Store) VaultInfo(id string) (*VaultInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raws, err := s.rawServers()
	if err != nil {
		return nil, err
	}
	_, r, ok := findRecord(raws, id)
	if !ok {
		return nil, nil
	}
	info := &VaultInfo{Remaining: VaultMaxFails}
	if r.Vault == nil || !r.Vault.Enabled {
		info.Remaining = 0
		return info, nil
	}
	now := time.Now()
	info.Enabled = true
	info.Fails = r.Vault.Fails
	info.Remaining = r.Vault.remaining()
	info.LockSeconds = r.Vault.lockSeconds(now)
	if info.LockSeconds > 0 {
		info.LockedUntil = r.Vault.LockedUntil
		info.Remaining = 0
	}
	info.Unlocked = info.LockSeconds == 0 && s.vaultKey(id) != nil
	return info, nil
}

// SetVaultPassword 给服务器加密码：凭据改成用密码派生的密钥加密。
// 不校验强度，只要求非空。
func (s *Store) SetVaultPassword(id, password string) (*VaultInfo, error) {
	if strings.TrimSpace(password) == "" {
		return nil, errors.New("请输入密码")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	raws, err := s.rawServers()
	if err != nil {
		return nil, err
	}
	idx, r, ok := findRecord(raws, id)
	if !ok {
		return nil, errors.New("服务器不存在")
	}
	if r.Vault != nil && r.Vault.Enabled {
		return nil, errors.New("该服务器已经设置过密码，请用「修改密码」")
	}
	creds, err := s.recordCreds(r)
	if err != nil || creds == nil {
		return nil, errors.New("凭据读取失败，请先在「编辑」里重新填写密码或私钥")
	}
	salt, err := newSalt()
	if err != nil {
		return nil, err
	}
	key, err := deriveKey(password, salt)
	if err != nil {
		return nil, err
	}
	secret, err := sealWithKey(key, encodeCreds(creds))
	if err != nil {
		return nil, err
	}
	verifier, err := sealWithKey(key, vaultVerifier)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	r.Vault = &VaultMeta{
		Enabled: true, Salt: salt, Verifier: *verifier, Secret: *secret,
		SetAt: now, UpdatedAt: now,
	}
	// 机器密钥那份明文凭据必须抹掉，否则加密形同虚设
	r.Secret = nil
	if err := s.saveRecord(raws, idx, r); err != nil {
		return nil, err
	}
	s.putVaultKey(id, key)
	return s.vaultInfoLocked(id, r), nil
}

// ChangeVaultPassword 改密码：验原密码 → 用新密码重新加密凭据。
func (s *Store) ChangeVaultPassword(id, oldPassword, newPassword string) (*VaultInfo, error) {
	if strings.TrimSpace(newPassword) == "" {
		return nil, errors.New("请输入新密码")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	raws, err := s.rawServers()
	if err != nil {
		return nil, err
	}
	idx, r, ok := findRecord(raws, id)
	if !ok {
		return nil, errors.New("服务器不存在")
	}
	if r.Vault == nil || !r.Vault.Enabled {
		return nil, errors.New("该服务器还没有设置密码")
	}
	key, err := s.vaultCheck(raws, idx, r, oldPassword)
	if err != nil {
		return nil, err
	}
	creds, err := vaultCreds(key, r.Vault)
	if err != nil {
		return nil, errors.New("凭据解密失败，密码可能已被改动")
	}
	salt, err := newSalt()
	if err != nil {
		return nil, err
	}
	newKey, err := deriveKey(newPassword, salt)
	if err != nil {
		return nil, err
	}
	secret, err := sealWithKey(newKey, encodeCreds(creds))
	if err != nil {
		return nil, err
	}
	verifier, err := sealWithKey(newKey, vaultVerifier)
	if err != nil {
		return nil, err
	}
	r.Vault.Salt = salt
	r.Vault.Secret = *secret
	r.Vault.Verifier = *verifier
	r.Vault.Fails = 0
	r.Vault.LockedUntil = ""
	r.Vault.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := s.saveRecord(raws, idx, r); err != nil {
		return nil, err
	}
	s.putVaultKey(id, newKey)
	return s.vaultInfoLocked(id, r), nil
}

// RemoveVault 关闭加密：凭据改回机器密钥加密（等同原来的行为）。
func (s *Store) RemoveVault(id, password string) (*VaultInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raws, err := s.rawServers()
	if err != nil {
		return nil, err
	}
	idx, r, ok := findRecord(raws, id)
	if !ok {
		return nil, errors.New("服务器不存在")
	}
	if r.Vault == nil || !r.Vault.Enabled {
		return nil, errors.New("该服务器没有设置密码")
	}
	key, err := s.vaultCheck(raws, idx, r, password)
	if err != nil {
		return nil, err
	}
	creds, err := vaultCreds(key, r.Vault)
	if err != nil {
		return nil, errors.New("凭据解密失败，无法关闭加密")
	}
	box, err := s.Encrypt(encodeCreds(creds))
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(box)
	if err != nil {
		return nil, err
	}
	r.Secret = raw
	r.Vault = nil
	if err := s.saveRecord(raws, idx, r); err != nil {
		return nil, err
	}
	s.dropVaultKey(id)
	return &VaultInfo{Enabled: false}, nil
}

// UnlockVault 校验密码并把密钥放进内存。
func (s *Store) UnlockVault(id, password string) (*VaultInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raws, err := s.rawServers()
	if err != nil {
		return nil, err
	}
	idx, r, ok := findRecord(raws, id)
	if !ok {
		return nil, errors.New("服务器不存在")
	}
	if r.Vault == nil || !r.Vault.Enabled {
		return nil, errors.New("该服务器没有设置本地密码")
	}
	key, err := s.vaultCheck(raws, idx, r, password)
	if err != nil {
		return nil, err
	}
	if r.Vault.Fails != 0 || r.Vault.LockedUntil != "" {
		r.Vault.Fails = 0
		r.Vault.LockedUntil = ""
		r.Vault.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := s.saveRecord(raws, idx, r); err != nil {
			return nil, err
		}
	}
	s.putVaultKey(id, key)
	return s.vaultInfoLocked(id, r), nil
}

// vaultInfoLocked 组装状态（调用方持锁且已确认服务器存在）。
func (s *Store) vaultInfoLocked(id string, r *ServerRecord) *VaultInfo {
	info := &VaultInfo{Remaining: VaultMaxFails}
	if r.Vault == nil || !r.Vault.Enabled {
		info.Remaining = 0
		return info
	}
	info.Enabled = true
	info.Fails = r.Vault.Fails
	info.Remaining = r.Vault.remaining()
	info.LockSeconds = r.Vault.lockSeconds(time.Now())
	if info.LockSeconds > 0 {
		info.LockedUntil = r.Vault.LockedUntil
		info.Remaining = 0
	}
	info.Unlocked = info.LockSeconds == 0 && s.vaultKey(id) != nil
	return info
}

// vaultNeedPassword 把「已加密但未解锁」翻译成统一的错误类型。
func vaultNeedPassword(r *ServerRecord) error {
	if r == nil || r.Vault == nil || !r.Vault.Enabled {
		return nil
	}
	until := ""
	if r.Vault.lockSeconds(time.Now()) > 0 {
		until = r.Vault.LockedUntil
	}
	return &VaultNeedPassword{ServerID: r.ID, Name: r.Name, LockedUntil: until}
}
