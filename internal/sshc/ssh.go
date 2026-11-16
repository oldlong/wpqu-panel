// Package sshc：SSH 连接池 + 命令执行 + SFTP 文件操作。
// 对应早期实现的 SSH 封装行为（错误文案也保持一致）。
package sshc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const defaultTimeout = 120 * time.Second

// Server 是业务层传入的最小连接信息（来自 store.ServerWithSecret）。
type Server struct {
	ID         string
	Host       string
	Port       int
	Username   string
	AuthType   string // password | key
	Password   string
	PrivateKey string
	Passphrase string
}

// Result 是一次命令执行的结果。
type Result struct {
	Code     int
	Stdout   string
	Stderr   string
	TimedOut bool
}

// ExecOptions 控制超时与输出回调。
type ExecOptions struct {
	Timeout time.Duration
	OnData  func(text string, isStderr bool)
}

type poolEntry struct {
	client *ssh.Client
	ready  bool
}

// Pool 按 serverId 复用 SSH 连接。
type Pool struct {
	mu      sync.Mutex
	clients map[string]*poolEntry
	pending map[string]chan struct{}
}

func NewPool() *Pool {
	return &Pool{clients: map[string]*poolEntry{}, pending: map[string]chan struct{}{}}
}

func describeError(err error, server Server) string {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		if netErr.Err != nil && strings.Contains(netErr.Err.Error(), "connection refused") {
			return fmt.Sprintf("连接被拒绝（%s:%d）。确认 SSH 端口是否正确、云服务器安全组是否放行。", server.Host, server.Port)
		}
	}
	if strings.Contains(msg, "no such host") || strings.Contains(msg, "lookup") {
		return fmt.Sprintf("无法解析主机名 %s，检查地址或本机 DNS。", server.Host)
	}
	if strings.Contains(msg, "i/o timeout") || strings.Contains(strings.ToLower(msg), "timed out") {
		return fmt.Sprintf("连接超时（%s:%d）。检查安全组、防火墙，或服务器是否在线。", server.Host, server.Port)
	}
	if strings.Contains(msg, "unable to authenticate") || strings.Contains(msg, "authentication") {
		return "SSH 认证失败：用户名、密码或密钥不正确。密钥方式请确认公钥已写入服务器的 ~/.ssh/authorized_keys。"
	}
	if strings.Contains(msg, "failed to unmarshal private key") || strings.Contains(msg, "parse private key") {
		return "私钥格式无法解析。请粘贴完整的 PEM/OpenSSH 私钥（以 -----BEGIN 开头）。"
	}
	if strings.Contains(msg, "encrypted") && strings.Contains(msg, "passphrase") {
		return "私钥带了口令，但没有填写 passphrase。请补充密钥口令。"
	}
	if strings.Contains(msg, "connection reset") {
		return "连接被重置。服务器可能限制了来源 IP，或 SSH 服务异常。"
	}
	return msg
}

func (p *Pool) connect(server Server) (*ssh.Client, error) {
	var auth []ssh.AuthMethod
	if server.AuthType == "key" {
		var signer ssh.Signer
		var err error
		if server.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(server.PrivateKey), []byte(server.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(server.PrivateKey))
		}
		if err != nil {
			if _, ok := err.(*ssh.PassphraseMissingError); ok {
				return nil, errors.New("私钥带了口令，但没有填写 passphrase。请补充密钥口令。")
			}
			return nil, errors.New("私钥格式无法解析。请粘贴完整的 PEM/OpenSSH 私钥（以 -----BEGIN 开头）。")
		}
		auth = append(auth, ssh.PublicKeys(signer))
	} else {
		auth = append(auth, ssh.Password(server.Password), ssh.KeyboardInteractive(func(_, _ string, _ []string, _ []bool) ([]string, error) {
			return []string{server.Password}, nil
		}))
	}

	cfg := &ssh.ClientConfig{
		User:            server.Username,
		Auth:            auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // 管理面板场景：信任已保存的服务器
		Timeout:         20 * time.Second,
	}
	addr := fmt.Sprintf("%s:%d", server.Host, server.Port)
	if server.Port == 0 {
		addr = fmt.Sprintf("%s:22", server.Host)
	}
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, errors.New(describeError(err, server))
	}
	return client, nil
}

