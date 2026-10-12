// Package jira は Jira Cloud REST API v3 のクライアント。
package jira

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sandats/slack-jira-bot/internal/textutil"
)

// Config は Jira への接続とチケット作成の設定。
type Config struct {
	BaseURL     string // チケット URL 表示用（https://xxx.atlassian.net）
	APIBase     string // API 呼び出し用（スコープ付きトークンは api.atlassian.com/ex/jira/<cloudId>）
	Email       string
	APIToken    string
	ProjectKey  string
	IssueTypeID string

	SetReporter bool // true のときだけ reporter を送る
	Transitions map[string]Transition
}

// Transition は close / cancel に対応する Jira の遷移設定。
type Transition struct {
	Label      string
	ID         string
	Resolution string
}

var (
	// Jira のラベルはスペース不可
	requestTypeLabels = map[string]string{
		"アカウント作成": "アカウント作成",
		"CI/CD":   "CI-CD",
		"使い方":     "使い方",
		"その他":     "その他",
	}
	// 右側は Jira の優先度の「名前」と一致させる
	priorityMap = map[string]string{
		"緊急": "Highest",
		"高":  "High",
		"中":  "Medium",
		"通常": "Low",
	}
)

type Client struct {
	cfg Config
	hc  *http.Client
}

func New(cfg Config) *Client {
	return &Client{cfg: cfg, hc: &http.Client{Timeout: 10 * time.Second}}
}

// do は Jira API を呼び出し、(ステータス, 本文) を返す。通信エラーのみ err。
func (j *Client) do(method, path string, query url.Values, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	u := j.cfg.APIBase + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return 0, nil, err
	}
	req.SetBasicAuth(j.cfg.Email, j.cfg.APIToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := j.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, data, err
}

// jiraError は Jira のエラー本文を含めたエラーにする（URL は含めない）。
func jiraError(status int, body []byte, max int) error {
	return fmt.Errorf("%d: %s", status, textutil.Truncate(string(body), max))
}

// ---- ADF（Atlassian Document Format）----

func adfParagraphs(text string) []any {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var out []any
	for line := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, map[string]any{
			"type":    "paragraph",
			"content": []any{map[string]any{"type": "text", "text": line}},
		})
	}
	return out
}

func adfDoc(content []any) map[string]any {
	return map[string]any{"type": "doc", "version": 1, "content": content}
}

// ---- チケット作成 ----

type IssueInput struct {
	Summary      string
	Detail       string
	ReqType      string
	Priority     string
	Due          string // YYYY-MM-DD（任意）
	Requester    string // ショートカットを実行した人
	ReporterName string // 問い合わせ者の表示名
	AssigneeID   string // Jira accountId（任意）
	ReporterID   string // Jira accountId（任意）
}

// buildIssueFields は POST /issue の fields を組み立てる（テストしやすいよう分離）。
func (j *Client) buildIssueFields(in IssueInput) map[string]any {
	summary := textutil.Truncate(textutil.OneLine(in.Summary), 255)
	if summary == "" {
		summary = "(無題)"
	}

	reporterName := in.ReporterName
	if reporterName == "" {
		reporterName = "不明"
	}
	due := in.Due
	if due == "" {
		due = "未指定"
	}
	content := adfParagraphs(fmt.Sprintf(
		"問い合わせ者: %s\n依頼種別: %s\n優先度: %s\n希望対応期限: %s\nSlack 実行者: %s",
		reporterName, in.ReqType, in.Priority, due, in.Requester,
	))
	content = append(content, map[string]any{"type": "rule"})
	if detail := adfParagraphs(in.Detail); len(detail) > 0 {
		content = append(content, detail...)
	} else {
		content = append(content, adfParagraphs("-")...)
	}

	label, ok := requestTypeLabels[in.ReqType]
	if !ok {
		label = in.ReqType
	}
	prio, ok := priorityMap[in.Priority]
	if !ok {
		prio = in.Priority
	}

	fields := map[string]any{
		"project":     map[string]any{"key": j.cfg.ProjectKey},
		"summary":     summary,
		"issuetype":   map[string]any{"id": j.cfg.IssueTypeID},
		"description": adfDoc(content),
		"labels":      []string{label},
		"priority":    map[string]any{"name": prio},
	}
	if in.Due != "" {
		fields["duedate"] = in.Due
	}
	if in.AssigneeID != "" {
		fields["assignee"] = map[string]any{"accountId": in.AssigneeID}
	}
	// reporter は「報告者を変更」権限の付与後に JIRA_SET_REPORTER=1 で有効化する
	if in.ReporterID != "" && j.cfg.SetReporter {
		fields["reporter"] = map[string]any{"accountId": in.ReporterID}
	}
	return fields
}

