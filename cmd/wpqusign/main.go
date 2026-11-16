// wpqusign：发布与授权工具（不属于面板运行时，单独构建）。
//
//	go run ./cmd/wpqusign keygen  -dir ../site/.keys
//	go run ./cmd/wpqusign sign    -key ../site/.keys/release.key -in ../site/api/updates.json
//	go run ./cmd/wpqusign license -key ../site/.keys/license.key -product openclaw \
//	                              -expire 2027-12-31 -seats 1 -name "ACME"
//	go run ./cmd/wpqusign verify  -pub <公钥> -in a.json -sig a.json.sig
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "keygen":
		cmdKeygen(os.Args[2:])
	case "sign":
		cmdSign(os.Args[2:])
	case "license":
		cmdLicense(os.Args[2:])
	case "verify":
		cmdVerify(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令：%s\n\n", os.Args[1])
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `wpqusign — WPQU panel 发布与授权工具

  keygen   生成 release / license 两对 Ed25519 密钥
  sign     为文件生成分离签名 <file>.sig
  license  签发离线授权码
  verify   校验签名

`)
	os.Exit(2)
}

// ---------------------------------------------------------------- keygen

func cmdKeygen(args []string) {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	dir := fs.String("dir", "keys", "密钥输出目录")
	force := fs.Bool("force", false, "已存在时覆盖（会作废已发出的所有签名与授权码）")
	_ = fs.Parse(args)

	if err := os.MkdirAll(*dir, 0o700); err != nil {
		die("创建目录失败：%v", err)
	}
	pairs := []struct{ name, purpose string }{
		{"release", "校验升级清单 / 插件索引 / 插件包"},
		{"license", "校验用户授权码"},
	}
	for _, p := range pairs {
		keyFile := filepath.Join(*dir, p.name+".key")
		pubFile := filepath.Join(*dir, p.name+".pub")
		if _, err := os.Stat(keyFile); err == nil && !*force {
			fmt.Printf("已存在，跳过：%s\n", keyFile)
			if pub, err := os.ReadFile(pubFile); err == nil {
				fmt.Printf("  %s 公钥：%s\n", p.name, strings.TrimSpace(string(pub)))
			}
			continue
		}
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			die("生成密钥失败：%v", err)
		}
		if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(priv)), 0o600); err != nil {
			die("写私钥失败：%v", err)
		}
		if err := os.WriteFile(pubFile, []byte(base64.StdEncoding.EncodeToString(pub)), 0o644); err != nil {
			die("写公钥失败：%v", err)
		}
		fmt.Printf("已生成 %s（%s）\n  私钥：%s\n  公钥：%s\n",
			p.name, p.purpose, keyFile, base64.StdEncoding.EncodeToString(pub))
	}
	fmt.Print(`
下一步：把上面两个「公钥」分别填进 panel-go/internal/signing/signing.go 的同名常量
（ReleasePublicKey / LicensePublicKey），然后重新构建面板。

⚠ 私钥等同于签名权与发码权，务必离线备份。丢失 = 老版本面板永久无法验证新包；
   泄露 = 任何人都能伪造升级包。切勿把 *.key 提交进 git。`)
}

// ---------------------------------------------------------------- sign

func cmdSign(args []string) {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyFile := fs.String("key", "", "私钥文件（release.key）")
	in := fs.String("in", "", "待签名文件")
	out := fs.String("out", "", "签名输出（默认 <in>.sig）")
	_ = fs.Parse(args)
	if *keyFile == "" || *in == "" {
		die("需要 -key 与 -in")
	}
	priv := loadKey(*keyFile)
	data, err := os.ReadFile(*in)
	if err != nil {
		die("读文件失败：%v", err)
	}
	sig := ed25519.Sign(priv, data)
	target := *out
	if target == "" {
		target = *in + ".sig"
	}
	if err := os.WriteFile(target, []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644); err != nil {
		die("写签名失败：%v", err)
	}
	fmt.Printf("已签名 %s → %s（%d 字节）\n", *in, target, len(data))
}

// ---------------------------------------------------------------- license

type licensePayload struct {
	Product string `json:"p"`           // 插件 id
	Expire  string `json:"e"`           // YYYY-MM-DD，空 = 永久
	Seats   int    `json:"s"`           // 可安装机器数，0 = 不限
	Name    string `json:"n,omitempty"` // 客户标识
	ID      string `json:"i"`           // 授权码唯一 id（对账用）
	Issued  string `json:"t"`           // 签发时间
}

func cmdLicense(args []string) {
	fs := flag.NewFlagSet("license", flag.ExitOnError)
	keyFile := fs.String("key", "", "授权私钥（license.key）")
	product := fs.String("product", "", "插件 id，例如 openclaw")
	expire := fs.String("expire", "", "到期日 YYYY-MM-DD，留空为永久")
	seats := fs.Int("seats", 1, "可安装机器数，0 为不限")
	name := fs.String("name", "", "客户标识（写进码里，便于对账）")
	count := fs.Int("count", 1, "批量生成数量")
	_ = fs.Parse(args)
	if *keyFile == "" || *product == "" {
		die("需要 -key 与 -product")
	}
	if *expire != "" {
		if _, err := time.Parse("2006-01-02", *expire); err != nil {
			die("到期日格式应为 YYYY-MM-DD：%v", err)
		}
	}
	priv := loadKey(*keyFile)
	for i := 0; i < *count; i++ {
		p := licensePayload{
			Product: *product,
			Expire:  *expire,
			Seats:   *seats,
			Name:    *name,
			ID:      randomID(6),
			Issued:  time.Now().Format("2006-01-02T15:04:05Z07:00"),
		}
		body, _ := json.Marshal(p)
		sig := ed25519.Sign(priv, body)
		fmt.Printf("%s.%s.%s\n",
			"WPQU1",
			base64.RawURLEncoding.EncodeToString(body),
			base64.RawURLEncoding.EncodeToString(sig))
	}
}

func randomID(n int) string {
	const alphabet = "0123456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(buf)
}

// ---------------------------------------------------------------- verify

func cmdVerify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	pubB64 := fs.String("pub", "", "公钥（base64）")
	in := fs.String("in", "", "被签名文件")
	sigFile := fs.String("sig", "", "签名文件（默认 <in>.sig）")
	_ = fs.Parse(args)
	if *pubB64 == "" || *in == "" {
		die("需要 -pub 与 -in")
	}
	rawPub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(*pubB64))
	if err != nil {
		die("公钥解析失败：%v", err)
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		die("读文件失败：%v", err)
	}
	sf := *sigFile
	if sf == "" {
		sf = *in + ".sig"
	}
	sigText, err := os.ReadFile(sf)
	if err != nil {
		die("读签名失败：%v", err)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigText)))
	if err != nil {
		die("签名解析失败：%v", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(rawPub), data, sig) {
		fmt.Println("✗ 签名不匹配")
		os.Exit(1)
	}
	fmt.Println("✓ 签名有效")
}

// ---------------------------------------------------------------- 工具

func loadKey(path string) ed25519.PrivateKey {
	raw, err := os.ReadFile(path)
	if err != nil {
		die("读私钥失败：%v", err)
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		die("私钥不是合法 base64：%v", err)
	}
	switch len(b) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(b)
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b)
	}
	die("私钥长度异常：%d", len(b))
	return nil
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
