// Package slackapp は Slack（Socket Mode）のイベントを受けて Jira を操作する。
package slackapp

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/sandats/slack-jira-bot/internal/config"
	"github.com/sandats/slack-jira-bot/internal/inquiry"
	"github.com/sandats/slack-jira-bot/internal/jira"
	"github.com/sandats/slack-jira-bot/internal/textutil"
)

const (
	callbackID      = "create_jira_ticket" // メッセージショートカットの Callback ID
	modalID         = "jira_ticket_modal"
	actionOpenModal = "open_create_modal"

	denyText = "この操作を実行する権限がありません。管理者にお問い合わせください。"
)

// App は Slack のイベントを受けて Jira を操作する。
type App struct {
	cfg  *config.Config
	api  *slack.Client
	sm   *socketmode.Client
	jira *jira.Client

	wg        sync.WaitGroup // 処理中のゴルーチン（終了時に待つ）
	names     sync.Map       // Slack ユーザー ID -> 表示名
	accounts  sync.Map       // Slack ユーザー ID -> Jira accountId（解決できたものだけ）
	connected atomic.Bool    // Socket Mode に接続中か（readiness 用）
}

func New(cfg *config.Config, api *slack.Client, sm *socketmode.Client, jc *jira.Client) *App {
	return &App{cfg: cfg, api: api, sm: sm, jira: jc}
}

// Connected は Socket Mode に接続中かを返す（readiness 用）。
func (a *App) Connected() bool { return a.connected.Load() }

// modalMeta は private_metadata に入れる状態。
// 複数 Pod のどれがモーダルを開き、どれが送信を受けるか不定のため、状態は Pod に持たない。
type modalMeta struct {
	Channel  string `json:"channel"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
	Author   string `json:"author"` // 問い合わせ者（Slack ユーザー ID）
}

// buttonValue は「Jira チケットを作成」ボタンの value に入れる状態。
type buttonValue struct {
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts"`
}

// spawn は処理をゴルーチンで実行する（イベントループを塞がない）。
func (a *App) spawn(name string, f func()) {
	a.wg.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("パニックを回復しました", "task", name, "panic", fmt.Sprint(r))
			}
		}()
		f()
	})
}

// Wait は処理中のゴルーチンの完了を待つ。
func (a *App) Wait() { a.wg.Wait() }

// ---------------------------------------------------------------
// イベントの振り分け
// ---------------------------------------------------------------

func (a *App) HandleEvent(evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeConnecting:
		slog.Info("Slack に接続中")
	case socketmode.EventTypeConnected:
		a.connected.Store(true)
		slog.Info("Slack に接続しました")
	case socketmode.EventTypeConnectionError, socketmode.EventTypeDisconnect:
		a.connected.Store(false)
		slog.Warn("Slack との接続が切れました（自動で再接続します）", "type", string(evt.Type))

	case socketmode.EventTypeEventsAPI:
		ev, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok || evt.Request == nil {
			return
		}
		a.ack(*evt.Request) // 3 秒以内に先に ack
		slog.Info("受信", "type", "events_api", "event", ev.InnerEvent.Type)
		if m, ok := ev.InnerEvent.Data.(*slackevents.AppMentionEvent); ok {
			a.spawn("mention", func() { a.handleMention(m) })
		}

	case socketmode.EventTypeInteractive:
		cb, ok := evt.Data.(slack.InteractionCallback)
		if !ok || evt.Request == nil {
			return
		}
		a.ack(*evt.Request) // 3 秒以内に先に ack（view_submission はこれでモーダルが閉じる）
		slog.Info("受信", "type", "interactive", "interaction", string(cb.Type), "callback_id", cb.CallbackID)
		a.dispatchInteraction(cb)
	}
}

// ack は Socket Mode のリクエストに応答する。失敗すると Slack から再送されることがあるため記録しておく。
func (a *App) ack(req socketmode.Request) {
	if err := a.sm.Ack(req); err != nil {
		slog.Error("ack に失敗", "envelope_id", req.EnvelopeID, "type", req.Type, "error", err)
	}
}

