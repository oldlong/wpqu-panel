// Package store：服务器档案（AES-256-GCM 加密）与操作日志。
// 数据格式与早期实现完全一致（servers.json / secret.key / logs/*.jsonl），
// 同一份 data 目录可以在两个实现之间无缝切换。
package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const keyLen = 32

// SecretBox 是 servers.json 里 secret 字段的形状（与早期实现逐字段一致）。
type SecretBox struct {
	IV   string `json:"iv"`
	Tag  string `json:"tag"`
	Data string `json:"data"`
}

// ServerRecord 是 servers.json 里的一条服务器记录。
// secret 用 json.RawMessage 保住原样，写入时也原样写回。
type ServerRecord struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Host            string          `json:"host"`
	Port            int             `json:"port"`
	Username        string          `json:"username"`
	AuthType        string          `json:"authType"`
	Note            string          `json:"note"`
	CreatedAt       string          `json:"createdAt"`
	LastConnectedAt *string         `json:"lastConnectedAt"`
	LastStatus      string          `json:"lastStatus"`
	OS              string          `json:"os,omitempty"`
	Hostname        string          `json:"hostname,omitempty"`
	Secret          json.RawMessage `json:"secret,omitempty"`
}

// PublicView 是不含 secret 的对外形状（列表 / 详情返回用）。
type PublicView struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Host            string  `json:"host"`
	Port            int     `json:"port"`
	Username        string  `json:"username"`
	AuthType        string  `json:"authType"`
	Note            string  `json:"note"`
	CreatedAt       string  `json:"createdAt"`
	LastConnectedAt *string `json:"lastConnectedAt"`
	LastStatus      string  `json:"lastStatus"`
	OS              string  `json:"os,omitempty"`
	Hostname        string  `json:"hostname,omitempty"`
}

// Credentials 是解密后的 SSH 凭据。
type Credentials struct {
	Password   string `json:"password"`
	PrivateKey string `json:"privateKey"`
	Passphrase string `json:"passphrase"`
}

// ServerWithSecret 同时带档案与解密后凭据，业务层直接用它连 SSH。
type ServerWithSecret struct {
	PublicView
	Credentials  *Credentials
	SecretBroken bool `json:"secretBroken,omitempty"`
}

// ServerInput 是新增 / 编辑服务器时的入参。
type ServerInput struct {
	Name       string `json:"name"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"username"`
	AuthType   string `json:"authType"`
	Note       string `json:"note"`
	Password   string `json:"password"`
	PrivateKey string `json:"privateKey"`
	Passphrase string `json:"passphrase"`
}

// LogEntry 是 logs/<日期>.jsonl 里的一行。
type LogEntry struct {
	TS   string `json:"ts"`
	Type string `json:"type"`
	// 其余字段用 map 保留，写什么读什么
	Extra map[string]any `json:"-"`
}

// Paths 暴露数据目录给业务层（备份保存位置等）。
type Paths struct {
	DataDir   string
	LogDir    string
	BackupDir string
}

// Store 持有数据目录与互斥锁；所有方法并发安全。
type Store struct {
	mu      sync.Mutex
	dataDir string
	key     []byte
	Paths   Paths
}

// Open 初始化数据目录并加载（或生成）密钥。
func Open(dataDir string) (*Store, error) {
	s := &Store{dataDir: dataDir}
	s.Paths = Paths{
		DataDir:   dataDir,
		LogDir:    filepath.Join(dataDir, "logs"),
		BackupDir: filepath.Join(dataDir, "backups"),
	}
	for _, dir := range []string{dataDir, s.Paths.LogDir, s.Paths.BackupDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}

	keyFile := filepath.Join(dataDir, "secret.key")
	raw, err := os.ReadFile(keyFile)
	if err == nil {
		key, err2 := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err2 != nil || len(key) != keyLen {
			return nil, fmt.Errorf("secret.key 内容异常（长度 %d），删除后重启可重新生成", len(key))
		}
		s.key = key
		return s, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	// 生成新密钥（与早期实现相同的 base64 单行格式）
	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		return nil, err
	}
	s.key = key
	return s, nil
}

