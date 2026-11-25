package docs

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"remote-cc/internal/cli"
)

// root is the repository, relative to this package.
const root = "../.."

// exceptions may be in English and stay out of README's document table
// (docs/documentation.md, 「例外」).
var exceptions = map[string]bool{
	"LICENSE": true, "NOTICE": true, "THIRD_PARTY_NOTICES.md": true, "CODE_OF_CONDUCT.md": true,
	"SECURITY.md": true, "UPSTREAM.md": true, "deploy/egress/mihomo.LICENSE": true,
}

func exception(f string) bool { return exceptions[f] || strings.HasPrefix(f, ".github/") }

// Files exempt from some greps: the rules themselves quote the forbidden
// words; history and records may name old versions and measurements.
const (
	rulesFile = "docs/documentation.md"
	changelog = "CHANGELOG.md"
	records   = "docs/records/"
)

// userVars are the RCC_* variables users and deployers set; each must be
// in the code and documented.
var userVars = []string{
	"RCC_VERSION", "RCC_INSTALL_DIR", "RCC_NAME", "RCC_REPO", // install.sh, install.ps1
	"RCC_ACCOUNT", "RCC_SHELL", "RCC_RCB_TOKEN", "RCC_EGRESS_TOKEN", // the CLI
	"RCC_EGRESS_MAX_NODES", "RCC_EGRESS_MAX_NODES_PER_USER", // the egress host's .env
}

// doc is one Markdown file, split into prose and code.
type doc struct {
	path  string   // relative to root, with slashes
	lines []string // raw
	code  []bool   // line is inside a fenced block
}

var (
	fenceRe  = regexp.MustCompile("^\\s*(```|~~~)")
	spanRe   = regexp.MustCompile("`[^`]*`")
	linkRe   = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	autoRe   = regexp.MustCompile(`<https?://[^>]*>`)
	idRe     = regexp.MustCompile(`<a id="([^"]+)"></a>`)
	headRe   = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	textRe   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	cmdRe    = regexp.MustCompile(`(^\s*|&&\s*|;\s*|\|\s*|\$\s*)remote-cc((?:[ \t]+[a-z][a-z0-9-]*)*)`)
	envRe    = regexp.MustCompile(`\bRCC_[A-Z0-9_]*[A-Z0-9]\b`)
	englishR = regexp.MustCompile(`(?:[A-Za-z][A-Za-z'’-]*[\s,;:]+){7}[A-Za-z][A-Za-z'’-]*`)
)

// prose is line i without code spans and link targets ("" inside a fence).
func (d *doc) prose(i int) string {
	if d.code[i] {
		return ""
	}
	s := spanRe.ReplaceAllString(d.lines[i], " ")
	s = linkRe.ReplaceAllString(s, "]")
	s = autoRe.ReplaceAllString(s, " ")
	return idRe.ReplaceAllString(s, " ")
}

// codeText is what line i says in code: the whole line inside a fence, else
// its code spans.
func (d *doc) codeText(i int) []string {
	if d.code[i] {
		return []string{d.lines[i]}
	}
	var out []string
	for _, m := range spanRe.FindAllString(d.lines[i], -1) {
		out = append(out, strings.Trim(m, "`"))
	}
	return out
}

func load(t *testing.T, rel string) *doc {
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	d := &doc{path: rel, lines: strings.Split(strings.TrimRight(string(b), "\n"), "\n")}
	in := false
	for _, l := range d.lines {
		f := fenceRe.MatchString(l)
		d.code = append(d.code, in || f)
		if f {
			in = !in
		}
	}
	return d
}

