// Package dbx：数据库浏览 / SQL 控制台 / 导入导出 / 优化。
// 与早期实现的数据库层行为一致，包括 SQL 安全检查与引擎差异处理。
package dbx

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"wpqu-panel/internal/ops"
	"wpqu-panel/internal/sites"
	"wpqu-panel/internal/sshc"
)

const maxRows = 500
const defaultStatementTimeout = 25

var systemDbs = map[string]bool{
	"information_schema": true, "performance_schema": true, "mysql": true, "sys": true,
}

// BadRequest 是 400 类错误。
type BadRequest struct{ Msg string }

func (e *BadRequest) Error() string { return e.Msg }

func bad(msg string) error { return &BadRequest{Msg: msg} }

var dbNameRe = regexp.MustCompile(`^[A-Za-z0-9_$-]{1,64}$`)
var tableNameRe = regexp.MustCompile(`^[A-Za-z0-9_$-]{1,64}$`)

// AssertDbName 校验库名。
func AssertDbName(v string) (string, error) {
	name := strings.TrimSpace(v)
	if name == "" {
		return "", bad("缺少数据库名")
	}
	if !dbNameRe.MatchString(name) {
		return "", bad("数据库名不合法（只允许字母、数字、下划线、连字符）")
	}
	if systemDbs[strings.ToLower(name)] {
		return "", bad(name + " 是系统库，面板不提供操作")
	}
	return name, nil
}

// AssertTableName 校验表名。
func AssertTableName(v string) (string, error) {
	t := strings.TrimSpace(v)
	if t == "" {
		return "", bad("缺少表名")
	}
	if !tableNameRe.MatchString(t) {
		return "", bad("表名不合法")
	}
	return t, nil
}

func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func sqlStr(v string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), "'", "''") + "'"
}

// ---------------------------------------------------------------- 执行

const clientDetect = `__cli=$(command -v mariadb || command -v mysql); ` +
	`if [ -z "$__cli" ]; then echo "服务器上没有 mariadb / mysql 客户端" >&2; exit 127; fi`

const sweepTmp = `find /tmp -maxdepth 1 \( -name 'wp-panel-dump-*' -o -name 'wp-panel-import-*' \) -mmin +60 -delete 2>/dev/null; `

var flavorCache = sync.Map{} // serverID -> "mariadb" | "mysql"

func dbFlavor(pool *sshc.Pool, server sshc.Server) string {
	if v, ok := flavorCache.Load(server.ID); ok {
		return v.(string)
	}
	res, err := pool.Exec(server, sshc.WithRoot(server,
		clientDetect+`; "$__cli" -N -B -e 'SELECT VERSION()' 2>/dev/null`),
		&sshc.ExecOptions{Timeout: 20 * time.Second})
	if err != nil {
		flavorCache.Store(server.ID, "mariadb")
		return "mariadb"
	}
	out := strings.TrimSpace(res.Stdout)
	flavor := "mariadb"
	if out != "" && !strings.Contains(strings.ToLower(out), "mariadb") {
		flavor = "mysql"
	}
	if out == "" {
		flavor = "mariadb"
	}
	flavorCache.Store(server.ID, flavor)
	return flavor
}

// RunSQLOptions 控制 SQL 执行。
type RunSQLOptions struct {
	Timeout          time.Duration
	StatementTimeout int // 秒
}

func clientCommand(db, sql string, opts RunSQLOptions, flavor string) string {
	secs := opts.StatementTimeout
	if secs <= 0 {
		secs = defaultStatementTimeout
	}
	var init string
	if flavor == "mysql" {
		init = fmt.Sprintf("SET SESSION max_execution_time=%d", secs*1000)
	} else {
		init = fmt.Sprintf("SET SESSION max_statement_time=%d", secs)
	}
	target := ""
	if db != "" {
		target = " " + sshc.ShellQuote(db)
	}
	return clientDetect + fmt.Sprintf(
		`; "$__cli" --batch --default-character-set=utf8mb4 --init-command=%s%s -e %s`,
		sshc.ShellQuote(init), target, sshc.ShellQuote(sql),
	)
}