func (j *Client) CreateIssue(in IssueInput) (string, error) {
	status, body, err := j.do("POST", "/rest/api/3/issue", nil,
		map[string]any{"fields": j.buildIssueFields(in)})
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", jiraError(status, body, 500)
	}
	var res struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &res); err != nil || res.Key == "" {
		return "", fmt.Errorf("課題キーを取得できませんでした: %s", textutil.Truncate(string(body), 200))
	}
	return res.Key, nil
}

// AddRemoteLink は Slack の元メッセージを Web リンクとして追加する。
// 同じ globalId のリンクは重複追加されず更新扱いになる。
func (j *Client) AddRemoteLink(issueKey, linkURL, title, globalID string) error {
	body := map[string]any{"object": map[string]any{"url": linkURL, "title": title}}
	if globalID != "" {
		body["globalId"] = globalID
	}
	status, resp, err := j.do("POST", "/rest/api/3/issue/"+issueKey+"/remotelink", nil, body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return jiraError(status, resp, 300)
	}
	return nil
}

// ---- ユーザー検索 ----

// AccountIDByEmail はメールアドレスから Jira の accountId を引く。見つからなければ空文字。
func (j *Client) AccountIDByEmail(email string) (string, error) {
	status, body, err := j.do("GET", "/rest/api/3/user/search", url.Values{"query": {email}}, nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", jiraError(status, body, 200)
	}
	var users []struct {
		AccountID    string `json:"accountId"`
		EmailAddress string `json:"emailAddress"`
		Active       *bool  `json:"active"`
	}
	if err := json.Unmarshal(body, &users); err != nil {
		return "", err
	}
	// メールが完全一致するユーザーを優先
	for _, u := range users {
		if strings.EqualFold(u.EmailAddress, email) {
			return u.AccountID, nil
		}
	}
	// Jira 側でメールが非公開の場合: 有効なユーザーが 1 件ならそれを採用
	var active []int
	for i, u := range users {
		if u.Active == nil || *u.Active {
			active = append(active, i)
		}
	}
	if len(active) == 1 && users[active[0]].EmailAddress == "" {
		return users[active[0]].AccountID, nil
	}
	return "", nil
}

// ---- ステータス更新（close / cancel）----

func (j *Client) TransitionIssue(issueKey, action string) error {
	conf, ok := j.cfg.Transitions[action]
	if !ok || conf.ID == "" {
		return fmt.Errorf("%s の遷移 ID が未設定です（管理者に連絡してください）", action)
	}
	body := map[string]any{"transition": map[string]any{"id": conf.ID}}
	if conf.Resolution != "" {
		body["fields"] = map[string]any{"resolution": map[string]any{"name": conf.Resolution}}
	}
	status, resp, err := j.do("POST", "/rest/api/3/issue/"+issueKey+"/transitions", nil, body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return jiraError(status, resp, 300)
	}
	return nil
}

// AvailableTransitions はその課題の現在のステータスから実行できる遷移の一覧（失敗時は空）。
func (j *Client) AvailableTransitions(issueKey string) []string {
	status, body, err := j.do("GET", "/rest/api/3/issue/"+issueKey+"/transitions", nil, nil)
	if err != nil || status != http.StatusOK {
		return nil
	}
	var res struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"transitions"`
	}
	if json.Unmarshal(body, &res) != nil {
		return nil
	}
	out := make([]string, 0, len(res.Transitions))
	for _, t := range res.Transitions {
		out = append(out, fmt.Sprintf("%s(id=%s)", t.Name, t.ID))
	}
	return out
}

func (j *Client) StatusName(issueKey string) string {
	status, body, err := j.do("GET", "/rest/api/3/issue/"+issueKey,
		url.Values{"fields": {"status"}}, nil)
	if err != nil || status != http.StatusOK {
		return ""
	}
	var res struct {
		Fields struct {
			Status struct {
				Name string `json:"name"`
			} `json:"status"`
		} `json:"fields"`
	}
	if json.Unmarshal(body, &res) != nil {
		return ""
	}
	return res.Fields.Status.Name
}

func (j *Client) AddComment(issueKey, text string) error {
	status, body, err := j.do("POST", "/rest/api/3/issue/"+issueKey+"/comment", nil,
		map[string]any{"body": adfDoc(adfParagraphs(text))})
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return jiraError(status, body, 300)
	}
	return nil
}