// mdFiles is every Markdown file in the repository (tracked or new, not
// ignored), relative to root.
func mdFiles(t *testing.T) []string {
	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "--", "*.md")
	cmd.Dir = root
	var files []string
	if out, err := cmd.Output(); err == nil {
		for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if f == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(root, f)); err == nil { // deleted but still tracked
				files = append(files, f)
			}
		}
		return files
	}
	// No git: walk the tree.
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() && (e.Name() == ".git" || e.Name() == "dist" || e.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if !e.IsDir() && strings.HasSuffix(p, ".md") {
			r, _ := filepath.Rel(root, p)
			files = append(files, filepath.ToSlash(r))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// docs is every Markdown file the rules apply to.
func docs(t *testing.T) []*doc {
	var ds []*doc
	for _, f := range mdFiles(t) {
		if !exception(f) {
			ds = append(ds, load(t, f))
		}
	}
	if len(ds) == 0 {
		t.Fatal("no Markdown files found")
	}
	return ds
}

// slug is GitHub's anchor for a heading.
func slug(h string) string {
	h = textRe.ReplaceAllString(h, "$1")
	h = strings.ReplaceAll(h, "`", "")
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}

// anchors is every anchor of a file: heading slugs and <a id>.
func anchors(d *doc) map[string]bool {
	a := map[string]bool{}
	seen := map[string]int{}
	for i, l := range d.lines {
		if d.code[i] {
			continue
		}
		for _, m := range idRe.FindAllStringSubmatch(l, -1) {
			a[m[1]] = true
		}
		if m := headRe.FindStringSubmatch(l); m != nil {
			s := slug(m[2])
			if n := seen[s]; n > 0 {
				a[s+"-"+strconv.Itoa(n)] = true
			} else {
				a[s] = true
			}
			seen[s]++
		}
	}
	return a
}

func TestReaderLine(t *testing.T) {
	for _, d := range docs(t) {
		var lines []string
		for _, l := range d.lines {
			if strings.TrimSpace(l) != "" {
				lines = append(lines, l)
			}
			if len(lines) == 2 {
				break
			}
		}
		if len(lines) < 2 || !strings.HasPrefix(lines[0], "# ") || !strings.HasPrefix(lines[1], "> 读者：") {
			t.Errorf("%s: must start with a title (# …) followed by the reader line (> 读者：…)", d.path)
		}
	}
}

func TestLength(t *testing.T) {
	for _, d := range docs(t) {
		max := 400
		if d.path == "README.md" {
			max = 150
		}
		if len(d.lines) > max {
			t.Errorf("%s: %d lines, at most %d", d.path, len(d.lines), max)
		}
	}
}

func TestDocumentTable(t *testing.T) {
	readme := load(t, "README.md")
	listed := map[string]bool{}
	in := false
	for i, l := range readme.lines {
		if m := headRe.FindStringSubmatch(l); m != nil && !readme.code[i] {
			in = m[1] == "##" && strings.TrimSpace(m[2]) == "文档"
			continue
		}
		if !in || !strings.HasPrefix(strings.TrimSpace(l), "|") {
			continue
		}
		for _, m := range linkRe.FindAllStringSubmatch(l, -1) {
			target := strings.SplitN(m[1], "#", 2)[0]
			listed[path.Clean(target)] = true
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(target))); err != nil {
				t.Errorf("README.md 「文档」: %s does not exist", target)
			}
		}
	}
	if len(listed) == 0 {
		t.Fatal("README.md has no 「文档」 table")
	}
	for _, d := range docs(t) {
		if d.path != "README.md" && !listed[d.path] {
			t.Errorf("%s is not in README.md's 「文档」 table", d.path)
		}
	}
}

func TestLinks(t *testing.T) {
	cache := map[string]map[string]bool{}
	for _, d := range docs(t) {
		cache[d.path] = anchors(d)
	}
	for _, d := range docs(t) {
		for i, l := range d.lines {
			if d.code[i] {
				continue
			}
			for _, m := range linkRe.FindAllStringSubmatch(spanRe.ReplaceAllString(l, ""), -1) {
				target := m[1]
				if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
					continue
				}
				file, anchor, _ := strings.Cut(target, "#")
				rel := d.path
				if file != "" {
					rel = path.Clean(path.Join(path.Dir(d.path), file))
					if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
						t.Errorf("%s:%d: link to %s: no such file", d.path, i+1, target)
						continue
					}
				}
				if anchor == "" || !strings.HasSuffix(rel, ".md") {
					continue
				}
				a, ok := cache[rel]
				if !ok {
					a = anchors(load(t, rel))
					cache[rel] = a
				}
				if !a[anchor] {
					t.Errorf("%s:%d: link to %s: no anchor #%s in %s", d.path, i+1, target, anchor, rel)
				}
			}
		}
	}
}