func runSql(pool *sshc.Pool, server sshc.Server, db, sql string, opts RunSQLOptions) (*sshc.Result, error) {
	flavor := dbFlavor(pool, server)
	if opts.Timeout == 0 {
		opts.Timeout = 60 * time.Second
	}
	return pool.Exec(server, sshc.WithRoot(server, clientCommand(db, sql, opts, flavor)), &sshc.ExecOptions{Timeout: opts.Timeout})
}

// ParseTSV 解析 mysql --batch 的输出。
func ParseTSV(stdout string) (columns []string, rows [][]any) {
	text := strings.ReplaceAll(stdout, "\r\n", "\n")
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	lines := strings.Split(text, "\n")
	columns = splitLine(lines[0])
	for _, line := range lines[1:] {
		cells := splitLine(line)
		row := make([]any, len(cells))
		for i, c := range cells {
			if c == "NULL" {
				row[i] = nil
			} else {
				row[i] = c
			}
		}
		rows = append(rows, row)
	}
	return columns, rows
}

var escRe = regexp.MustCompile(`\\(.)`)

func unescapeCell(cell string) string {
	if !strings.Contains(cell, `\`) {
		return cell
	}
	return escRe.ReplaceAllStringFunc(cell, func(m string) string {
		switch m[1] {
		case 'n':
			return "\n"
		case 't':
			return "\t"
		case '0':
			return "\x00"
		case '\\':
			return `\`
		default:
			return m
		}
	})
}

func splitLine(line string) []string {
	parts := strings.Split(line, "\t")
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = unescapeCell(p)
	}
	return out
}

var errLineRe = regexp.MustCompile(`(?m)^.*ERROR\s+\d+.*$`)

func cleanError(raw string) string {
	text := strings.TrimSpace(ops.StripANSI(raw))
	if text == "" {
		return ""
	}
	if m := errLineRe.FindString(text); m != "" {
		return strings.TrimSpace(m)
	}
	lines := strings.Split(text, "\n")
	return strings.TrimSpace(lines[0])
}

// ---------------------------------------------------------------- SQL 检查

var readKeywords = map[string]bool{
	"select": true, "show": true, "describe": true, "desc": true, "explain": true,
	"with": true, "analyze": true, "values": true, "table": true, "help": true,
}

var forbidden = []struct {
	re  *regexp.Regexp
	msg string
}{
	{regexp.MustCompile(`(?i)\binto\s+(outfile|dumpfile)\b`), "不支持把结果写入服务器文件（INTO OUTFILE / DUMPFILE）"},
	{regexp.MustCompile(`(?i)\bload_file\s*\(`), "不支持读取服务器任意文件（LOAD_FILE）"},
	{regexp.MustCompile(`(?i)^\s*load\s+data\b`), "导入数据请用「导入 SQL」功能"},
	{regexp.MustCompile(`(?i)\b(install|uninstall)\s+(soname|plugin)\b`), "不支持在面板里安装数据库插件"},
	{regexp.MustCompile(`(?i)\b(create|drop|alter)\s+(user|role)\b|\bgrant\b|\brevoke\b|\bset\s+password\b`), "面板不做数据库账号与权限管理"},
}

var firstWordRe = regexp.MustCompile(`^[A-Za-z]+`)
var writeVerbRe = regexp.MustCompile(`(?i)\b(insert|update|delete|replace|truncate|alter|drop|create)\b`)

// InspectResult 是 SQL 静态检查的结果。
type InspectResult struct {
	SQL      string
	First    string
	ReadOnly bool
}

// CountStatements 数语句条数（跳过字符串与注释里的分号）。
func CountStatements(sql string) int {
	count := 0
	quote := byte(0)
	i := 0
	for i < len(sql) {
		ch := sql[i]
		var next byte
		if i+1 < len(sql) {
			next = sql[i+1]
		}
		if quote != 0 {
			if ch == '\\' && quote != '`' {
				i += 2
				continue
			}
			if ch == quote {
				if next == quote {
					i += 2
					continue
				}
				quote = 0
			}
			i++
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' {
			quote = ch
			i++
			continue
		}
		if ch == '-' && next == '-' {
			if nl := strings.IndexByte(sql[i:], '\n'); nl >= 0 {
				i += nl + 1
			} else {
				i = len(sql)
			}
			continue
		}
		if ch == '#' {
			if nl := strings.IndexByte(sql[i:], '\n'); nl >= 0 {
				i += nl + 1
			} else {
				i = len(sql)
			}
			continue
		}
		if ch == '/' && next == '*' {
			if end := strings.Index(sql[i+2:], "*/"); end >= 0 {
				i += end + 4
			} else {
				i = len(sql)
			}
			continue
		}
		if ch == ';' {
			count++
		}
		i++
	}
	return count + 1
}

