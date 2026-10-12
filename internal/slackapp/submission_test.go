package slackapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/sandats/slack-jira-bot/internal/config"
	"github.com/sandats/slack-jira-bot/internal/jira"
)

// fakeJira は Jira REST API の代わりに、呼ばれたパスを記録して handler で応答する。
type fakeJira struct {
	mu    sync.Mutex
	paths []string
}

func (f *fakeJira) count(suffix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.paths {
		if strings.HasSuffix(p, suffix) {
			n++
		}
	}
	return n
}

func newSubmissionApp(t *testing.T, setReporter bool, issueStatus int, issueBody string) (*App, *fakeSlack, *fakeJira) {
	t.Helper()
	fs, api := newFakeSlack(t)
	fj := &fakeJira{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fj.mu.Lock()
		fj.paths = append(fj.paths, r.URL.Path)
		fj.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/user/search"):
			_, _ = w.Write([]byte(`[{"accountId":"acc-1","emailAddress":"u@example.com"}]`))
		case strings.HasSuffix(r.URL.Path, "/remotelink"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/issue"):
			w.WriteHeader(issueStatus)
			_, _ = w.Write([]byte(issueBody))
		}
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		AllowedIDs: map[string]struct{}{"U0OK": {}},
		Jira: jira.Config{
			BaseURL: "https://jira.example", APIBase: srv.URL,
			ProjectKey: "OPS", IssueTypeID: "10002", SetReporter: setReporter,
		},
	}
	return New(cfg, api, nil, jira.New(cfg.Jira)), fs, fj
}

func submissionCallback(t *testing.T, user string, meta modalMeta) slack.InteractionCallback {
	t.Helper()
	b, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	var cb slack.InteractionCallback
	cb.Type = slack.InteractionTypeViewSubmission
	cb.User.ID = user
	cb.User.Name = "taro"
	cb.View.CallbackID = modalID
	cb.View.PrivateMetadata = string(b)
	cb.View.State = &slack.ViewState{Values: map[string]map[string]slack.BlockAction{
		"summary":  {"v": {Value: "ログインできない"}},
		"req_type": {"v": {SelectedOption: slack.OptionBlockObject{Value: "使い方"}}},
		"priority": {"v": {SelectedOption: slack.OptionBlockObject{Value: "高"}}},
		"due":      {"v": {}},
		"desc":     {"v": {Value: "詳細です"}},
	}}
	return cb
}

// form は fakeSlack が受け取った form 形式の本文を返す（chat.postMessage / chat.update）。
func (f *fakeSlack) form(t *testing.T, method string) url.Values {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := url.ParseQuery(string(f.bodies[method]))
	if err != nil {
		t.Fatalf("%s の本文を解析できません: %v", method, err)
	}
	return v
}

var buttonMeta = modalMeta{Channel: "C1", TS: "111.222", ThreadTS: "111.222", Author: "U0REQ", ButtonTS: "333.444"}

// 作成に成功したら、ボタンのメッセージを作成済みの表示に置き換えてボタンを消す
func TestHandleSubmission_SuccessRemovesButton(t *testing.T) {
	app, fs, fj := newSubmissionApp(t, false, http.StatusCreated, `{"key":"OPS-7"}`)

	app.handleSubmission(t.Context(), submissionCallback(t, "U0OK", buttonMeta))

	upd := fs.form(t, "chat.update")
	if upd.Get("ts") != "333.444" || upd.Get("channel") != "C1" {
		t.Errorf("chat.update の対象が違います: %v", upd)
	}
	if upd.Get("blocks") != "[]" {
		t.Errorf("ボタンを消すため blocks は空にすること: %q", upd.Get("blocks"))
	}
	if !strings.Contains(upd.Get("text"), "OPS-7") {
		t.Errorf("chat.update の本文: %q", upd.Get("text"))
	}
	if post := fs.form(t, "chat.postMessage"); !strings.Contains(post.Get("text"), "https://jira.example/browse/OPS-7") {
		t.Errorf("スレッドへの返信: %q", post.Get("text"))
	}
	if n := fj.count("/remotelink"); n != 1 {
		t.Errorf("remotelink の呼び出し回数 = %d", n)
	}
	// JIRA_SET_REPORTER が無効なら、問い合わせ者の Jira ユーザー検索はしない（担当者の 1 回だけ）
	if n := fj.count("/user/search"); n != 1 {
		t.Errorf("user/search の呼び出し回数 = %d, want 1", n)
	}
}

