package inquiry

import (
	"encoding/json"
	"testing"
)

func TestParseInquiry_PlainLabels(t *testing.T) {
	text := "お問い合わせの内容（タイトル）: GitLab にログインできない\n" +
		"依頼種別: CI/CD\n優先度: 高\n希望対応期限: 2026/10/20\n" +
		"お問い合わせの詳細: 昨日から 500 エラーです。\n2 行目です。"
	got := Parse(text)
	want := map[string]string{
		"title":    "GitLab にログインできない",
		"type":     "CI/CD",
		"priority": "高",
		"due":      "2026/10/20",
		"detail":   "昨日から 500 エラーです。\n2 行目です。",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q, want %q", k, got[k], v)
		}
	}
}

func TestParseInquiry_BoldLabelsNewlineValues(t *testing.T) {
	text := "*お問い合わせの内容（タイトル）*\nアカウントを作りたい\n*依頼種別*\nアカウント作成\n*優先度*\n中"
	got := Parse(text)
	if got["title"] != "アカウントを作りたい" || got["type"] != "アカウント作成" || got["priority"] != "中" {
		t.Errorf("unexpected: %#v", got)
	}
}

func TestParseInquiry_NoLabels(t *testing.T) {
	if got := Parse("ただの文章です"); len(got) != 0 {
		t.Errorf("expected empty, got %#v", got)
	}
}

func TestPickOptionAndDate(t *testing.T) {
	if got := PickOption("高 (至急)", Priorities); got != "高" {
		t.Errorf("pickOption partial: %q", got)
	}
	if got := PickOption("不明", Priorities); got != "" {
		t.Errorf("pickOption none: %q", got)
	}
	cases := map[string]string{
		"2026/10/20":    "2026-10-20",
		"2026年1月5日":     "2026-01-05",
		"2026-02-30":    "", // 存在しない日付
		"来週中":           "",
		"期限 2026.12.1 ": "2026-12-01",
	}
	for in, want := range cases {
		if got := ParseDate(in); got != want {
			t.Errorf("ParseDate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseCommand(t *testing.T) {
	cases := []struct {
		in         string
		action, id string
		ok         bool
	}{
		{"<@U0BOT> close ops-123", "close", "OPS-123", true},
		{"<@U0BOT>  cansel OPS-9", "cancel", "OPS-9", true},
		{"<@U0BOT> CANCEL OPS-9", "cancel", "OPS-9", true},
		{"<@U0BOT> クローズ OPS-1", "close", "OPS-1", true},
		{"<@U0BOT> close 123", "", "", false}, // プロジェクトキーなし
		{"<@U0BOT> お願いします", "", "", false},
	}
	for _, c := range cases {
		a, k, ok := ParseCommand(c.in)
		if a != c.action || k != c.id || ok != c.ok {
			t.Errorf("ParseCommand(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, a, k, ok, c.action, c.id, c.ok)
		}
	}
}

// ワークフロー投稿（Bot 投稿・rich_text 内にメンション）を想定
func TestExtractInquiry_RichTextWithMention(t *testing.T) {
	raw := `[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[
	  {"type":"text","text":"依頼者: "},{"type":"user","user_id":"U0REQ1"},
	  {"type":"text","text":"\nお問い合わせの内容（タイトル）: 権限申請\n依頼種別: 使い方\n優先度: 通常\nお問い合わせの詳細: 教えてください"}
	]}]}]`
	var blocks []any
	if err := json.Unmarshal([]byte(raw), &blocks); err != nil {
		t.Fatal(err)
	}
	msg := Message{Text: "(fallback)", BotID: "B0WF", Blocks: blocks}
	full, parsed := Extract(msg)
	if parsed["title"] != "権限申請" || parsed["type"] != "使い方" || parsed["priority"] != "通常" {
		t.Fatalf("parsed: %#v", parsed)
	}
	if got := FindRequesterID(msg, full); got != "U0REQ1" {
		t.Errorf("requester = %q", got)
	}
}

func TestFindRequesterID_Fallbacks(t *testing.T) {
	if got := FindRequesterID(Message{User: "U0HUMAN"}, "本文"); got != "U0HUMAN" {
		t.Errorf("human author: %q", got)
	}
	if got := FindRequesterID(Message{User: "U0BOT", BotID: "B1"}, "本文"); got != "" {
		t.Errorf("bot author must not be requester: %q", got)
	}
}