// InspectSQL 检查一条 SQL。
func InspectSQL(rawSql string) (*InspectResult, error) {
	sql := strings.TrimSpace(rawSql)
	sql = strings.TrimRight(sql, "; \t\n\r")
	sql = strings.TrimRight(sql, ";")
	if sql == "" {
		return nil, bad("SQL 不能为空")
	}
	if CountStatements(sql) > 1 {
		return nil, bad("一次只能执行一条语句。多条请拆开逐条执行，或使用「导入 SQL」")
	}
	for _, rule := range forbidden {
		if rule.re.MatchString(sql) {
			return nil, bad(rule.msg)
		}
	}
	first := strings.ToLower(firstWordRe.FindString(sql))
	readOnly := readKeywords[first]
	if readOnly && writeVerbRe.MatchString(sql) {
		readOnly = false
	}
	return &InspectResult{SQL: sql, First: first, ReadOnly: readOnly}, nil
}

var limitRe = regexp.MustCompile(`(?i)\blimit\s+\d`)
var selectHeadRe = regexp.MustCompile(`(?i)^(select|with|values|table)\b`)

func applyRowLimit(sql string) string {
	if !selectHeadRe.MatchString(sql) {
		return sql
	}
	if limitRe.MatchString(sql) {
		return sql
	}
	return fmt.Sprintf("%s LIMIT %d", sql, maxRows)
}

// ---------------------------------------------------------------- 库 / 表

