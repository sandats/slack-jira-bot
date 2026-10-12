// Package inquiry は Slack に投稿された問い合わせ本文を項目ごとに解析する（Slack SDK 非依存）。
package inquiry

import (
	"cmp"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
)

// 項目定義
var (
	RequestTypes = []string{"アカウント作成", "CI/CD", "使い方", "その他"}
	Priorities   = []string{"緊急", "高", "中", "通常"}

	// 元メッセージ中のラベル表記（表記ゆれはここに追加）
	fieldAliases = []struct {
		Key     string
		Aliases []string
	}{
		{"title", []string{"お問い合わせの内容（タイトル）", "お問い合わせの内容(タイトル)", "お問い合わせの内容"}},
		{"type", []string{"依頼種別"}},
		{"priority", []string{"優先度"}},
		{"due", []string{"希望対応期限"}},
		{"detail", []string{"お問い合わせの詳細"}},
	}
)

// Message は Slack 非依存の「元メッセージ」表現。slack-go の型から slackapp で変換する。
type Message struct {
	Text        string
	User        string
	BotID       string
	TS          string
	ThreadTS    string
	Blocks      []any // blocks を JSON にして汎用型へ展開したもの
	Attachments []Attachment
}

type Attachment struct {
	Pretext string
	Text    string
	Blocks  []any
}