// TestOwnAnchors: the anchors RCB links to (「对外锚点」) exist.
func TestOwnAnchors(t *testing.T) {
	rules := load(t, rulesFile)
	in, n := false, 0
	row := regexp.MustCompile("^\\|\\s*`([^`]+)`\\s*\\|\\s*`([^`]+)`\\s*\\|")
	for i, l := range rules.lines {
		if m := headRe.FindStringSubmatch(l); m != nil && !rules.code[i] {
			in = strings.TrimSpace(m[2]) == "对外锚点"
			continue
		}
		if m := row.FindStringSubmatch(l); in && m != nil {
			n++
			if !anchors(load(t, m[1]))[m[2]] {
				t.Errorf("%s: anchor #%s (listed in %s 「对外锚点」) is missing", m[1], m[2], rulesFile)
			}
		}
	}
	if n == 0 {
		t.Errorf("%s: no anchors found under 「对外锚点」", rulesFile)
	}
}

// forbidden are the 「不要写」 words of the term table that a grep can tell
// apart from correct uses (docs/documentation.md, 「术语」), checked in prose.
var forbidden = []struct{ re, say string }{
	{`Remote Browser|remote-browser`, "Remote Claude Browser（RCB）"},
	{`(^|[^\w./-])rcc([^\w/-]|$)`, "remote-cc (in prose; `rcc-*` names go in backticks)"},
	{`浏览器账号|(?i)browser account`, "浏览器配置"},
	{`(?i)\bprofile\b`, "浏览器配置 or 历史档案"},
	{`(?i)\bworkspace\b|前端`, "工作台"},
	{`(?i)service users`, "用户"},
	{`用户管理|(?i)admin panel`, "「管理后台」"},
	{`工作台 → 代理|工作台 → API`, "「代理与 API」面板"},
	{`代理池`, "代理来源"},
	{`代理节点|出口节点`, "节点"},
	{`部署默认出口|(?i)deployment default exit`, "出口"},
	{`图像模式|(?i)picture mode`, "画面模式"},
	{`远程窗口`, "会话"},
	{`专用实例|共享账号池`, "CPA 实例"},
	{`外部 API Key|个人 Key|(?i)inference key`, "调用 Key"},
	{`浏览器宿主机`, "入口机、服务端"},
	{`\bAgent\b`, "网关"},
	{`指纹身份`, "系统身份"},
	{`指纹浏览器`, "内核"},
	{`运维者|(?i)\boperator\b`, "部署者"},
	{`(^|[^\w])[AB]([^\w]|$)|本机`, "电脑、服务器"},
	{`会话许可`, "会话授权"},
}

func TestTerms(t *testing.T) {
	for _, d := range docs(t) {
		if d.path == rulesFile || d.path == changelog || strings.HasPrefix(d.path, records) {
			continue
		}
		for i := range d.lines {
			p := d.prose(i)
			for _, f := range forbidden {
				if regexp.MustCompile(f.re).MatchString(p) {
					t.Errorf("%s:%d: forbidden term /%s/ (write: %s): %s", d.path, i+1, f.re, f.say, d.lines[i])
				}
			}
			if m := englishR.FindString(p); m != "" {
				t.Errorf("%s:%d: English prose (8+ words outside backticks): %q", d.path, i+1, m)
			}
		}
	}
}