// ListDatabases 列业务库。
func ListDatabases(pool *sshc.Pool, server sshc.Server) ([]map[string]any, error) {
	sql := `SELECT s.SCHEMA_NAME AS name,
      s.DEFAULT_CHARACTER_SET_NAME AS charset,
      (SELECT COUNT(*) FROM information_schema.TABLES t WHERE t.TABLE_SCHEMA = s.SCHEMA_NAME) AS tbls,
      (SELECT IFNULL(ROUND(SUM(t2.DATA_LENGTH + t2.INDEX_LENGTH) / 1048576, 2), 0) FROM information_schema.TABLES t2 WHERE t2.TABLE_SCHEMA = s.SCHEMA_NAME) AS mb
    FROM information_schema.SCHEMATA s
    WHERE s.SCHEMA_NAME NOT IN ('information_schema', 'performance_schema', 'mysql', 'sys')
    ORDER BY s.SCHEMA_NAME`
	res, err := runSql(pool, server, "", sql, RunSQLOptions{Timeout: 45 * time.Second})
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		msg := cleanError(res.Stderr + res.Stdout)
		if msg == "" {
			msg = "读取数据库列表失败"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	_, rows := ParseTSV(res.Stdout)
	out := []map[string]any{}
	for _, r := range rows {
		cell := func(i int) string {
			if i < len(r) && r[i] != nil {
				return fmt.Sprintf("%v", r[i])
			}
			return ""
		}
		out = append(out, map[string]any{
			"name":     cell(0),
			"charset":  cell(1),
			"tables":   atoiSafe(cell(2)),
			"sizeMb":   atofSafe(cell(3)),
			"sizeText": ops.FormatSize(int64(atofSafe(cell(3)) * 1048576)),
		})
	}
	return out, nil
}

// ListTables 列某库的表。
func ListTables(pool *sshc.Pool, server sshc.Server, db string) ([]map[string]any, error) {
	sql := `SELECT TABLE_NAME, ENGINE, TABLE_ROWS,
      ROUND((DATA_LENGTH + INDEX_LENGTH) / 1024, 1) AS kb,
      TABLE_COLLATION,
      REPLACE(REPLACE(IFNULL(TABLE_COMMENT, ''), '\t', ' '), '\n', ' ') AS cmt
    FROM information_schema.TABLES
    WHERE TABLE_SCHEMA = ` + sqlStr(db) + `
    ORDER BY TABLE_NAME`
	res, err := runSql(pool, server, "", sql, RunSQLOptions{Timeout: 45 * time.Second})
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		msg := cleanError(res.Stderr + res.Stdout)
		if msg == "" {
			msg = "读取表列表失败"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	_, rows := ParseTSV(res.Stdout)
	out := []map[string]any{}
	for _, r := range rows {
		cell := func(i int) string {
			if i < len(r) && r[i] != nil {
				return fmt.Sprintf("%v", r[i])
			}
			return ""
		}
		out = append(out, map[string]any{
			"name": cell(0), "engine": cell(1), "rows": atoiSafe(cell(2)),
			"sizeKb": atofSafe(cell(3)), "collation": cell(4), "comment": cell(5),
		})
	}
	return out, nil
}

// FindSiteForDb 找出哪个站点在用这个库。
func FindSiteForDb(pool *sshc.Pool, server sshc.Server, db string) (map[string]any, error) {
	script := `
set +e
DB=` + sshc.ShellQuote(db) + `
for f in /var/www/*/wp-config.php; do
  [ -f "$f" ] || continue
  n=$(awk -F"'" '/DB_NAME/ {print $4; exit}' "$f" 2>/dev/null)
  [ "$n" = "$DB" ] || continue
  echo "ROOT=$(dirname "$f")"
  echo "DBUSER=$(awk -F"'" '/DB_USER/ {print $4; exit}' "$f" 2>/dev/null)"
  echo "DBPASS=$(awk -F"'" '/DB_PASSWORD/ {print $4; exit}' "$f" 2>/dev/null)"
  echo "DBHOST=$(awk -F"'" '/DB_HOST/ {print $4; exit}' "$f" 2>/dev/null)"
  echo "PREFIX=$(awk -F"'" '/table_prefix/ {print $2; exit}' "$f" 2>/dev/null)"
done
echo "__END__"
`
	res, err := sites.RunScript(pool, server, script, 30*time.Second)
	if err != nil {
		return nil, err
	}
	kv := map[string]string{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		idx := strings.Index(line, "=")
		if idx <= 0 {
			continue
		}
		switch line[:idx] {
		case "ROOT", "DBUSER", "DBPASS", "DBHOST", "PREFIX":
			kv[line[:idx]] = line[idx+1:]
		}
	}
	if kv["ROOT"] == "" {
		return nil, nil
	}
	host := kv["DBHOST"]
	if host == "" {
		host = "localhost"
	}
	return map[string]any{
		"root": kv["ROOT"], "user": kv["DBUSER"], "password": kv["DBPASS"],
		"host": host, "prefix": kv["PREFIX"],
	}, nil
}

// FetchRows 读表数据（分页）。
func FetchRows(pool *sshc.Pool, server sshc.Server, db, table string, limit, offset int) (map[string]any, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	sql := fmt.Sprintf("SELECT * FROM %s.%s LIMIT %d OFFSET %d", quoteIdent(db), quoteIdent(table), limit, offset)
	res, err := runSql(pool, server, db, sql, RunSQLOptions{Timeout: 60 * time.Second})
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		msg := cleanError(res.Stderr + res.Stdout)
		if msg == "" {
			msg = "读取表数据失败"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	columns, rows := ParseTSV(res.Stdout)
	return map[string]any{
		"db": db, "table": table, "limit": limit, "offset": offset,
		"columns": columns, "rows": rows, "truncated": len(rows) > maxRows,
	}, nil
}

// TableInfo 表结构（columns / indexes）。
func TableInfo(pool *sshc.Pool, server sshc.Server, db, table, kind string) (map[string]any, error) {
	if kind != "indexes" {
		kind = "columns"
	}
	var sql string
	if kind == "indexes" {
		sql = fmt.Sprintf("SHOW INDEX FROM %s.%s", quoteIdent(db), quoteIdent(table))
	} else {
		sql = fmt.Sprintf("SHOW FULL COLUMNS FROM %s.%s", quoteIdent(db), quoteIdent(table))
	}
	res, err := runSql(pool, server, db, sql, RunSQLOptions{Timeout: 45 * time.Second})
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		msg := cleanError(res.Stderr + res.Stdout)
		if msg == "" {
			msg = "读取表结构失败"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	columns, rows := ParseTSV(res.Stdout)
	return map[string]any{"kind": kind, "columns": columns, "rows": rows}, nil
}

// RunQuery SQL 控制台（含写操作二次确认逻辑）。
func RunQuery(pool *sshc.Pool, server sshc.Server, db, rawSql string, confirm bool) (map[string]any, error) {
	info, err := InspectSQL(rawSql)
	if err != nil {
		return nil, err
	}
	if !info.ReadOnly && !confirm {
		return map[string]any{"needConfirm": true, "kind": "write", "statement": info.SQL}, nil
	}

	started := time.Now()
	if info.ReadOnly {
		sql := applyRowLimit(info.SQL)
		res, err := runSql(pool, server, db, sql, RunSQLOptions{Timeout: 60 * time.Second})
		if err != nil {
			return nil, err
		}
		if res.Code != 0 {
			msg := cleanError(res.Stderr + res.Stdout)
			if msg == "" {
				msg = "执行失败"
			}
			return nil, fmt.Errorf("%s", msg)
		}
		columns, rows := ParseTSV(res.Stdout)
		if len(rows) > maxRows {
			rows = rows[:maxRows]
		}
		return map[string]any{
			"ok": true, "kind": "read",
			"durationMs": time.Since(started).Milliseconds(),
			"columns":    columns, "rows": rows,
			"rowCount": len(rows), "truncated": false, "statement": sql,
		}, nil
	}

	res, err := runSql(pool, server, db, info.SQL+"; SELECT ROW_COUNT() AS __affected", RunSQLOptions{Timeout: 120 * time.Second})
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		msg := cleanError(res.Stderr + res.Stdout)
		if msg == "" {
			msg = "执行失败"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	lines := strings.Fields(res.Stdout)
	affected := 0
	if len(lines) > 0 {
		affected = atoiSafe(lines[len(lines)-1])
	}
	return map[string]any{
		"ok": true, "kind": "write",
		"durationMs": time.Since(started).Milliseconds(),
		"affected":   affected,
		"message":    fmt.Sprintf("执行成功，影响 %d 行", affected),
	}, nil
}

// ---------------------------------------------------------------- 导出 / 导入

func dumpRemote(pool *sshc.Pool, server sshc.Server, db string) (string, int64, error) {
	remote := fmt.Sprintf("/tmp/wp-panel-dump-%d-%x.sql.gz", time.Now().UnixMilli(), time.Now().UnixNano()&0xffffff)
	cmd := strings.Join([]string{
		sweepTmp + `__dump=$(command -v mariadb-dump || command -v mysqldump)`,
		`[ -n "$__dump" ] || { echo "服务器上没有 mariadb-dump" >&2; exit 127; }`,
		fmt.Sprintf(`"$__dump" --single-transaction --quick --skip-lock-tables --default-character-set=utf8mb4 --routines --events --triggers %s | gzip > %s`, sshc.ShellQuote(db), remote),
		`rc=$?`,
		fmt.Sprintf(`chmod 644 %s 2>/dev/null`, remote),
		`exit $rc`,
	}, "; ")
	res, err := pool.Exec(server, sshc.WithRoot(server, cmd), &sshc.ExecOptions{Timeout: 20 * time.Minute})
	if err != nil {
		return "", 0, err
	}
	if res.Code != 0 {
		msg := cleanError(res.Stderr + res.Stdout)
		if msg == "" {
			msg = fmt.Sprintf("退出码 %d", res.Code)
		}
		return "", 0, fmt.Errorf("导出失败：%s", msg)
	}
	st, err := pool.StatPath(server, remote)
	if err != nil {
		return "", 0, err
	}
	if st.Size == 0 {
		return "", 0, fmt.Errorf("导出文件为空，检查库名是否正确")
	}
	return remote, st.Size, nil
}

func removeRemote(pool *sshc.Pool, server sshc.Server, remote string) {
	_, _ = pool.Exec(server, sshc.WithRoot(server, "rm -f "+sshc.ShellQuote(remote)), &sshc.ExecOptions{Timeout: 15 * time.Second})
}

// BackupDatabase 导出库到本机备份目录。
func BackupDatabase(pool *sshc.Pool, server sshc.Server, db string, backupDir string) (map[string]any, error) {
	remote, _, err := dumpRemote(pool, server, db)
	if err != nil {
		return nil, err
	}
	defer removeRemote(pool, server, remote)

	dir := fmt.Sprintf("%s/%s", backupDir, server.ID)
	if err := mkdirAll(dir); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("db-%s-%s.sql.gz", db, time.Now().UTC().Format("20060102150405"))
	local := dir + "/" + name
	if err := pool.DownloadToFile(server, remote, local, nil); err != nil {
		return nil, err
	}
	st, err := statLocal(local)
	if err != nil {
		return nil, err
	}
	return map[string]any{"name": name, "path": local, "size": st, "sizeText": ops.FormatSize(st)}, nil
}

// StreamDump 流式导出（直接给 HTTP 响应写）。
type DumpResult struct {
	Remote string
	Size   int64
	Finish func()
}

// PrepareDump 先在远端打包，返回句柄；调用方负责调 Finish 清理。
func PrepareDump(pool *sshc.Pool, server sshc.Server, db string) (*DumpResult, error) {
	remote, size, err := dumpRemote(pool, server, db)
	if err != nil {
		return nil, err
	}
	return &DumpResult{
		Remote: remote,
		Size:   size,
		Finish: func() { removeRemote(pool, server, remote) },
	}, nil
}

// ImportDatabase 导入 SQL（可选先备份）。
func ImportDatabase(pool *sshc.Pool, server sshc.Server, db, filename string, data []byte, backupFirst bool, backupDir string) (map[string]any, error) {
	if len(data) == 0 {
		return nil, bad("没有收到文件内容")
	}
	safe := sanitizeFilename(filename)
	remote := fmt.Sprintf("/tmp/wp-panel-import-%d-%s", time.Now().UnixMilli(), safe)
	_, _ = pool.Exec(server, sshc.WithRoot(server, sweepTmp+"true"), &sshc.ExecOptions{Timeout: 20 * time.Second})
	if err := pool.UploadBuffer(server, remote, data); err != nil {
		return nil, err
	}

	var backup map[string]any
	if backupFirst {
		b, err := BackupDatabase(pool, server, db, backupDir)
		if err != nil {
			removeRemote(pool, server, remote)
			return nil, fmt.Errorf("导入前的自动备份失败，已中止导入：%v", err)
		}
		backup = b
	}

	useGzip := strings.HasSuffix(strings.ToLower(safe), ".gz")
	var inner string
	if useGzip {
		inner = fmt.Sprintf(`gunzip -c %s | "$__cli" --default-character-set=utf8mb4 %s`, sshc.ShellQuote(remote), sshc.ShellQuote(db))
	} else {
		inner = fmt.Sprintf(`"$__cli" --default-character-set=utf8mb4 %s < %s`, sshc.ShellQuote(db), sshc.ShellQuote(remote))
	}
	cmd := fmt.Sprintf(`%s; %s 2>&1; rc=$?; rm -f %s; exit $rc`, clientDetect, inner, sshc.ShellQuote(remote))

	res, err := pool.Exec(server, sshc.WithRoot(server, cmd), &sshc.ExecOptions{Timeout: 30 * time.Minute})
	if err != nil {
		removeRemote(pool, server, remote)
		return nil, err
	}
	if res.Code != 0 {
		msg := cleanError(res.Stderr + res.Stdout)
		if msg == "" {
			msg = fmt.Sprintf("退出码 %d", res.Code)
		}
		return nil, fmt.Errorf("导入失败：%s", msg)
	}
	output := strings.TrimSpace(res.Stdout)
	if len(output) > 2000 {
		output = output[len(output)-2000:]
	}
	return map[string]any{"ok": true, "backup": backup, "output": output}, nil
}

// OptimizeTables 优化 / 修复表。
func OptimizeTables(pool *sshc.Pool, server sshc.Server, db, table, mode string) (map[string]any, error) {
	verb := "OPTIMIZE"
	if mode == "repair" {
		verb = "REPAIR"
	}
	var list []string
	if table != "" {
		list = []string{table}
	} else {
		tables, err := ListTables(pool, server, db)
		if err != nil {
			return nil, err
		}
		for _, t := range tables {
			list = append(list, fmt.Sprintf("%v", t["name"]))
		}
	}
	if len(list) == 0 {
		return nil, bad("这个库里没有表")
	}
	if len(list) > 200 {
		return nil, bad("表太多，请到具体表的详情里单独操作")
	}

	// MySQL 8 的 NO_ZERO_DATE 与 WordPress 的 '0000-00-00' 冲突，会话级放宽
	flavor := dbFlavor(pool, server)
	preamble := ""
	if flavor == "mysql" {
		preamble = "SET SESSION sql_mode='NO_ENGINE_SUBSTITUTION'; "
	}
	quoted := make([]string, len(list))
	for i, t := range list {
		quoted[i] = quoteIdent(t)
	}
	sql := preamble + verb + " TABLE " + strings.Join(quoted, ", ")
	res, err := runSql(pool, server, db, sql, RunSQLOptions{Timeout: 15 * time.Minute, StatementTimeout: 600})
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		msg := cleanError(res.Stderr + res.Stdout)
		if msg == "" {
			msg = verb + " 失败"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	columns, rows := ParseTSV(res.Stdout)
	return map[string]any{
		"mode": strings.ToLower(verb), "columns": columns, "rows": rows, "count": len(list),
	}, nil
}

// ---------------------------------------------------------------- 工具

func atoiSafe(s string) int {
	n := 0
	neg := false
	s = strings.TrimSpace(s)
	for i, c := range s {
		if i == 0 && c == '-' {
			neg = true
			continue
		}
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		return -n
	}
	return n
}

func atofSafe(s string) float64 {
	var v float64
	fmt.Sscanf(strings.TrimSpace(s), "%g", &v)
	return v
}

var filenameBadRe = regexp.MustCompile(`[^\w.\-]`)

func sanitizeFilename(name string) string {
	if name == "" {
		name = "upload.sql"
	}
	s := filenameBadRe.ReplaceAllString(name, "_")
	if len(s) > 60 {
		s = s[len(s)-60:]
	}
	return s
}

// mkdirAll / statLocal：小工具（放在这里避免额外 import 块改动）。
func mkdirAll(dir string) error { return os.MkdirAll(dir, 0o700) }
func statLocal(p string) (int64, error) {
	st, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}
