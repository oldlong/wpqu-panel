package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s, dir
}

func addTestServer(t *testing.T, s *Store) string {
	t.Helper()
	v, err := s.AddServer(ServerInput{
		Name: "test", Host: "127.0.0.1", Port: 22, Username: "root",
		AuthType: "password", Password: "s3cret-ssh",
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	return v.ID
}

// 设了密码之后：机器密钥读不出凭据，必须解锁。
func TestVaultLocksCredentials(t *testing.T) {
	s, dir := newTestStore(t)
	id := addTestServer(t, s)

	if _, err := s.SetVaultPassword(id, "1234"); err != nil {
		t.Fatalf("set: %v", err)
	}

	// 刚设完是解锁状态（用户刚亲手输过密码）
	got, err := s.GetServerWithSecret(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Locked || got.Credentials == nil || got.Credentials.Password != "s3cret-ssh" {
		t.Fatalf("刚设完密码应可直接用，got locked=%v creds=%v", got.Locked, got.Credentials)
	}

	// 模拟重启：内存里的密钥没了
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err = s2.GetServerWithSecret(id)
	if err != nil {
		t.Fatalf("get2: %v", err)
	}
	if !got.Locked || got.Credentials != nil {
		t.Fatalf("重启后应处于未解锁状态，got locked=%v creds=%v", got.Locked, got.Credentials)
	}

	// 文件里不能留机器密钥那份明文
	raw, _ := os.ReadFile(filepath.Join(dir, "servers.json"))
	var db dbFile
	if err := json.Unmarshal(raw, &db); err != nil {
		t.Fatalf("parse: %v", err)
	}
	var rec ServerRecord
	if err := json.Unmarshal(db.Servers[0], &rec); err != nil {
		t.Fatalf("rec: %v", err)
	}
	if len(rec.Secret) != 0 {
		t.Fatalf("设密码后不应再保留 secret 字段")
	}
	if rec.Vault == nil || !rec.Vault.Enabled {
		t.Fatalf("vault 字段应存在")
	}

	// 解锁
	if _, err := s2.UnlockVault(id, "1234"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	got, err = s2.GetServerWithSecret(id)
	if err != nil {
		t.Fatalf("get3: %v", err)
	}
	if got.Locked || got.Credentials == nil || got.Credentials.Password != "s3cret-ssh" {
		t.Fatalf("解锁后应能读到凭据，got locked=%v", got.Locked)
	}
}

// 连错 5 次锁 1 小时，且计数与锁定时间要落盘（重启不清零）。
func TestVaultLockoutAfterFiveFails(t *testing.T) {
	s, dir := newTestStore(t)
	id := addTestServer(t, s)
	if _, err := s.SetVaultPassword(id, "0000"); err != nil {
		t.Fatalf("set: %v", err)
	}
	s.LockVault(id) // 先丢掉解锁状态

	for i := 1; i <= 4; i++ {
		_, err := s.UnlockVault(id, "wrong")
		var bad *VaultBadPassword
		if !errors.As(err, &bad) {
			t.Fatalf("第 %d 次应返回密码错误，got %v", i, err)
		}
		if bad.Remaining != 5-i {
			t.Fatalf("第 %d 次剩余次数应为 %d，got %d", i, 5-i, bad.Remaining)
		}
	}

	_, err := s.UnlockVault(id, "wrong")
	var out *VaultLockedOut
	if !errors.As(err, &out) {
		t.Fatalf("第 5 次应触发锁定，got %v", err)
	}
	if out.Seconds < 3500 || out.Seconds > 3600 {
		t.Fatalf("锁定时间应接近 1 小时，got %d 秒", out.Seconds)
	}

	// 锁定期间即使密码正确也不放行
	if _, err := s.UnlockVault(id, "0000"); !errors.As(err, &out) {
		t.Fatalf("锁定期间正确密码也应被拒，got %v", err)
	}

	// 重启后仍然是锁定的
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := s2.UnlockVault(id, "0000"); !errors.As(err, &out) {
		t.Fatalf("重启不应解除锁定，got %v", err)
	}

	// 把锁定时间改到过去，模拟一小时之后
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	raw, _ := os.ReadFile(filepath.Join(dir, "servers.json"))
	var db dbFile
	_ = json.Unmarshal(raw, &db)
	var rec ServerRecord
	_ = json.Unmarshal(db.Servers[0], &rec)
	rec.Vault.LockedUntil = past
	patched, _ := encodeFull(&rec)
	db.Servers[0] = patched
	out2, _ := json.MarshalIndent(db, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "servers.json"), out2, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	s3, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen2: %v", err)
	}
	if _, err := s3.UnlockVault(id, "0000"); err != nil {
		t.Fatalf("锁定到期后应能解锁，got %v", err)
	}
}

func TestVaultChangeAndRemove(t *testing.T) {
	s, dir := newTestStore(t)
	id := addTestServer(t, s)
	if _, err := s.SetVaultPassword(id, "1111"); err != nil {
		t.Fatalf("set: %v", err)
	}

	// 原密码不对不改
	if _, err := s.ChangeVaultPassword(id, "9999", "2222"); err == nil {
		t.Fatalf("原密码错应失败")
	}
	if _, err := s.ChangeVaultPassword(id, "1111", "2222"); err != nil {
		t.Fatalf("change: %v", err)
	}

	s2, _ := Open(dir)
	if _, err := s2.UnlockVault(id, "1111"); err == nil {
		t.Fatalf("旧密码应失效")
	}
	if _, err := s2.UnlockVault(id, "2222"); err != nil {
		t.Fatalf("新密码应可用：%v", err)
	}
	got, _ := s2.GetServerWithSecret(id)
	if got.Credentials == nil || got.Credentials.Password != "s3cret-ssh" {
		t.Fatalf("换密码后凭据应保持完整")
	}

	// 关闭加密：凭据回到机器密钥加密，重启也能直接读
	if _, err := s2.RemoveVault(id, "2222"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	s3, _ := Open(dir)
	got, err := s3.GetServerWithSecret(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Locked || got.HasVault {
		t.Fatalf("关闭后不应再锁定，got locked=%v hasVault=%v", got.Locked, got.HasVault)
	}
	if got.Credentials == nil || got.Credentials.Password != "s3cret-ssh" {
		t.Fatalf("关闭后凭据应可读")
	}
}

// 改凭据时必须先解锁，改完仍然用密码派生的密钥保存。
func TestVaultUpdateCredentialsNeedsUnlock(t *testing.T) {
	s, dir := newTestStore(t)
	id := addTestServer(t, s)
	if _, err := s.SetVaultPassword(id, "abcd"); err != nil {
		t.Fatalf("set: %v", err)
	}
	s2, _ := Open(dir)

	if _, err := s2.UpdateServer(id, ServerInput{Password: "new-pass"}); err == nil {
		t.Fatalf("未解锁时改凭据应失败")
	} else {
		var need *VaultNeedPassword
		if !errors.As(err, &need) {
			t.Fatalf("应返回需要密码的错误，got %v", err)
		}
	}

	if _, err := s2.UnlockVault(id, "abcd"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if _, err := s2.UpdateServer(id, ServerInput{Password: "new-pass"}); err != nil {
		t.Fatalf("解锁后改凭据应成功：%v", err)
	}
	got, _ := s2.GetServerWithSecret(id)
	if got.Credentials == nil || got.Credentials.Password != "new-pass" {
		t.Fatalf("新凭据应生效，got %+v", got.Credentials)
	}
	// 落盘仍是密码加密，机器密钥读不出来
	raw, _ := os.ReadFile(filepath.Join(dir, "servers.json"))
	var db dbFile
	_ = json.Unmarshal(raw, &db)
	var rec ServerRecord
	_ = json.Unmarshal(db.Servers[0], &rec)
	if len(rec.Secret) != 0 || rec.Vault == nil {
		t.Fatalf("改完凭据后仍应只保留 vault 加密")
	}
}

// MarkServer（打状态补丁）不能把 vault 字段弄丢。
func TestVaultSurvivesMarkServer(t *testing.T) {
	s, dir := newTestStore(t)
	id := addTestServer(t, s)
	if _, err := s.SetVaultPassword(id, "7777"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.MarkServer(id, map[string]any{"lastStatus": "ok", "os": "Debian"}); err != nil {
		t.Fatalf("mark: %v", err)
	}
	s2, _ := Open(dir)
	info, err := s2.VaultInfo(id)
	if err != nil || info == nil || !info.Enabled {
		t.Fatalf("vault 应保留，got %+v err=%v", info, err)
	}
	if _, err := s2.UnlockVault(id, "7777"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
}

// 不校验强度：一位数字也能设。
func TestVaultAcceptsWeakPassword(t *testing.T) {
	s, _ := newTestStore(t)
	id := addTestServer(t, s)
	if _, err := s.SetVaultPassword(id, "1"); err != nil {
		t.Fatalf("一位数字应允许：%v", err)
	}
	if _, err := s.SetVaultPassword(id, "  "); err == nil {
		t.Fatalf("空白密码应拒绝")
	}
}
