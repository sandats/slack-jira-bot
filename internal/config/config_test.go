package config

import (
	"strings"
	"testing"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"SLACK_BOT_TOKEN":    "xoxb-test",
		"SLACK_APP_TOKEN":    "xapp-test",
		"JIRA_BASE_URL":      "https://example.atlassian.net/",
		"JIRA_API_BASE":      "https://api.atlassian.com/ex/jira/cloud",
		"JIRA_EMAIL":         "svc@example.com",
		"JIRA_API_TOKEN":     "tok",
		"JIRA_PROJECT_KEY":   "OPS",
		"JIRA_ISSUE_TYPE_ID": "10002",
		"ALLOWED_USER_IDS":   "U1, U2",
	} {
		t.Setenv(k, v)
	}
}

func TestLoadConfig_AllowList(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AllowAll || !cfg.IsAllowed("U1") || !cfg.IsAllowed("U2") || cfg.IsAllowed("U3") {
		t.Errorf("allow list: all=%v ids=%v", cfg.AllowAll, cfg.AllowedIDs)
	}
	if cfg.Jira.BaseURL != "https://example.atlassian.net" {
		t.Errorf("Jira.BaseURL = %q", cfg.Jira.BaseURL)
	}
}

func TestLoadConfig_AllowAllMustBeExplicit(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("ALLOWED_USER_IDS", "*")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowAll || !cfg.IsAllowed("U999") {
		t.Error("* must allow everyone")
	}
}

func TestLoadConfig_EmptyAllowListIsRejected(t *testing.T) {
	for _, v := range []string{"", "  ", " , "} {
		setRequiredEnv(t)
		t.Setenv("ALLOWED_USER_IDS", v)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ALLOWED_USER_IDS") {
			t.Errorf("ALLOWED_USER_IDS=%q: err = %v", v, err)
		}
	}
}

func TestIsAllowed_ZeroValueDeniesAll(t *testing.T) {
	// 設定の組み立て漏れがあっても全員許可にならないこと
	if (&Config{}).IsAllowed("U1") {
		t.Error("zero Config must deny")
	}
}

func TestLoadConfig_TokenPrefix(t *testing.T) {
	cases := map[string][2]string{
		"SLACK_BOT_TOKEN": {"xapp-swapped", "SLACK_BOT_TOKEN"},
		"SLACK_APP_TOKEN": {"xoxb-swapped", "SLACK_APP_TOKEN"},
	}
	for key, c := range cases {
		setRequiredEnv(t)
		t.Setenv(key, c[0])
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), c[1]) {
			t.Errorf("%s=%q: err = %v", key, c[0], err)
		}
	}
}
