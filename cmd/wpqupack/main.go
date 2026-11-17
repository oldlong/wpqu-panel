// wpqupack：把插件源码打成签名包，并生成带签名的市场索引与升级清单。
//
//	go run ./cmd/wpqupack plugins -src ../plugins-src -site ../site -key ../site/.keys/release.key
//	go run ./cmd/wpqupack updates -dist ../dist -site ../site -version 1.9.0 -key ../site/.keys/release.key
//
// 实际逻辑在 internal/pack，管理后台 wpquadmin 用的是同一份代码。
// 生成物（全部带 .sig，面板只认签名过的）：
//
//	site/plugins/<id>/<version>.zip
//	site/api/plugins/index.json
//	site/api/updates.json
//	site/dl/<version>/<binary>
package main

import (
	"crypto/ed25519"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"wpqu-panel/internal/pack"
	"wpqu-panel/internal/signing"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "plugins":
		cmdPlugins(os.Args[2:])
	case "updates":
		cmdUpdates(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `wpqupack — WPQU panel 打包工具

  plugins   打包 plugins-src/* 并生成市场索引
  updates   收集 dist/* 二进制并生成升级清单
`)
	os.Exit(2)
}

func cmdPlugins(args []string) {
	fs := flag.NewFlagSet("plugins", flag.ExitOnError)
	src := fs.String("src", "../plugins-src", "插件源码目录")
	site := fs.String("site", "../site", "站点目录")
	keyFile := fs.String("key", "", "发布私钥")
	dryRun := fs.Bool("dry-run", false, "只计算不落盘")
	_ = fs.Parse(args)
	if *keyFile == "" {
		die("需要 -key 指定发布私钥")
	}
	priv := loadKey(*keyFile)

	res, err := pack.PackPlugins(*src, *site, priv, *dryRun)
	if err != nil {
		die("%v", err)
	}
	for _, p := range res.Plugins {
		mark := map[string]string{"new": "+", "changed": "~", "same": "="}[p.Status]
		fmt.Printf("  %s %-10s v%-8s %6.1f KB  %d 个文件\n",
			mark, p.ID, p.Version, float64(p.Size)/1024, p.Files)
	}
	for _, s := range res.Skipped {
		fmt.Printf("  跳过 %s\n", s)
	}
	if *dryRun {
		fmt.Printf("\n（演练）索引 %s 将%s，%d 个插件\n",
			res.IndexPath, map[string]string{"same": "不变", "changed": "变化"}[res.IndexStatus], len(res.Plugins))
		return
	}
	fmt.Printf("\n索引：%s（%d 个插件）\n", res.IndexPath, len(res.Plugins))
}

func cmdUpdates(args []string) {
	fs := flag.NewFlagSet("updates", flag.ExitOnError)
	dist := fs.String("dist", "../dist", "构建产物目录")
	site := fs.String("site", "../site", "站点目录")
	ver := fs.String("version", "", "本次版本号，例如 1.9.0")
	keyFile := fs.String("key", "", "发布私钥")
	notesFile := fs.String("notes", "", "发布说明 JSON（可选）")
	minVer := fs.String("min", "", "低于此版本不再提示升级（可选）")
	critical := fs.Bool("critical", false, "重大更新，面板会标红")
	_ = fs.Parse(args)
	if *ver == "" || *keyFile == "" {
		die("需要 -version 与 -key")
	}
	priv := loadKey(*keyFile)

	var notes map[string][]string
	if *notesFile != "" {
		raw, err := os.ReadFile(*notesFile)
		if err != nil {
			die("读发布说明失败：%v", err)
		}
		if err := json.Unmarshal(raw, &notes); err != nil {
			die("发布说明格式应为 {\"zh-CN\":[\"...\"],\"en\":[\"...\"]}：%v", err)
		}
	}

	res, err := pack.PackUpdates(pack.UpdatesOptions{
		DistDir:      *dist,
		SiteDir:      *site,
		Version:      *ver,
		MinSupported: *minVer,
		Critical:     *critical,
		Notes:        notes,
	}, priv)
	if err != nil {
		die("%v", err)
	}
	for _, f := range res.Files {
		fmt.Printf("  ✓ %s\n", f)
	}
	fmt.Printf("\n升级清单：%s（%d 个平台，共 %d 个版本）\n", res.Path, res.Platforms, res.Versions)
}

func loadKey(path string) ed25519.PrivateKey {
	priv, err := signing.LoadPrivateKey(path)
	if err != nil {
		die("%v", err)
	}
	return priv
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