// greps are principle 1, 3, 4 and 6's checks: a hit is allowed only in the
// exempt files, or on a line that links where it says.
func TestGreps(t *testing.T) {
	const rcb = "github.com/NotDesigned/remote-claude-browser"
	checks := []struct {
		name, re string
		exempt   func(string) bool
		linkTo   string // a hit on a line containing this is fine
	}{
		{"principle 3 (history only in CHANGELOG)", `v0\.[0-9]|旧版|兼容：|已移除|草案|之前的版本`,
			func(f string) bool { return f == changelog || strings.HasPrefix(f, records) }, "CHANGELOG.md"},
		{"principle 4 (no implementation details)", `\.ts:[0-9]|\.go:[0-9]|已实测|里程碑|待完成|TODO`,
			func(f string) bool { return strings.HasPrefix(f, records) }, ""},
		{"principle 6 (RCB's side)", `/api/remote-cc/v1|rcck_|rcch_`,
			func(string) bool { return false }, rcb},
	}
	// Principle 1: volatile facts are written in their owning file only.
	owners := map[string]string{
		`/var/lib/remote-cc|/opt/remote-cc|servers\.json|RCC_EGRESS_MAX_NODES`: "deploy/README.md",
		`dcache_timeout|RefreshEvery|deniedLimit|\.rcc-oauth-token`:            "docs/architecture.md",
		`rcb-dirs\.json|rcb\.json|15 分钟`:                                       "docs/usage.md",
	}
	for re, owner := range owners {
		owner := owner
		checks = append(checks, struct {
			name, re string
			exempt   func(string) bool
			linkTo   string
		}{"principle 1 (owned by " + owner + ")", re, func(f string) bool {
			return f == owner || f == changelog || strings.HasPrefix(f, records)
		}, path.Base(owner)})
	}
	for _, d := range docs(t) {
		if d.path == rulesFile {
			continue
		}
		for _, c := range checks {
			if c.exempt(d.path) {
				continue
			}
			re := regexp.MustCompile(c.re)
			for i, l := range d.lines {
				if re.MatchString(l) && (c.linkTo == "" || !strings.Contains(l, c.linkTo)) {
					t.Errorf("%s:%d: %s: %s", d.path, i+1, c.name, l)
				}
			}
		}
	}
}

// TestCommands: every command line `remote-cc …` in code (at the start of a
// code span or line, or after &&, ;, | or $) names a command of the command
// table (or a group of them); a plain word after remote-cc that is no
// command would be taken for a HOST, so docs write hosts as <服务器>-cc.
func TestCommands(t *testing.T) {
	names := map[string]bool{}
	nouns := map[string]bool{}
	for _, n := range cli.CommandNames() {
		names[n] = true
		w := strings.Fields(n)
		for k := 1; k < len(w); k++ {
			nouns[strings.Join(w[:k], " ")] = true
		}
	}
	// longest is how many of words name a command (or a group).
	longest := func(words []string) (n int, cmd bool) {
		for k := min(3, len(words)); k > 0; k-- {
			if s := strings.Join(words[:k], " "); names[s] || nouns[s] {
				return k, names[s]
			}
		}
		return 0, false
	}
	for _, d := range docs(t) {
		for i := range d.lines {
			for _, text := range d.codeText(i) {
				for _, m := range cmdRe.FindAllStringSubmatchIndex(text, -1) {
					if end := m[1]; end < len(text) && strings.ContainsRune("-_", rune(text[end])) {
						continue // remote-cc-server, …
					}
					words := strings.Fields(text[m[4]:m[5]])
					if len(words) == 0 {
						continue
					}
					n, cmd := longest(words)
					bad := n == 0
					if !bad && !cmd && n < len(words) {
						bad = true // a group followed by a word that is none of its commands
					}
					if !bad && words[0] == "help" && n < len(words) {
						rest := words[n:]
						k, _ := longest(rest)
						bad = k != len(rest)
					}
					if bad {
						t.Errorf("%s:%d: %q: not a command of remote-cc help", d.path, i+1, "remote-cc "+strings.Join(words, " "))
					}
				}
			}
		}
	}
}

// TestEnvVars: every RCC_* variable in the docs exists in the code, and the
// ones users and deployers set are documented.
func TestEnvVars(t *testing.T) {
	code := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			switch e.Name() {
			case ".git", "dist", "docs", "shims":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".md") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		if info, err := e.Info(); err != nil || info.Size() > 1<<20 {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil || bytes.IndexByte(b, 0) >= 0 {
			return err
		}
		for _, v := range envRe.FindAllString(string(b), -1) {
			code[v] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for _, d := range docs(t) {
		for i, l := range d.lines {
			for _, v := range envRe.FindAllString(l, -1) {
				documented[v] = true
				if !code[v] {
					t.Errorf("%s:%d: %s is not used anywhere in the code", d.path, i+1, v)
				}
			}
		}
	}
	var missing []string
	for _, v := range userVars {
		if !code[v] {
			t.Errorf("%s is listed as a user-facing variable but the code does not use it", v)
		}
		if !documented[v] {
			missing = append(missing, v)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("user-facing variables not documented: %s", strings.Join(missing, ", "))
	}
}