// GetClient 返回复用的连接；不在池里就新建（并发调用只建一条）。
func (p *Pool) GetClient(server Server) (*ssh.Client, error) {
	p.mu.Lock()
	if entry, ok := p.clients[server.ID]; ok && entry.ready {
		p.mu.Unlock()
		return entry.client, nil
	}
	if ch, ok := p.pending[server.ID]; ok {
		p.mu.Unlock()
		<-ch
		return p.GetClient(server)
	}
	done := make(chan struct{})
	p.pending[server.ID] = done
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		delete(p.pending, server.ID)
		close(done)
		p.mu.Unlock()
	}()

	client, err := p.connect(server)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.clients[server.ID] = &poolEntry{client: client, ready: true}
	p.mu.Unlock()
	return client, nil
}

// CloseConnection 主动断开某台服务器的连接。
func (p *Pool) CloseConnection(serverID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry, ok := p.clients[serverID]; ok {
		_ = entry.client.Close()
		delete(p.clients, serverID)
	}
}

// CloseAll 关闭全部连接。
func (p *Pool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id := range p.clients {
		_ = p.clients[id].client.Close()
		delete(p.clients, id)
	}
}

// GetClientOrRetry 在连接失效（远端重启 / 断网恢复）时重建一次。
func (p *Pool) GetClientOrRetry(server Server) (*ssh.Client, error) {
	client, err := p.GetClient(server)
	if err != nil {
		// 连接失败可能是池里的旧连接已死，清掉重试一次
		p.CloseConnection(server.ID)
		client, err = p.GetClient(server)
		if err != nil {
			return nil, err
		}
	}
	// 再探活一次：TCP 未断但 SSH 会话已死的情况
	if _, err := doExec(client, "true", &ExecOptions{Timeout: 10 * time.Second}); err != nil {
		p.CloseConnection(server.ID)
		client, err = p.GetClient(server)
		if err != nil {
			return nil, err
		}
	}
	return client, nil
}

func doExec(client *ssh.Client, command string, opts *ExecOptions) (*Result, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	var timeout = defaultTimeout
	if opts != nil && opts.Timeout > 0 {
		timeout = opts.Timeout
	}

	done := make(chan error, 1)
	if err := session.Start(command); err != nil {
		return nil, err
	}
	go func() { done <- session.Wait() }()

	// Start 后拿不到流式输出（已绑 Buffer），流式场景走 ExecStream
	select {
	case err := <-done:
		code := 0
		if err != nil {
			var exitErr *ssh.ExitError
			if errors.As(err, &exitErr) {
				code = exitErr.ExitStatus()
			} else {
				return &Result{Code: -1, Stdout: stdout.String(), Stderr: stderr.String()}, nil
			}
		}
		return &Result{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}, nil
	case <-time.After(timeout):
		_ = session.Close()
		return &Result{Code: -1, Stdout: stdout.String(), Stderr: stderr.String(), TimedOut: true}, nil
	}
}

// Exec 执行命令；支持流式回调（OnData）与超时。
func (p *Pool) Exec(server Server, command string, opts *ExecOptions) (*Result, error) {
	client, err := p.GetClientOrRetry(server)
	if err != nil {
		return nil, err
	}

	if opts == nil || opts.OnData == nil {
		return doExec(client, command, opts)
	}

	// 流式版本：手动泵 stdout/stderr
	session, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()

	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := session.StderrPipe()
	if err != nil {
		return nil, err
	}

	var outBuf, errBuf bytes.Buffer
	var mu sync.Mutex
	streamCopy := func(r io.Reader, isErr bool) {
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				text := string(buf[:n])
				mu.Lock()
				if isErr {
					errBuf.WriteString(text)
				} else {
					outBuf.WriteString(text)
				}
				mu.Unlock()
				if opts.OnData != nil {
					opts.OnData(text, isErr)
				}
			}
			if err != nil {
				return
			}
		}
	}

	if err := session.Start(command); err != nil {
		return nil, err
	}
	go streamCopy(stdoutPipe, false)
	go streamCopy(stderrPipe, true)

	var timeout = defaultTimeout
	if opts.Timeout > 0 {
		timeout = opts.Timeout
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()

	select {
	case err := <-done:
		code := 0
		if err != nil {
			var exitErr *ssh.ExitError
			if errors.As(err, &exitErr) {
				code = exitErr.ExitStatus()
			} else {
				code = -1
			}
		}
		mu.Lock()
		defer mu.Unlock()
		return &Result{Code: code, Stdout: outBuf.String(), Stderr: errBuf.String()}, nil
	case <-time.After(timeout):
		_ = session.Close()
		mu.Lock()
		defer mu.Unlock()
		return &Result{Code: -1, Stdout: outBuf.String(), Stderr: errBuf.String(), TimedOut: true}, nil
	}
}