func TestHandleSubmission_LooksUpReporterWhenEnabled(t *testing.T) {
	app, _, fj := newSubmissionApp(t, true, http.StatusCreated, `{"key":"OPS-7"}`)
	app.handleSubmission(t.Context(), submissionCallback(t, "U0OK", buttonMeta))
	if n := fj.count("/user/search"); n != 2 {
		t.Errorf("user/search の呼び出し回数 = %d, want 2（担当者と報告者）", n)
	}
}

// ショートカット経由（ボタンなし）のときは chat.update しない
func TestHandleSubmission_NoButtonNoUpdate(t *testing.T) {
	app, fs, _ := newSubmissionApp(t, false, http.StatusCreated, `{"key":"OPS-7"}`)
	meta := buttonMeta
	meta.ButtonTS = ""
	app.handleSubmission(t.Context(), submissionCallback(t, "U0OK", meta))
	if calls := fs.snapshot(); slices.Contains(calls, "chat.update") {
		t.Errorf("chat.update を呼んではいけません: %v", calls)
	}
}

// 作成に失敗しても、Jira のエラー本文はチャンネルに出さない
func TestHandleSubmission_FailureHidesJiraError(t *testing.T) {
	app, fs, _ := newSubmissionApp(t, false, http.StatusBadRequest,
		`{"errors":{"customfield_10010":"secret-internal-detail"}}`)

	app.handleSubmission(t.Context(), submissionCallback(t, "U0OK", buttonMeta))

	text := fs.form(t, "chat.postMessage").Get("text")
	if strings.Contains(text, "secret-internal-detail") || strings.Contains(text, "400") {
		t.Errorf("Jira のエラー詳細が投稿に含まれています: %q", text)
	}
	if !strings.Contains(text, "参照 ID") {
		t.Errorf("参照 ID がありません: %q", text)
	}
	if slices.Contains(fs.snapshot(), "chat.update") {
		t.Error("失敗時にボタンを消してはいけません")
	}
}

type ackRecord struct {
	envelopeID string
	payload    []any
}

func recordAcks(app *App) *[]ackRecord {
	var acks []ackRecord
	app.ackFn = func(req socketmode.Request, payload ...any) error {
		acks = append(acks, ackRecord{req.EnvelopeID, payload})
		return nil
	}
	return &acks
}

// 権限のないユーザーの送信は、モーダルを閉じずにエラーを返す（response_action: errors）
func TestHandleEvent_DeniedSubmissionShowsError(t *testing.T) {
	app, fs, fj := newSubmissionApp(t, false, http.StatusCreated, `{"key":"OPS-7"}`)
	acks := recordAcks(app)

	app.HandleEvent(socketmode.Event{
		Type:    socketmode.EventTypeInteractive,
		Data:    submissionCallback(t, "U0NG", buttonMeta),
		Request: &socketmode.Request{EnvelopeID: "env-1"},
	})
	app.Wait()

	if len(*acks) != 1 || len((*acks)[0].payload) != 1 {
		t.Fatalf("acks = %+v", *acks)
	}
	raw, _ := json.Marshal((*acks)[0].payload[0])
	if !strings.Contains(string(raw), `"response_action":"errors"`) || !strings.Contains(string(raw), `"summary"`) {
		t.Errorf("ack の payload: %s", raw)
	}
	if len(fs.snapshot()) != 0 || len(fj.paths) != 0 {
		t.Errorf("権限のない送信で API を呼んではいけません: slack=%v jira=%v", fs.snapshot(), fj.paths)
	}
}

// 中身を解釈できないイベントでも ack は返す（返さないと Slack から再送される）
func TestHandleEvent_AcksUnexpectedData(t *testing.T) {
	for _, typ := range []socketmode.EventType{socketmode.EventTypeEventsAPI, socketmode.EventTypeInteractive} {
		app, _, _ := newSubmissionApp(t, false, http.StatusCreated, `{}`)
		acks := recordAcks(app)
		app.HandleEvent(socketmode.Event{Type: typ, Data: "unexpected", Request: &socketmode.Request{EnvelopeID: "env-2"}})
		if len(*acks) != 1 || (*acks)[0].envelopeID != "env-2" {
			t.Errorf("%s: acks = %+v", typ, *acks)
		}
	}
}
