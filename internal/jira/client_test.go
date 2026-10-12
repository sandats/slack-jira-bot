package jira

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, handler http.HandlerFunc, setReporter bool) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(Config{
		APIBase:     srv.URL,
		Email:       "svc@example.com",
		APIToken:    "tok",
		ProjectKey:  "OPS",
		IssueTypeID: "10002",
		SetReporter: setReporter,
		Transitions: map[string]Transition{
			"close":  {Label: "close", ID: "31", Resolution: "Done"},
			"cancel": {Label: "cancel"}, // 未設定
		},
	})
}
func sampleInput() IssueInput {
	return IssueInput{
		Summary:      "ログイン\nできない",
		Detail:       "詳細1\n\n詳細2",
		ReqType:      "CI/CD",
		Priority:     "緊急",
		Due:          "2026-10-20",
		Requester:    "taro (U1)",
		ReporterName: "@花子",
		AssigneeID:   "acc-assignee",
		ReporterID:   "acc-reporter",
	}
}

func TestBuildIssueFields(t *testing.T) {
	j := testClient(t, nil, false)
	f := j.buildIssueFields(sampleInput())

	if f["summary"] != "ログイン できない" {
		t.Errorf("summary: %v", f["summary"])
	}
	if f["priority"].(map[string]any)["name"] != "Highest" {
		t.Errorf("priority: %v", f["priority"])
	}
	if f["labels"].([]string)[0] != "CI-CD" {
		t.Errorf("labels: %v", f["labels"])
	}
	if f["duedate"] != "2026-10-20" {
		t.Errorf("duedate: %v", f["duedate"])
	}
	if f["assignee"].(map[string]any)["accountId"] != "acc-assignee" {
		t.Errorf("assignee: %v", f["assignee"])
	}
	if _, ok := f["reporter"]; ok {
		t.Error("reporter must be omitted when JIRA_SET_REPORTER is off")
	}
	if f["issuetype"].(map[string]any)["id"] != "10002" {
		t.Errorf("issuetype: %v", f["issuetype"])
	}

	// SET_REPORTER 有効時のみ reporter を送る
	f2 := testClient(t, nil, true).buildIssueFields(sampleInput())
	if f2["reporter"].(map[string]any)["accountId"] != "acc-reporter" {
		t.Errorf("reporter: %v", f2["reporter"])
	}

	// 期限・担当者なし
	in := sampleInput()
	in.Due, in.AssigneeID = "", ""
	f3 := j.buildIssueFields(in)
	if _, ok := f3["duedate"]; ok {
		t.Error("duedate must be omitted")
	}
	if _, ok := f3["assignee"]; ok {
		t.Error("assignee must be omitted")
	}
}

func TestBuildIssueFields_SummaryLimitAndEmpty(t *testing.T) {
	j := testClient(t, nil, false)
	in := sampleInput()
	in.Summary = strings.Repeat("あ", 300)
	if got := []rune(j.buildIssueFields(in)["summary"].(string)); len(got) != 255 {
		t.Errorf("summary length = %d", len(got))
	}
	in.Summary = " \n "
	if got := j.buildIssueFields(in)["summary"]; got != "(無題)" {
		t.Errorf("empty summary = %v", got)
	}
}

func TestCreateIssue_RequestAndErrors(t *testing.T) {
	var gotPath, gotAuthUser string
	var gotBody map[string]any
	j := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuthUser, _, _ = r.BasicAuth()
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"key":"OPS-42"}`))
	}, false)

	key, err := j.CreateIssue(sampleInput())
	if err != nil || key != "OPS-42" {
		t.Fatalf("key=%q err=%v", key, err)
	}
	if gotPath != "/rest/api/3/issue" || gotAuthUser != "svc@example.com" {
		t.Errorf("path=%q user=%q", gotPath, gotAuthUser)
	}
	if _, ok := gotBody["fields"]; !ok {
		t.Error("body must contain fields")
	}

	// 400 のときは Jira のエラー本文を含める
	j2 := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"errors":{"reporter":"Field 'reporter' cannot be set."}}`))
	}, true)
	_, err = j2.CreateIssue(sampleInput())
	if err == nil || !strings.HasPrefix(err.Error(), "400: ") || !strings.Contains(err.Error(), "reporter") {
		t.Errorf("err = %v", err)
	}
}

func TestAccountIDByEmail(t *testing.T) {
	respond := func(body string) *Client {
		return testClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("query") != "a@example.com" {
				t.Errorf("query = %q", r.URL.Query().Get("query"))
			}
			_, _ = w.Write([]byte(body))
		}, false)
	}

	// 完全一致を優先（大文字小文字は区別しない）
	id, _ := respond(`[{"accountId":"x","emailAddress":"other@example.com"},{"accountId":"y","emailAddress":"A@Example.com"}]`).AccountIDByEmail("a@example.com")
	if id != "y" {
		t.Errorf("exact match: %q", id)
	}
	// メール非公開で有効ユーザー 1 件
	id, _ = respond(`[{"accountId":"z","active":true}]`).AccountIDByEmail("a@example.com")
	if id != "z" {
		t.Errorf("hidden email single: %q", id)
	}
	// 複数候補で一致なし → 採用しない
	id, _ = respond(`[{"accountId":"p"},{"accountId":"q"}]`).AccountIDByEmail("a@example.com")
	if id != "" {
		t.Errorf("ambiguous must be empty: %q", id)
	}
}

func TestTransitionIssue(t *testing.T) {
	var body map[string]any
	var path string
	j := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.WriteHeader(204)
	}, false)

	if err := j.TransitionIssue("OPS-1", "close"); err != nil {
		t.Fatal(err)
	}
	if path != "/rest/api/3/issue/OPS-1/transitions" {
		t.Errorf("path = %q", path)
	}
	if body["transition"].(map[string]any)["id"] != "31" {
		t.Errorf("transition body: %v", body)
	}
	if body["fields"].(map[string]any)["resolution"].(map[string]any)["name"] != "Done" {
		t.Errorf("resolution body: %v", body)
	}
	// 遷移 ID 未設定
	if err := j.TransitionIssue("OPS-1", "cancel"); err == nil || !strings.Contains(err.Error(), "未設定") {
		t.Errorf("unset transition err = %v", err)
	}
}

func TestAvailableTransitionsAndStatus(t *testing.T) {
	j := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/transitions"):
			_, _ = w.Write([]byte(`{"transitions":[{"id":"31","name":"Close"},{"id":"41","name":"Cancel"}]}`))
		default:
			_, _ = w.Write([]byte(`{"fields":{"status":{"name":"Closed"}}}`))
		}
	}, false)
	got := j.AvailableTransitions("OPS-1")
	if len(got) != 2 || got[0] != "Close(id=31)" {
		t.Errorf("transitions: %v", got)
	}
	if s := j.StatusName("OPS-1"); s != "Closed" {
		t.Errorf("status: %q", s)
	}
}