// WithRoot 以 root 身份执行：非 root 账号走免密 sudo。
func WithRoot(server Server, command string) string {
	if server.Username == "root" {
		return command
	}
	return fmt.Sprintf("sudo -n bash -c %s", ShellQuote(command))
}

// ShellQuote 单引号包裹。
func ShellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// ---------------------------------------------------------------- SFTP

func (p *Pool) sftp(server Server) (*sftp.Client, error) {
	client, err := p.GetClientOrRetry(server)
	if err != nil {
		return nil, err
	}
	return sftp.NewClient(client)
}

// DirItem 是目录列表的一项。
type DirItem struct {
	Name  string `json:"name"`
	Type  string `json:"type"` // dir | file | link
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"` // 毫秒
	Mode  uint32 `json:"mode"`
}

// ListDir 列目录（目录在前，按名排序）。
func (p *Pool) ListDir(server Server, dir string) ([]DirItem, error) {
	sc, err := p.sftp(server)
	if err != nil {
		return nil, fmt.Errorf("读取目录失败：%v", err)
	}
	defer sc.Close()

	infos, err := sc.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取目录失败：%v", err)
	}
	items := make([]DirItem, 0, len(infos))
	for _, fi := range infos {
		t := "file"
		if fi.IsDir() {
			t = "dir"
		} else if fi.Mode()&os.ModeSymlink != 0 {
			t = "link"
		}
		items = append(items, DirItem{
			Name:  fi.Name(),
			Type:  t,
			Size:  fi.Size(),
			Mtime: fi.ModTime().UnixMilli(),
			Mode:  posixMode(fi),
		})
	}
	// 目录在前，同类型按名（与早期实现的 localeCompare 行为接近）
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if (a.Type == "dir") != (b.Type == "dir") {
			return a.Type == "dir"
		}
		la, lb := strings.ToLower(a.Name), strings.ToLower(b.Name)
		if la != lb {
			return la < lb
		}
		return a.Name < b.Name
	})
	return items, nil
}

// posixMode 把 Go 的 FileMode 还原成 POSIX st_mode（与早期实现的 mode 一致）。
func posixMode(fi os.FileInfo) uint32 {
	m := fi.Mode()
	var t uint32
	switch {
	case m.IsDir():
		t = 0o040000
	case m&os.ModeSymlink != 0:
		t = 0o120000
	case m&os.ModeNamedPipe != 0:
		t = 0o010000
	case m&os.ModeSocket != 0:
		t = 0o140000
	case m&os.ModeCharDevice != 0:
		t = 0o020000
	case m&os.ModeDevice != 0:
		t = 0o060000
	default:
		t = 0o100000
	}
	t |= uint32(m.Perm())
	// setuid / setgid / sticky：os.FileMode 用独立位表示，不能漏，
	// 否则 /tmp 这种 1777 会显示成 777，改权限时还会被抹掉
	if m&os.ModeSetuid != 0 {
		t |= 0o4000
	}
	if m&os.ModeSetgid != 0 {
		t |= 0o2000
	}
	if m&os.ModeSticky != 0 {
		t |= 0o1000
	}
	return t
}