func (s *Store) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Encrypt 输出与早期实现一致的 {iv, tag, data}（全 base64）。
func (s *Store) Encrypt(plaintext string) (*SecretBox, error) {
	aead, err := s.gcm()
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

// Decrypt 解早期实现写入的 {iv, tag, data}。
func (s *Store) Decrypt(box *SecretBox) (string, error) {
	aead, err := s.gcm()
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

// ---------------------------------------------------------------- servers.json

type dbFile struct {
	Servers []json.RawMessage `json:"servers"`
}

func (s *Store) dbPath() string { return filepath.Join(s.dataDir, "servers.json") }

// rawServers 读出未解析的记录，保留未知字段。
func (s *Store) rawServers() ([]json.RawMessage, error) {
	raw, err := os.ReadFile(s.dbPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var db dbFile
	if err := json.Unmarshal(raw, &db); err != nil {
		return nil, nil // 损坏文件当作空库，与早期实现一致
	}
	return db.Servers, nil
}

func (s *Store) writeRaw(list []json.RawMessage) error {
	out, err := json.MarshalIndent(dbFile{Servers: list}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.dbPath() + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.dbPath())
}

// decode 把原始 JSON 变成结构体。
func decode(raw json.RawMessage) (*ServerRecord, error) {
	var r ServerRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// encodeFull 把整条记录（含 secret）序列化回去。
// 之前 encode() 只写 PublicView 字段，导致 MarkServer/UpdateServer 一落盘
// 就把加密凭据弄丢，服务器全部变成「凭据读取失败」。
func encodeFull(r *ServerRecord) (json.RawMessage, error) {
	type fullRecord struct {
		ID              string          `json:"id"`
		Name            string          `json:"name"`
		Host            string          `json:"host"`
		Port            int             `json:"port"`
		Username        string          `json:"username"`
		AuthType        string          `json:"authType"`
		Note            string          `json:"note"`
		CreatedAt       string          `json:"createdAt"`
		LastConnectedAt *string         `json:"lastConnectedAt"`
		LastStatus      string          `json:"lastStatus"`
		OS              string          `json:"os,omitempty"`
		Hostname        string          `json:"hostname,omitempty"`
		Secret          json.RawMessage `json:"secret,omitempty"`
	}
	return json.Marshal(fullRecord{
		ID: r.ID, Name: r.Name, Host: r.Host, Port: r.Port, Username: r.Username,
		AuthType: r.AuthType, Note: r.Note, CreatedAt: r.CreatedAt,
		LastConnectedAt: r.LastConnectedAt, LastStatus: r.LastStatus,
		OS: r.OS, Hostname: r.Hostname, Secret: r.Secret,
	})
}

// encode 把结构体变回原始 JSON（丢掉凭据后）。
func encode(r *ServerRecord) (json.RawMessage, error) {
	v := PublicView{
		ID: r.ID, Name: r.Name, Host: r.Host, Port: r.Port, Username: r.Username,
		AuthType: r.AuthType, Note: r.Note, CreatedAt: r.CreatedAt,
		LastConnectedAt: r.LastConnectedAt, LastStatus: r.LastStatus,
		OS: r.OS, Hostname: r.Hostname,
	}
	return json.Marshal(v)
}

func publicView(r *ServerRecord) PublicView {
	return PublicView{
		ID: r.ID, Name: r.Name, Host: r.Host, Port: r.Port, Username: r.Username,
		AuthType: r.AuthType, Note: r.Note, CreatedAt: r.CreatedAt,
		LastConnectedAt: r.LastConnectedAt, LastStatus: r.LastStatus,
		OS: r.OS, Hostname: r.Hostname,
	}
}

// NewID 生成 srv_ 开头的 16 位十六进制 ID，与早期实现同格式。
func NewID(prefix string) string {
	if prefix == "" {
		prefix = "srv"
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s_%x", prefix, b)
}

func nowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// ListServers 返回不含凭据的服务器列表。
func (s *Store) ListServers() ([]PublicView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raws, err := s.rawServers()
	if err != nil {
		return nil, err
	}
	out := make([]PublicView, 0, len(raws))
	for _, raw := range raws {
		r, err := decode(raw)
		if err != nil {
			continue
		}
		out = append(out, publicView(r))
	}
	return out, nil
}

// GetServerWithSecret 返回档案 + 解密凭据；secret 损坏时 Credentials 为 nil。
func (s *Store) GetServerWithSecret(id string) (*ServerWithSecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raws, err := s.rawServers()
	if err != nil {
		return nil, err
	}
	for _, raw := range raws {
		r, err := decode(raw)
		if err != nil || r.ID != id {
			continue
		}
		out := &ServerWithSecret{PublicView: publicView(r)}
		if len(r.Secret) > 0 {
			var box SecretBox
			if err := json.Unmarshal(r.Secret, &box); err == nil && box.Data != "" {
				plain, err := s.Decrypt(&box)
				if err == nil {
					var creds Credentials
					if json.Unmarshal([]byte(plain), &creds) == nil {
						out.Credentials = &creds
					}
				}
			}
		}
		return out, nil
	}
	return nil, nil
}

// AddServer 新增一条记录（凭据加密后落盘）。
func (s *Store) AddServer(in ServerInput) (PublicView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	creds, _ := json.Marshal(Credentials{
		Password: in.Password, PrivateKey: in.PrivateKey, Passphrase: in.Passphrase,
	})
	box, err := s.Encrypt(string(creds))
	if err != nil {
		return PublicView{}, err
	}
	boxRaw, _ := json.Marshal(box)

	authType := "password"
	if in.AuthType == "key" {
		authType = "key"
	}
	name := in.Name
	if name == "" {
		name = in.Host
	}
	if len(name) > 60 {
		name = name[:60]
	}
	username := strings.TrimSpace(in.Username)
	if username == "" {
		username = "root"
	}
	port := in.Port
	if port == 0 {
		port = 22
	}
	note := in.Note
	if len(note) > 200 {
		note = note[:200]
	}

	rec := ServerRecord{
		ID: NewID("srv"), Name: name, Host: strings.TrimSpace(in.Host), Port: port,
		Username: username, AuthType: authType, Note: note,
		CreatedAt: nowISO(), LastStatus: "unknown", Secret: boxRaw,
	}
	raw, err := encodeFull(&rec)
	if err != nil {
		return PublicView{}, err
	}
	raws, err := s.rawServers()
	if err != nil {
		return PublicView{}, err
	}
	if err := s.writeRaw(append(raws, raw)); err != nil {
		return PublicView{}, err
	}
	return publicView(&rec), nil
}

// UpdateServer 按 ID 更新；只有填了新凭据才覆盖旧的。
func (s *Store) UpdateServer(id string, in ServerInput) (*PublicView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raws, err := s.rawServers()
	if err != nil {
		return nil, err
	}
	for i, raw := range raws {
		r, err := decode(raw)
		if err != nil || r.ID != id {
			continue
		}
		if in.Name != "" {
			r.Name = truncate(in.Name, 60)
		}
		if in.Host != "" {
			r.Host = strings.TrimSpace(in.Host)
		}
		if in.Port != 0 {
			r.Port = in.Port
		}
		if in.Username != "" {
			r.Username = strings.TrimSpace(in.Username)
		}
		if in.Note != "" {
			r.Note = truncate(in.Note, 200)
		}
		if in.AuthType == "key" || in.AuthType == "password" {
			r.AuthType = in.AuthType
		}
		if in.Password != "" || in.PrivateKey != "" || in.Passphrase != "" {
			prev := Credentials{}
			if len(r.Secret) > 0 {
				var box SecretBox
				if json.Unmarshal(r.Secret, &box) == nil {
					if plain, err := s.Decrypt(&box); err == nil {
						_ = json.Unmarshal([]byte(plain), &prev)
					}
				}
			}
			if in.Password != "" {
				prev.Password = in.Password
			}
			if in.PrivateKey != "" {
				prev.PrivateKey = in.PrivateKey
			}
			if in.Passphrase != "" {
				prev.Passphrase = in.Passphrase
			}
			creds, _ := json.Marshal(prev)
			box, err := s.Encrypt(string(creds))
			if err != nil {
				return nil, err
			}
			r.Secret, _ = json.Marshal(box)
		}
		out, err := encodeFull(r)
		if err != nil {
			return nil, err
		}
		raws[i] = out
		if err := s.writeRaw(raws); err != nil {
			return nil, err
		}
		v := publicView(r)
		return &v, nil
	}
	return nil, nil
}

// MarkServer 打补丁（lastStatus / os / hostname 等）。
func (s *Store) MarkServer(id string, patch map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	raws, err := s.rawServers()
	if err != nil {
		return err
	}
	for i, raw := range raws {
		r, err := decode(raw)
		if err != nil || r.ID != id {
			continue
		}
		if v, ok := patch["lastConnectedAt"].(string); ok {
			r.LastConnectedAt = &v
		}
		if v, ok := patch["lastStatus"].(string); ok {
			r.LastStatus = v
		}
		if v, ok := patch["os"].(string); ok {
			r.OS = v
		}
		if v, ok := patch["hostname"].(string); ok {
			r.Hostname = v
		}
		out, err := encodeFull(r)
		if err != nil {
			return err
		}
		raws[i] = out
		return s.writeRaw(raws)
	}
	return nil
}

// RemoveServer 删除一条记录。
func (s *Store) RemoveServer(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raws, err := s.rawServers()
	if err != nil {
		return false, err
	}
	out := raws[:0]
	found := false
	for _, raw := range raws {
		r, err := decode(raw)
		if err == nil && r.ID == id {
			found = true
			continue
		}
		out = append(out, raw)
	}
	if !found {
		return false, nil
	}
	return true, s.writeRaw(out)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---------------------------------------------------------------- 日志

// AppendLog 追加一行 JSONL。entry 里已有 ts 时不覆盖。
func (s *Store) AppendLog(entry map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := entry["ts"]; !ok {
		entry["ts"] = nowISO()
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	day := time.Now().UTC().Format("2006-01-02")
	path := filepath.Join(s.Paths.LogDir, day+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// ReadLogs 倒序读最近 limit 条（跨天文件，从新到旧）。
func (s *Store) ReadLogs(limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 300 {
		limit = 300
	}
	entries, err := os.ReadDir(s.Paths.LogDir)
	if err != nil {
		return nil, nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	out := make([]map[string]any, 0, limit)
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(s.Paths.LogDir, name))
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if lines[i] == "" {
				continue
			}
			var m map[string]any
			if json.Unmarshal([]byte(lines[i]), &m) == nil {
				out = append(out, m)
			}
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}