var (
	mentionRe = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|[^>]*)?>`)
	dateRe    = regexp.MustCompile(`(\d{4})\D+(\d{1,2})\D+(\d{1,2})`)
	cmdRe     = regexp.MustCompile(`(?i)(close|cancel|cansel|クローズ|キャンセル)\s+([A-Za-z][A-Za-z0-9_]*-\d+)`)

	aliasToKey map[string]string
	labelRe    *regexp.Regexp

	commandAliases = map[string]string{
		"close":  "close",
		"クローズ":   "close",
		"cancel": "cancel",
		"cansel": "cancel", // 打ち間違いも受け付ける
		"キャンセル":  "cancel",
	}
)

func init() {
	aliasToKey = map[string]string{}
	var aliases []string
	for _, f := range fieldAliases {
		for _, a := range f.Aliases {
			aliasToKey[a] = f.Key
			aliases = append(aliases, a)
		}
	}
	// 長いラベルを優先してマッチさせる
	slices.SortFunc(aliases, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	quoted := make([]string, len(aliases))
	for i, a := range aliases {
		quoted[i] = regexp.QuoteMeta(a)
	}
	labelRe = regexp.MustCompile(
		`[\*_]*(` + strings.Join(quoted, "|") + `)[\*_\s]*[:：]?[\*_\s]*`,
	)
}

// Parse は「ラベル: 値」形式の本文を項目ごとに切り出す。見つからない項目は含まれない。
func Parse(text string) map[string]string {
	result := map[string]string{}
	locs := labelRe.FindAllStringSubmatchIndex(text, -1)
	for i, loc := range locs {
		label := text[loc[2]:loc[3]]
		key := aliasToKey[label]
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		value := strings.TrimSpace(text[loc[1]:end])
		if _, exists := result[key]; !exists {
			result[key] = value
		}
	}
	return result
}

// collectText は blocks（汎用 JSON）から表示テキストを順に集める。
func collectText(node any, out *[]string) {
	switch n := node.(type) {
	case map[string]any:
		t, _ := n["type"].(string)
		switch t {
		case "text", "mrkdwn", "plain_text":
			if s, ok := n["text"].(string); ok {
				*out = append(*out, s)
			}
		case "link":
			if s, ok := n["text"].(string); ok && s != "" {
				*out = append(*out, s)
			} else if u, ok := n["url"].(string); ok {
				*out = append(*out, u)
			}
		case "emoji":
			if name, ok := n["name"].(string); ok {
				*out = append(*out, ":"+name+":")
			}
		case "user":
			// rich_text 内のメンションは通常のメンション書式に戻す
			if uid, ok := n["user_id"].(string); ok && uid != "" {
				*out = append(*out, "<@"+uid+">")
			}
		}
		// map の走査順を安定させる（text を先に、残りはキー順）
		keys := slices.SortedFunc(maps.Keys(n), func(a, b string) int {
			if (a == "text") != (b == "text") {
				if a == "text" {
					return -1
				}
				return 1
			}
			return strings.Compare(a, b)
		})
		for _, k := range keys {
			collectText(n[k], out)
		}
	case []any:
		for _, v := range n {
			collectText(v, out)
		}
	}
}

func blocksText(blocks []any) string {
	lines := make([]string, 0, len(blocks))
	for _, b := range blocks {
		var parts []string
		collectText(b, &parts)
		sep := "\n"
		if m, ok := b.(map[string]any); ok && m["type"] == "rich_text" {
			sep = ""
		}
		lines = append(lines, strings.Join(parts, sep))
	}
	return strings.Join(lines, "\n")
}

// Extract は text / blocks / attachments のうち、最も多く項目を取れたものを採用する。
func Extract(m Message) (string, map[string]string) {
	candidates := []string{m.Text, blocksText(m.Blocks)}
	for _, a := range m.Attachments {
		var parts []string
		for _, s := range []string{a.Pretext, a.Text, blocksText(a.Blocks)} {
			if s != "" {
				parts = append(parts, s)
			}
		}
		candidates = append(candidates, strings.Join(parts, "\n"))
	}

	bestText := ""
	best := map[string]string{}
	for _, c := range candidates {
		parsed := Parse(c)
		if len(parsed) > len(best) || (len(best) == 0 && len(c) > len(bestText)) {
			bestText, best = c, parsed
		}
	}
	return bestText, best
}

func findUserID(node any) string {
	switch n := node.(type) {
	case map[string]any:
		if n["type"] == "user" {
			if uid, ok := n["user_id"].(string); ok && uid != "" {
				return uid
			}
		}
		for _, k := range slices.Sorted(maps.Keys(n)) {
			if uid := findUserID(n[k]); uid != "" {
				return uid
			}
		}
	case []any:
		for _, v := range n {
			if uid := findUserID(v); uid != "" {
				return uid
			}
		}
	}
	return ""
}

// FindRequesterID は問い合わせ者の Slack ユーザー ID を返す。見つからなければ空文字。
func FindRequesterID(m Message, fullText string) string {
	// 1. 本文内のメンション（最初に出てくるものを問い合わせ者とみなす）
	if sub := mentionRe.FindStringSubmatch(fullText); sub != nil {
		return sub[1]
	}
	// 2. blocks の rich_text にある user 要素
	if uid := findUserID(m.Blocks); uid != "" {
		return uid
	}
	// 3. 人が直接投稿している場合は投稿者
	if m.User != "" && m.BotID == "" {
		return m.User
	}
	return ""
}

// ReplaceMentions は本文中の <@U12345> を、name(ユーザー ID) の戻り値に置き換える。
func ReplaceMentions(text string, name func(userID string) string) string {
	return mentionRe.ReplaceAllStringFunc(text, func(m string) string {
		return name(mentionRe.FindStringSubmatch(m)[1])
	})
}

// PickOption は value に一致する（なければ value に含まれる）選択肢を返す。なければ空文字。
func PickOption(value string, options []string) string {
	value = strings.TrimSpace(value)
	for _, o := range options {
		if value == o {
			return o
		}
	}
	for _, o := range options {
		if o != "" && strings.Contains(value, o) {
			return o
		}
	}
	return ""
}

// ParseDate は「2026/10/20」「2026年10月20日」などを YYYY-MM-DD にする。解釈できなければ空文字。
func ParseDate(value string) string {
	m := dateRe.FindStringSubmatch(value)
	if m == nil {
		return ""
	}
	var y, mo, d int
	for i, p := range []*int{&y, &mo, &d} {
		n := 0
		for _, r := range m[i+1] {
			n = n*10 + int(r-'0')
		}
		*p = n
	}
	t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	if t.Year() != y || int(t.Month()) != mo || t.Day() != d {
		return ""
	}
	return t.Format("2006-01-02")
}

// ParseCommand はメンション本文から (action, 課題キー) を返す。コマンドでなければ ok=false。
func ParseCommand(text string) (action, key string, ok bool) {
	plain := mentionRe.ReplaceAllString(text, "")
	m := cmdRe.FindStringSubmatch(plain)
	if m == nil {
		return "", "", false
	}
	return commandAliases[strings.ToLower(m[1])], strings.ToUpper(m[2]), true
}