func (a *App) dispatchInteraction(cb slack.InteractionCallback) {
	switch cb.Type {
	case slack.InteractionTypeMessageAction, slack.InteractionTypeShortcut:
		if cb.CallbackID != callbackID {
			return
		}
		a.spawn("shortcut", func() { a.handleShortcut(cb) })
	case slack.InteractionTypeBlockActions:
		a.spawn("block_actions", func() { a.handleBlockActions(cb) })
	case slack.InteractionTypeViewSubmission:
		if cb.View.CallbackID != modalID {
			return
		}
		if !a.cfg.IsAllowed(cb.User.ID) {
			slog.Warn("権限のないユーザーが送信", "user", cb.User.ID, "name", cb.User.Name)
			return
		}
		a.spawn("submission", func() { a.handleSubmission(cb) })
	}
}

// ---------------------------------------------------------------
// ショートカット / ボタン
// ---------------------------------------------------------------

func (a *App) handleShortcut(cb slack.InteractionCallback) {
	uid, uname, ch := cb.User.ID, cb.User.Name, cb.Channel.ID
	if !a.cfg.IsAllowed(uid) {
		slog.Warn("権限のないユーザーが実行", "user", uid, "name", uname, "channel", ch)
		if ch != "" {
			// 元メッセージがスレッド内なら親の ts、そうでなければ元メッセージの ts
			threadTS := cb.Message.ThreadTimestamp
			if threadTS == "" {
				threadTS = cb.Message.Timestamp
			}
			a.replyEphemeral(ch, uid, threadTS, denyText)
		}
		return
	}
	// メッセージショートカットは元メッセージが payload に含まれる
	load := func() (slack.Message, error) { return cb.Message, nil }
	if err := a.openModal(cb.TriggerID, ch, load); err != nil {
		slog.Error("モーダルの表示に失敗", "user", uid, "name", uname, "channel", ch, "error", err)
	}
}

func (a *App) handleBlockActions(cb slack.InteractionCallback) {
	uid, uname := cb.User.ID, cb.User.Name

	var act *slack.BlockAction
	for _, b := range cb.ActionCallback.BlockActions {
		if b != nil && b.ActionID == actionOpenModal {
			act = b
			break
		}
	}
	if act == nil {
		return
	}

	var v buttonValue
	if err := json.Unmarshal([]byte(act.Value), &v); err != nil || v.Channel == "" || v.ThreadTS == "" {
		slog.Error("ボタンの値の解析に失敗", "user", uid, "error", err)
		return
	}

	if !a.cfg.IsAllowed(uid) {
		slog.Warn("権限のないユーザーがボタンを押下", "user", uid, "name", uname, "channel", v.Channel)
		a.replyEphemeral(v.Channel, uid, v.ThreadTS, denyText)
		return
	}

	// 親メッセージ（問い合わせ本文）は、モーダルを開いた後に取得する
	load := func() (slack.Message, error) {
		msgs, _, _, err := a.api.GetConversationReplies(&slack.GetConversationRepliesParameters{
			ChannelID: v.Channel,
			Timestamp: v.ThreadTS,
			Limit:     1,
			Inclusive: true,
		})
		if err != nil {
			return slack.Message{}, err
		}
		if len(msgs) == 0 {
			return slack.Message{}, fmt.Errorf("親メッセージがありません")
		}
		return msgs[0], nil
	}
	if err := a.openModal(cb.TriggerID, v.Channel, load); err != nil {
		slog.Error("モーダルの表示に失敗", "user", uid, "name", uname, "channel", v.Channel, "ts", v.ThreadTS, "error", err)
	}
}

// ---------------------------------------------------------------
// モーダル
// ---------------------------------------------------------------

func plain(text string) *slack.TextBlockObject {
	return slack.NewTextBlockObject(slack.PlainTextType, text, false, false)
}