// StatInfo 是 stat 结果。
type StatInfo struct {
	Type  string `json:"type"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
	Mode  uint32 `json:"mode"`
}

// StatPath 查看远端路径属性。
func (p *Pool) StatPath(server Server, target string) (*StatInfo, error) {
	sc, err := p.sftp(server)
	if err != nil {
		return nil, err
	}
	defer sc.Close()
	fi, err := sc.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("路径不存在或无权访问：%v", err)
	}
	t := "file"
	if fi.IsDir() {
		t = "dir"
	}
	return &StatInfo{Type: t, Size: fi.Size(), Mtime: fi.ModTime().UnixMilli(), Mode: posixMode(fi)}, nil
}

// ReadFile 读小文件（≤2MB，拒绝二进制）。
func (p *Pool) ReadFile(server Server, target string) (map[string]any, error) {
	sc, err := p.sftp(server)
	if err != nil {
		return nil, err
	}
	defer sc.Close()
	fi, err := sc.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("读取失败：%v", err)
	}
	const maxBytes = 2 * 1024 * 1024
	if fi.Size() > maxBytes {
		return nil, fmt.Errorf("文件过大（%.1f MB），请下载后本地编辑", float64(fi.Size())/1048576)
	}
	f, err := sc.Open(target)
	if err != nil {
		return nil, fmt.Errorf("读取失败：%v", err)
	}
	defer f.Close()
	buf, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("读取失败：%v", err)
	}
	if bytes.IndexByte(buf, 0) >= 0 {
		return nil, errors.New("这是二进制文件，不支持在线预览")
	}
	return map[string]any{
		"content": string(buf),
		"size":    fi.Size(),
		"mtime":   fi.ModTime().UnixMilli(),
	}, nil
}

// WriteFile 写文本文件。
func (p *Pool) WriteFile(server Server, target, content string) error {
	sc, err := p.sftp(server)
	if err != nil {
		return fmt.Errorf("写入失败：%v", err)
	}
	defer sc.Close()
	f, err := sc.Create(target)
	if err != nil {
		return fmt.Errorf("写入失败：%v", err)
	}
	defer f.Close()
	if _, err := f.Write([]byte(content)); err != nil {
		return fmt.Errorf("写入失败：%v", err)
	}
	return nil
}

// Mkdir 建目录。
func (p *Pool) Mkdir(server Server, target string) error {
	sc, err := p.sftp(server)
	if err != nil {
		return fmt.Errorf("创建目录失败：%v", err)
	}
	defer sc.Close()
	if err := sc.Mkdir(target); err != nil {
		return fmt.Errorf("创建目录失败：%v", err)
	}
	return nil
}

// Chmod 改权限。mode 形如 "755"、"644"、"4755"；recursive 走 chmod -R。
func (p *Pool) Chmod(server Server, target, mode string, recursive bool) error {
	flag := ""
	if recursive {
		flag = "-R "
	}
	cmd := "chmod " + flag + mode + " -- " + ShellQuote(target)
	res, err := p.Exec(server, WithRoot(server, cmd), &ExecOptions{Timeout: 45 * time.Second})
	if err != nil {
		return fmt.Errorf("修改权限失败：%v", err)
	}
	if res.Code != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = "修改权限失败"
		}
		return errors.New(msg)
	}
	return nil
}

// Rename 重命名 / 移动。
func (p *Pool) Rename(server Server, from, to string) error {
	sc, err := p.sftp(server)
	if err != nil {
		return fmt.Errorf("重命名失败：%v", err)
	}
	defer sc.Close()
	if err := sc.Rename(from, to); err != nil {
		return fmt.Errorf("重命名失败：%v", err)
	}
	return nil
}

// RemoveAll 批量删除（rm -rf），一次命令删完。
func (p *Pool) RemoveAll(server Server, targets []string) error {
	if len(targets) == 0 {
		return nil
	}
	quoted := make([]string, 0, len(targets))
	for _, t := range targets {
		quoted = append(quoted, ShellQuote(t))
	}
	cmd := "rm -rf -- " + strings.Join(quoted, " ")
	res, err := p.Exec(server, WithRoot(server, cmd), &ExecOptions{Timeout: 120 * time.Second})
	if err != nil {
		return fmt.Errorf("删除失败：%v", err)
	}
	if res.Code != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = "删除失败"
		}
		return errors.New(msg)
	}
	return nil
}

// BatchResult 是一次批量复制/移动的结果。
type BatchResult struct {
	From []string `json:"from"`
	To   []string `json:"to"`
}

// Copy 把 sources 复制进 dest 目录（cp -a）。dest 与源同目录时自动加 -copy 后缀。
func (p *Pool) Copy(server Server, sources []string, dest string) (*BatchResult, error) {
	return p.transfer(server, sources, dest, false)
}

// Move 把 sources 移动进 dest 目录（mv）。目标已存在时报错，不覆盖。
func (p *Pool) Move(server Server, sources []string, dest string) (*BatchResult, error) {
	return p.transfer(server, sources, dest, true)
}

// transfer 是复制/移动的公共实现：先算出每个目标路径，再逐条执行。
func (p *Pool) transfer(server Server, sources []string, dest string, isMove bool) (*BatchResult, error) {
	if len(sources) == 0 {
		return &BatchResult{}, nil
	}
	dest = strings.TrimRight(dest, "/")
	if dest == "" {
		dest = "/"
	}

	sc, err := p.sftp(server)
	if err != nil {
		return nil, err
	}
	defer sc.Close()

	fi, err := sc.Stat(dest)
	if err != nil {
		return nil, fmt.Errorf("目标目录不存在：%s", dest)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("目标不是目录：%s", dest)
	}

	exists := func(target string) bool {
		if _, err := sc.Stat(target); err == nil {
			return true
		}
		return false
	}

	res := &BatchResult{}
	type pair struct{ from, to string }
	pairs := make([]pair, 0, len(sources))

	for _, src := range sources {
		src = strings.TrimRight(src, "/")
		base := src
		if i := strings.LastIndex(src, "/"); i >= 0 {
			base = src[i+1:]
		}
		if base == "" {
			return nil, fmt.Errorf("路径不合法：%s", src)
		}
		parent := src[:len(src)-len(base)]
		parent = strings.TrimRight(parent, "/")
		if parent == "" {
			parent = "/"
		}
		if parent == dest {
			if isMove {
				return nil, fmt.Errorf("%s 已在目标目录里，无需移动", base)
			}
			// 复制到原地：文件名加 -copy 后缀，避免 cp 报 same file
			target := joinPath(dest, base+"-copy")
			for i := 2; exists(target); i++ {
				target = joinPath(dest, fmt.Sprintf("%s-copy-%d", base, i))
			}
			pairs = append(pairs, pair{src, target})
			continue
		}
		target := joinPath(dest, base)
		if exists(target) {
			return nil, fmt.Errorf("目标已存在：%s（请改名或先移动/删除）", target)
		}
		pairs = append(pairs, pair{src, target})
	}

	verb := "cp -a"
	label := "复制"
	if isMove {
		verb = "mv"
		label = "移动"
	}
	for _, pr := range pairs {
		cmd := verb + " -- " + ShellQuote(pr.from) + " " + ShellQuote(pr.to)
		out, err := p.Exec(server, WithRoot(server, cmd), &ExecOptions{Timeout: 300 * time.Second})
		if err != nil {
			return res, fmt.Errorf("%s %s 失败：%v", label, pr.from, err)
		}
		if out.Code != 0 {
			msg := strings.TrimSpace(out.Stderr)
			if msg == "" {
				msg = label + "失败"
			}
			return res, fmt.Errorf("%s %s 失败：%s", label, pr.from, msg)
		}
		res.From = append(res.From, pr.from)
		res.To = append(res.To, pr.to)
	}
	return res, nil
}

func joinPath(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

// UploadBuffer 上传内存数据到远端路径。
func (p *Pool) UploadBuffer(server Server, target string, data []byte) error {
	sc, err := p.sftp(server)
	if err != nil {
		return fmt.Errorf("上传失败：%v", err)
	}
	defer sc.Close()
	f, err := sc.Create(target)
	if err != nil {
		return fmt.Errorf("上传失败：%v", err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("上传失败：%v", err)
	}
	return nil
}

// StreamDownload 把远端文件流式写到 w（HTTP 下载用）。
func (p *Pool) StreamDownload(server Server, target string, w io.Writer) error {
	sc, err := p.sftp(server)
	if err != nil {
		return fmt.Errorf("下载失败：%v", err)
	}
	defer sc.Close()
	f, err := sc.Open(target)
	if err != nil {
		return fmt.Errorf("下载失败：%v", err)
	}
	defer f.Close()
	if _, err := io.Copy(w, f); err != nil {
		return fmt.Errorf("下载失败：%v", err)
	}
	return nil
}

// DownloadToFile 下载到本地文件，onProgress 回调累计字节数。
func (p *Pool) DownloadToFile(server Server, remotePath, localPath string, onProgress func(received int64)) error {
	sc, err := p.sftp(server)
	if err != nil {
		return fmt.Errorf("下载失败：%v", err)
	}
	defer sc.Close()
	f, err := sc.Open(remotePath)
	if err != nil {
		return fmt.Errorf("下载失败：%v", err)
	}
	defer f.Close()
	out, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer out.Close()

	var received int64
	buf := make([]byte, 64*1024)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
			received += int64(n)
			if onProgress != nil {
				onProgress(received)
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// TestConnection 验证连通性并返回基础信息。
func (p *Pool) TestConnection(server Server) (map[string]any, error) {
	res, err := p.Exec(server, "id -u; hostname; cat /etc/os-release 2>/dev/null | head -2", &ExecOptions{Timeout: 20 * time.Second})
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = "连接成功但命令执行失败"
		}
		return nil, errors.New(msg)
	}
	lines := strings.Split(res.Stdout, "\n")
	uid := strings.TrimSpace(lineAt(lines, 0))
	os := strings.ReplaceAll(strings.ReplaceAll(lineAt(lines, 2), "PRETTY_NAME=", ""), "\"", "")
	return map[string]any{
		"uid":      uid,
		"isRoot":   uid == "0",
		"hostname": strings.TrimSpace(lineAt(lines, 1)),
		"os":       os,
	}, nil
}

func lineAt(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return ""
}
