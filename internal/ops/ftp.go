package ops

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"wpqu-panel/internal/sshc"
)

// FTP 组件（vsftpd）的按需安装与管理。
// 所有逻辑都在 scripts/ftp.sh 里，这里只负责传参、解析和超时。

// FTPUser 是一个面板创建的 FTP 账号。
type FTPUser struct {
	Name     string `json:"name"`
	Dir      string `json:"dir"`
	Writable bool   `json:"writable"`
	Active   bool   `json:"active"`
}

// FTPStatus 是组件与账号的完整状态。
type FTPStatus struct {
	Installed bool      `json:"installed"`
	Running   bool      `json:"running"`
	Version   string    `json:"version"`
	Port      int       `json:"port"`
	PasvMin   int       `json:"pasvMin"`
	PasvMax   int       `json:"pasvMax"`
	PasvAddr  string    `json:"pasvAddr"`
	PublicIP  string    `json:"publicIp"`
	Firewall  string    `json:"firewall"`
	Users     []FTPUser `json:"users"`
}

// FTPParams 是一次 FTP 操作的参数。
type FTPParams struct {
	Action   string // status/install/uninstall/start/stop/restart/config/adduser/deluser/setdir/setpass/setrw
	User     string
	Password string
	Dir      string
	ReadOnly bool
	PasvMin  int
	PasvMax  int
	PasvAddr string
}

// FTP 执行一次 FTP 操作，返回最新状态和脚本原始结果。
func (r *Runner) FTP(server sshc.Server, p FTPParams, onData func(string)) (*FTPStatus, *sshc.Result, error) {
	env := map[string]string{"FTP_ACTION": p.Action}
	if p.User != "" {
		env["FTP_USER"] = p.User
	}
	if p.Password != "" {
		env["FTP_PASS_B64"] = base64.StdEncoding.EncodeToString([]byte(p.Password))
	}
	if p.Dir != "" {
		env["FTP_DIR"] = p.Dir
	}
	if p.PasvMin > 0 {
		env["FTP_PASV_MIN"] = strconv.Itoa(p.PasvMin)
	}
	if p.PasvMax > 0 {
		env["FTP_PASV_MAX"] = strconv.Itoa(p.PasvMax)
	}
	if p.PasvAddr != "" {
		env["FTP_PASV_ADDR"] = p.PasvAddr
	}
	switch p.Action {
	case "adduser", "setrw":
		if p.ReadOnly {
			env["FTP_RW"] = "ro"
		} else {
			env["FTP_RW"] = "rw"
		}
	}

	// 安装/卸载要跑 apt，给足时间；其余都是秒级
	timeout := 120 * time.Second
	switch p.Action {
	case "install", "uninstall":
		timeout = 8 * time.Minute
	}

	res, err := r.RunUploadedScript(server, "ftp.sh", env, onData, timeout)
	if err != nil {
		return nil, nil, err
	}
	st := ParseFTPStatus(StripANSI(res.Stdout))
	if res.Code != 0 {
		msg := LastErrorLine(StripANSI(res.Stdout + "\n" + res.Stderr))
		if msg == "" {
			msg = "FTP 操作失败"
		}
		return st, res, fmt.Errorf("%s", msg)
	}
	return st, res, nil
}

// LastErrorLine 取 [err] 开头的最后一行，没有就返回空。
func LastErrorLine(out string) string {
	msg := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[err]") {
			msg = strings.TrimSpace(strings.TrimPrefix(line, "[err]"))
		}
	}
	return msg
}

// ParseFTPStatus 从脚本输出里提取 PANEL_ 开头的键值。
func ParseFTPStatus(out string) *FTPStatus {
	st := &FTPStatus{Port: 21, Users: []FTPUser{}}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "PANEL_") {
			continue
		}
		kv := strings.SplitN(strings.TrimPrefix(line, "PANEL_"), "=", 2)
		if len(kv) != 2 {
			continue
		}
		key, val := kv[0], kv[1]
		switch key {
		case "INSTALLED":
			st.Installed = val == "1"
		case "RUNNING":
			st.Running = val == "1"
		case "VERSION":
			st.Version = val
		case "PORT":
			if n, err := strconv.Atoi(val); err == nil {
				st.Port = n
			}
		case "PASV_MIN":
			if n, err := strconv.Atoi(val); err == nil {
				st.PasvMin = n
			}
		case "PASV_MAX":
			if n, err := strconv.Atoi(val); err == nil {
				st.PasvMax = n
			}
		case "PASV_ADDR":
			st.PasvAddr = val
		case "PUBLIC_IP":
			st.PublicIP = val
		case "FW":
			st.Firewall = val
		case "USER":
			parts := strings.Split(val, "|")
			if len(parts) != 4 {
				continue
			}
			st.Users = append(st.Users, FTPUser{
				Name:     parts[0],
				Dir:      parts[1],
				Writable: parts[2] == "rw",
				Active:   parts[3] == "active",
			})
		}
	}
	if !st.Installed {
		st.Users = []FTPUser{}
	}
	return st
}