func inputBlock(blockID, label string, optional bool, el slack.BlockElement) *slack.InputBlock {
	b := slack.NewInputBlock(blockID, plain(label), nil, el)
	b.Optional = optional
	return b
}

func selectBlock(blockID, label string, options []string, selected string) *slack.InputBlock {
	opts := make([]*slack.OptionBlockObject, 0, len(options))
	for _, o := range options {
		opts = append(opts, slack.NewOptionBlockObject(o, plain(o), nil))
	}
	el := slack.NewOptionsSelectBlockElement(slack.OptTypeStatic, plain("選択してください"), "v", opts...)
	if selected != "" {
		el.InitialOption = slack.NewOptionBlockObject(selected, plain(selected), nil)
	}
	return inputBlock(blockID, label, false, el)
}

func toGeneric(v any) []any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out []any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

// toInquiryMessage は slack-go のメッセージを、Slack 非依存の表現へ変換する。
func toInquiryMessage(m slack.Message) inquiry.Message {
	im := inquiry.Message{
		Text:     m.Text,
		User:     m.User,
		BotID:    m.BotID,
		TS:       m.Timestamp,
		ThreadTS: m.ThreadTimestamp,
		Blocks:   toGeneric(m.Blocks),
	}
	for _, at := range m.Attachments {
		im.Attachments = append(im.Attachments, inquiry.Attachment{
			Pretext: at.Pretext,
			Text:    at.Text,
			Blocks:  toGeneric(at.Blocks),
		})
	}
	return im
}

// noticeView はメッセージだけを表示するモーダル（送信ボタンなし）。
func noticeView(text string) slack.ModalViewRequest {
	return slack.ModalViewRequest{
		Type:  slack.VTModal,
		Title: plain("Jira チケット作成"),
		Close: plain("閉じる"),
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			slack.NewSectionBlock(plain(text), nil, nil),
		}},
	}
}

// openModal はモーダルを開く。trigger_id は 3 秒で失効するため、先に読み込み中のモーダルを開き、
// 元メッセージの取得・解析（Slack API の呼び出しを伴う）が終わってから views.update で中身を差し替える。
func (a *App) openModal(triggerID, channel string, load func() (slack.Message, error)) error {
	opened, err := a.api.OpenView(triggerID, noticeView("元のメッセージを読み込んでいます…"))
	if err != nil {
		return fmt.Errorf("views.open: %w", err)
	}

	view := noticeView("元のメッセージを取得できませんでした。")
	msg, loadErr := load()
	if loadErr == nil {
		view = a.buildModal(channel, msg)
	}
	// hash を渡し、利用者が閉じた・別の更新が入ったなどで状態が変わっていれば上書きしない
	if _, err := a.api.UpdateView(view, "", opened.Hash, opened.ID); err != nil {
		return fmt.Errorf("views.update: %w", err)
	}
	if loadErr != nil {
		return fmt.Errorf("元メッセージの取得: %w", loadErr)
	}
	return nil
}

