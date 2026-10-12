package slackapp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/slack-go/slack"

	"github.com/sandats/slack-jira-bot/internal/config"
)

// fakeSlack は Slack Web API の代わりに呼び出し順と送信内容を記録する。
type fakeSlack struct {
	mu      sync.Mutex
	calls   []string          // 呼ばれた API メソッド名（順番どおり）
	bodies  map[string][]byte // メソッド名 -> 最後のリクエスト本文
	replies string            // conversations.replies の応答
}

func newFakeSlack(t *testing.T) (*fakeSlack, *slack.Client) {
	t.Helper()
	f := &fakeSlack{bodies: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
}

func (f *fakeSlack) serve(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/")
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.bodies[method] = body
	replies := f.replies
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch method {
	case "views.open":
		_, _ = w.Write([]byte(`{"ok":true,"view":{"id":"V1","hash":"h1"}}`))
	case "views.update":
		_, _ = w.Write([]byte(`{"ok":true,"view":{"id":"V1","hash":"h2"}}`))
	case "conversations.replies":
		_, _ = w.Write([]byte(replies))
	case "users.info":
		_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"U0REQ","name":"hanako","real_name":"花子"}}`))
	default:
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

func (f *fakeSlack) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// updatedView は views.update で送られた view と hash / view_id を返す。
func (f *fakeSlack) updatedView(t *testing.T) (view map[string]any, hash, viewID string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var req struct {
		View   map[string]any `json:"view"`
		Hash   string         `json:"hash"`
		ViewID string         `json:"view_id"`
	}
	if err := json.Unmarshal(f.bodies["views.update"], &req); err != nil {
		t.Fatalf("views.update の本文を解析できません: %v", err)
	}
	return req.View, req.Hash, req.ViewID
}

func newTestApp(t *testing.T) (*App, *fakeSlack) {
	t.Helper()
	f, api := newFakeSlack(t)
	cfg := &config.Config{AllowedIDs: map[string]struct{}{"U0OK": {}}}
	return New(cfg, api, nil, nil), f
}

func openButtonCallback(user string) slack.InteractionCallback {
	var cb slack.InteractionCallback
	cb.Type = slack.InteractionTypeBlockActions
	cb.User.ID = user
	cb.TriggerID = "trigger-1"
	cb.ActionCallback.BlockActions = []*slack.BlockAction{{
		ActionID: actionOpenModal,
		Value:    `{"channel":"C1","thread_ts":"111.222"}`,
	}}
	return cb
}

const repliesOK = `{"ok":true,"has_more":false,"messages":[{"type":"message","user":"U0REQ","ts":"111.222",
 "text":"お問い合わせの内容（タイトル）: <@U0REQ> のログイン不具合\n依頼種別: 使い方\n優先度: 高\nお問い合わせの詳細: 詳しく"}]}`

// trigger_id の失効を避けるため、他の Slack API より先に views.open を呼ぶこと
func TestHandleBlockActions_OpensModalBeforeSlowCalls(t *testing.T) {
	app, f := newTestApp(t)
	f.replies = repliesOK

	app.handleBlockActions(t.Context(), openButtonCallback("U0OK"))

	calls := f.snapshot()
	if len(calls) == 0 || calls[0] != "views.open" {
		t.Fatalf("最初の呼び出しが views.open ではありません: %v", calls)
	}
	if calls[len(calls)-1] != "views.update" {
		t.Fatalf("最後に views.update で差し替えていません: %v", calls)
	}
	for _, want := range []string{"conversations.replies", "users.info"} {
		if !slices.Contains(calls, want) {
			t.Errorf("%s が呼ばれていません: %v", want, calls)
		}
	}

	view, hash, viewID := f.updatedView(t)
	if viewID != "V1" || hash != "h1" {
		t.Errorf("view_id=%q hash=%q（views.open の応答を使うこと）", viewID, hash)
	}
	if view["callback_id"] != modalID {
		t.Errorf("callback_id = %v", view["callback_id"])
	}
	raw, _ := json.Marshal(view)
	for _, want := range []string{
		`"initial_value":"@花子 のログイン不具合"`, // メンションは表示名に置換
		`\"author\":\"U0REQ\"`,           // private_metadata に問い合わせ者
		`\"thread_ts\":\"111.222\"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("view に %s が含まれません: %s", want, raw)
		}
	}
}

func TestHandleShortcut_OpensModalFirst(t *testing.T) {
	app, f := newTestApp(t)
	var cb slack.InteractionCallback
	cb.Type = slack.InteractionTypeMessageAction
	cb.CallbackID = callbackID
	cb.User.ID = "U0OK"
	cb.Channel.ID = "C1"
	cb.TriggerID = "trigger-1"
	cb.Message.Timestamp = "111.222"
	cb.Message.Text = "お問い合わせの内容（タイトル）: <@U0REQ> 権限申請\n依頼種別: 使い方"

	app.handleShortcut(t.Context(), cb)

	calls := f.snapshot()
	if len(calls) < 3 || calls[0] != "views.open" || calls[len(calls)-1] != "views.update" {
		t.Fatalf("calls = %v", calls)
	}
	if !slices.Contains(calls, "users.info") {
		t.Errorf("users.info が呼ばれていません: %v", calls)
	}
}

// 親メッセージを取得できないときは、開いたモーダルをエラー表示に差し替える
func TestHandleBlockActions_LoadFailureUpdatesModal(t *testing.T) {
	app, f := newTestApp(t)
	f.replies = `{"ok":false,"error":"channel_not_found"}`

	app.handleBlockActions(t.Context(), openButtonCallback("U0OK"))

	calls := f.snapshot()
	if len(calls) != 3 || calls[0] != "views.open" || calls[1] != "conversations.replies" || calls[2] != "views.update" {
		t.Fatalf("calls = %v", calls)
	}
	view, _, _ := f.updatedView(t)
	raw, _ := json.Marshal(view)
	if !strings.Contains(string(raw), "取得できませんでした") {
		t.Errorf("エラー表示になっていません: %s", raw)
	}
	if _, ok := view["submit"]; ok {
		t.Error("エラー表示のモーダルに送信ボタンがあってはいけません")
	}
}

func TestHandleBlockActions_DeniedUser(t *testing.T) {
	app, f := newTestApp(t)
	app.handleBlockActions(t.Context(), openButtonCallback("U0NG"))

	calls := f.snapshot()
	if len(calls) != 1 || calls[0] != "chat.postEphemeral" {
		t.Fatalf("calls = %v（拒否時はモーダルを開かず一時メッセージのみ）", calls)
	}
}

func TestHandleShortcut_DeniedUser(t *testing.T) {
	app, f := newTestApp(t)
	var cb slack.InteractionCallback
	cb.User.ID = "U0NG"
	cb.Channel.ID = "C1"
	cb.TriggerID = "trigger-1"
	cb.Message.Timestamp = "111.222"

	app.handleShortcut(t.Context(), cb)

	if calls := f.snapshot(); slices.Contains(calls, "views.open") {
		t.Errorf("権限のないユーザーにモーダルを開いています: %v", calls)
	}
}