// buildModal は元メッセージを解析して、入力済みのチケット作成モーダルを組み立てる。
func (a *App) buildModal(channel string, msg slack.Message) slack.ModalViewRequest {
	im := toInquiryMessage(msg)
	fullText, parsed := inquiry.Extract(im)

	var title, detail string
	if len(parsed) > 0 {
		title, detail = parsed["title"], parsed["detail"]
	} else {
		// 書式が想定と違う場合: 先頭行をタイトル、全文を詳細にする
		if t := strings.TrimSpace(fullText); t != "" {
			title = strings.SplitN(t, "\n", 2)[0]
		}
		detail = fullText
		slog.Warn("元メッセージを項目に分解できませんでした（全文を詳細に設定）")
	}

	// 問い合わせ者の特定は、メンション置換の前の本文で行う
	requesterID := inquiry.FindRequesterID(im, fullText)
	// Slack の記法（<URL|表示名>、&amp; など）を普通のテキストに戻してから Jira へ渡す
	title = inquiry.FormatText(a.replaceMentions(title))
	detail = inquiry.FormatText(a.replaceMentions(detail))

	reqType := inquiry.PickOption(parsed["type"], inquiry.RequestTypes)
	priority := inquiry.PickOption(parsed["priority"], inquiry.Priorities)
	due := inquiry.ParseDate(parsed["due"])

	threadTS := msg.ThreadTimestamp
	if threadTS == "" {
		threadTS = msg.Timestamp
	}
	meta, _ := json.Marshal(modalMeta{Channel: channel, TS: msg.Timestamp, ThreadTS: threadTS, Author: requesterID})

	summaryEl := slack.NewPlainTextInputBlockElement(nil, "v")
	summaryEl.InitialValue = textutil.Truncate(textutil.OneLine(title), 255)
	summaryEl.MaxLength = 255

	dueEl := slack.NewDatePickerBlockElement("v")
	dueEl.InitialDate = due

	descEl := slack.NewPlainTextInputBlockElement(nil, "v")
	descEl.Multiline = true
	descEl.InitialValue = textutil.Truncate(detail, 3000) // Slack の上限

	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      modalID,
		PrivateMetadata: string(meta),
		Title:           plain("Jira チケット作成"),
		Submit:          plain("作成"),
		Close:           plain("キャンセル"),
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			inputBlock("summary", "お問い合わせの内容（タイトル）", false, summaryEl),
			selectBlock("req_type", "依頼種別", inquiry.RequestTypes, reqType),
			selectBlock("priority", "優先度", inquiry.Priorities, priority),
			inputBlock("due", "希望対応期限", true, dueEl),
			inputBlock("desc", "お問い合わせの詳細", true, descEl),
		}},
	}
}

func (a *App) handleSubmission(cb slack.InteractionCallback) {
	user := cb.User.ID
	if cb.View.State == nil {
		slog.Error("モーダル入力が空です", "user", user)
		return
	}
	vals := cb.View.State.Values
	summary := vals["summary"]["v"].Value
	reqType := vals["req_type"]["v"].SelectedOption.Value
	priority := vals["priority"]["v"].SelectedOption.Value
	due := vals["due"]["v"].SelectedDate
	detail := vals["desc"]["v"].Value

	var meta modalMeta
	if err := json.Unmarshal([]byte(cb.View.PrivateMetadata), &meta); err != nil {
		slog.Error("モーダルの状態の解析に失敗", "user", user, "error", err)
		return
	}

	permalink := ""
	if meta.Channel != "" && meta.TS != "" {
		p, err := a.api.GetPermalink(&slack.PermalinkParameters{Channel: meta.Channel, Ts: meta.TS})
		if err != nil {
			slog.Error("permalink の取得に失敗（続行します）", "error", err)
		} else {
			permalink = p
		}
	}

	requester := fmt.Sprintf("%s (%s)", cb.User.Name, user)
	assigneeID := a.jiraAccountID(user) // ショートカットを押した人
	reporterID := a.jiraAccountID(meta.Author)
	reporterName := ""
	if meta.Author != "" {
		reporterName = a.replaceMentions("<@" + meta.Author + ">")
	}

	var notes []string
	if assigneeID == "" {
		notes = append(notes, "担当者を設定できませんでした")
	}
	if a.cfg.Jira.SetReporter && meta.Author != "" && reporterID == "" {
		notes = append(notes, "報告者を設定できませんでした")
	}

	var text string
	key, err := a.jira.CreateIssue(jira.IssueInput{
		Summary: summary, Detail: detail, ReqType: reqType, Priority: priority, Due: due,
		Requester: requester, ReporterName: reporterName,
		AssigneeID: assigneeID, ReporterID: reporterID,
	})
	if err != nil {
		slog.Error("Jira チケット作成に失敗", "user", user, "error", err)
		text = fmt.Sprintf("<@%s> Jira チケットの作成に失敗しました: %v", user, err)
	} else {
		slog.Info("Jira チケット作成成功", "key", key, "user", user)
		text = fmt.Sprintf("<@%s> チケットを作成しました: %s/browse/%s", user, a.cfg.Jira.BaseURL, key)

		// 元メッセージを Web リンクとして追加（失敗してもチケットは残す）
		if permalink != "" {
			gid := fmt.Sprintf("slack-%s-%s", meta.Channel, meta.TS)
			if err := a.jira.AddRemoteLink(key, permalink, "Slack の元メッセージ", gid); err != nil {
				slog.Error("Web リンクの追加に失敗", "key", key, "user", user, "error", err)
				notes = append(notes, "Web リンクを追加できませんでした")
			}
		}
		if len(notes) > 0 {
			text += "\n（注意: " + strings.Join(notes, " / ") + "）"
		}
	}

	if meta.Channel != "" {
		a.replyThread(meta.Channel, meta.ThreadTS, text)
	} else {
		a.replyThread(user, "", text) // グローバルショートカットの場合は DM
	}
}

// ---------------------------------------------------------------
// メンション（@App / close / cancel）
// ---------------------------------------------------------------

func (a *App) handleMention(ev *slackevents.AppMentionEvent) {
	uid, ch := ev.User, ev.Channel
	if ev.BotID != "" || uid == "" || ch == "" {
		return
	}
	replyTS := ev.ThreadTimeStamp
	if replyTS == "" {
		replyTS = ev.TimeStamp
	}
	slog.Info("メンション受信", "user", uid, "channel", ch, "in_thread", ev.ThreadTimeStamp != "")

	if !a.cfg.IsAllowed(uid) {
		slog.Warn("権限のないユーザーがメンション", "user", uid, "channel", ch)
		a.replyEphemeral(ch, uid, replyTS, denyText)
		return
	}

	if action, key, ok := inquiry.ParseCommand(ev.Text); ok {
		a.runTransitionCommand(uid, ch, replyTS, action, key)
		return
	}

	if ev.ThreadTimeStamp == "" {
		a.replyEphemeral(ch, uid, replyTS, fmt.Sprintf(
			"問い合わせメッセージのスレッド内でメンションしてください。\n"+
				"ステータス更新は `@App close %[1]s-123` / `@App cancel %[1]s-123` の形式です。",
			a.cfg.Jira.ProjectKey))
		return
	}
	if err := a.postCreateButton(ch, ev.ThreadTimeStamp); err != nil {
		slog.Error("作成ボタンの投稿に失敗", "channel", ch, "error", err)
	}
}

func (a *App) postCreateButton(ch, threadTS string) error {
	val, _ := json.Marshal(buttonValue{Channel: ch, ThreadTS: threadTS})
	btn := slack.NewButtonBlockElement(actionOpenModal, string(val),
		slack.NewTextBlockObject(slack.PlainTextType, "Jira チケットを作成", false, false))
	btn.Style = slack.StylePrimary

	section := slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, "このスレッドの問い合わせから Jira チケットを作成します。", false, false),
		nil, nil)
	_, _, err := a.api.PostMessage(ch,
		slack.MsgOptionTS(threadTS),
		slack.MsgOptionText("Jira チケットを作成できます", false),
		slack.MsgOptionBlocks(section, slack.NewActionBlock("", btn)),
	)
	return err
}

func (a *App) runTransitionCommand(uid, ch, replyTS, action, key string) {
	conf := a.cfg.Jira.Transitions[action]
	prefix, _, _ := strings.Cut(key, "-")
	if !strings.EqualFold(prefix, a.cfg.Jira.ProjectKey) {
		a.replyThread(ch, replyTS,
			fmt.Sprintf("<@%s> %s は対象外のプロジェクトです（対象: %s）", uid, key, a.cfg.Jira.ProjectKey))
		return
	}

	if err := a.jira.TransitionIssue(key, action); err != nil {
		slog.Error("ステータス更新に失敗", "key", key, "action", action, "user", uid, "error", err)
		hint := ""
		if avail := a.jira.AvailableTransitions(key); len(avail) > 0 {
			hint = "\n現在実行できる遷移: " + strings.Join(avail, ", ")
		}
		a.replyThread(ch, replyTS,
			fmt.Sprintf("<@%s> %s を %s できませんでした: %v%s", uid, key, conf.Label, err, hint))
		return
	}
	slog.Info("ステータス更新成功", "key", key, "action", action, "user", uid)

	// 遷移はサービスアカウントで実行されるため、指示した人をコメントに残す
	uname := a.replaceMentions("<@" + uid + ">")
	if err := a.jira.AddComment(key, fmt.Sprintf("Slack から %s (%s) が %s を実行しました。", uname, uid, conf.Label)); err != nil {
		slog.Error("コメントの追加に失敗（続行します）", "key", key, "error", err)
	}

	text := fmt.Sprintf("<@%s> %s を %s しました", uid, key, conf.Label)
	if st := a.jira.StatusName(key); st != "" {
		text += fmt.Sprintf("（現在のステータス: %s）", st)
	}
	text += fmt.Sprintf("\n%s/browse/%s", a.cfg.Jira.BaseURL, key)
	a.replyThread(ch, replyTS, text)
}

// ---------------------------------------------------------------
// 返信・ユーザー解決
// ---------------------------------------------------------------

// replyEphemeral は本人にだけ見えるメッセージをスレッドに返す。失敗してもログのみ。
func (a *App) replyEphemeral(ch, user, threadTS, text string) {
	opts := []slack.MsgOption{slack.MsgOptionText(text, false)}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	if _, err := a.api.PostEphemeral(ch, user, opts...); err != nil {
		slog.Error("一時メッセージの送信に失敗", "user", user, "channel", ch, "error", err)
	}
}

func (a *App) replyThread(ch, threadTS, text string) {
	opts := []slack.MsgOption{slack.MsgOptionText(text, false)}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	if _, _, err := a.api.PostMessage(ch, opts...); err != nil {
		slog.Error("Slack への投稿に失敗", "channel", ch, "error", err)
	}
}

// replaceMentions は <@U12345> を @表示名 に置換する（結果は Pod 内でキャッシュ）。
func (a *App) replaceMentions(text string) string {
	return inquiry.ReplaceMentions(text, func(uid string) string {
		if v, ok := a.names.Load(uid); ok {
			return v.(string)
		}
		name := "@" + uid
		if u, err := a.api.GetUserInfo(uid); err == nil {
			switch {
			case u.RealName != "":
				name = "@" + u.RealName
			case u.Name != "":
				name = "@" + u.Name
			}
			a.names.Store(uid, name)
		}
		return name
	})
}

// jiraAccountID は Slack ユーザー ID から Jira の accountId を返す。解決できなければ空文字。
func (a *App) jiraAccountID(slackUserID string) string {
	if slackUserID == "" {
		return ""
	}
	if v, ok := a.accounts.Load(slackUserID); ok {
		return v.(string)
	}
	u, err := a.api.GetUserInfo(slackUserID)
	if err != nil {
		slog.Error("Slack ユーザー情報の取得に失敗", "user", slackUserID, "error", err)
		return ""
	}
	if u.IsBot {
		return ""
	}
	email := u.Profile.Email
	if email == "" {
		slog.Warn("Slack ユーザーのメールを取得できません", "user", slackUserID)
		return ""
	}
	id, err := a.jira.AccountIDByEmail(email)
	if err != nil {
		slog.Warn("Jira ユーザー検索に失敗", "user", slackUserID, "error", err)
		return ""
	}
	if id == "" {
		slog.Warn("Jira に一致するユーザーがいません", "user", slackUserID)
		return ""
	}
	a.accounts.Store(slackUserID, id)
	return id
}
