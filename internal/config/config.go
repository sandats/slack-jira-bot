// Package config は環境変数からアプリの設定を読み込む。
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/sandats/slack-jira-bot/internal/jira"
)

type Config struct {
	SlackBotToken string
	SlackAppToken string

	Jira jira.Config

	AllowAll   bool                // ALLOWED_USER_IDS=* のときだけ全員許可
	AllowedIDs map[string]struct{} // 許可するユーザー ID

	HTTPAddr string // ヘルスチェック用
}

// Load は環境変数から設定を読み込む。必須項目の不足や不正な値はエラーにする。
func Load() (*Config, error) {
	required := []string{
		"SLACK_BOT_TOKEN", "SLACK_APP_TOKEN",
		"JIRA_BASE_URL", "JIRA_API_BASE", "JIRA_EMAIL", "JIRA_API_TOKEN",
		"JIRA_PROJECT_KEY", "JIRA_ISSUE_TYPE_ID",
		// 未設定で全員に操作を許してしまわないよう必須にする（全員許可は * を明示）
		"ALLOWED_USER_IDS",
	}
	var missing []string
	for _, k := range required {
		if strings.TrimSpace(os.Getenv(k)) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("環境変数が不足しています: %s", strings.Join(missing, ", "))
	}

	cfg := &Config{
		SlackBotToken: os.Getenv("SLACK_BOT_TOKEN"),
		SlackAppToken: os.Getenv("SLACK_APP_TOKEN"),
		Jira: jira.Config{
			BaseURL:     strings.TrimRight(os.Getenv("JIRA_BASE_URL"), "/"),
			APIBase:     strings.TrimRight(os.Getenv("JIRA_API_BASE"), "/"),
			Email:       os.Getenv("JIRA_EMAIL"),
			APIToken:    os.Getenv("JIRA_API_TOKEN"),
			ProjectKey:  os.Getenv("JIRA_PROJECT_KEY"),
			IssueTypeID: os.Getenv("JIRA_ISSUE_TYPE_ID"),
			SetReporter: os.Getenv("JIRA_SET_REPORTER") == "1",
			Transitions: map[string]jira.Transition{
				"close": {
					Label:      "close",
					ID:         os.Getenv("JIRA_CLOSE_TRANSITION_ID"),
					Resolution: os.Getenv("JIRA_CLOSE_RESOLUTION"),
				},
				"cancel": {
					Label:      "cancel",
					ID:         os.Getenv("JIRA_CANCEL_TRANSITION_ID"),
					Resolution: os.Getenv("JIRA_CANCEL_RESOLUTION"),
				},
			},
		},
		AllowedIDs: map[string]struct{}{},
		HTTPAddr:   getenvDefault("HTTP_ADDR", ":8080"),
	}

	// トークンの取り違え（xoxb と xapp の逆設定など）は起動時に気づけるようにする
	if !strings.HasPrefix(cfg.SlackBotToken, "xoxb-") {
		return nil, fmt.Errorf("SLACK_BOT_TOKEN は xoxb- で始まる Bot Token を設定してください")
	}
	if !strings.HasPrefix(cfg.SlackAppToken, "xapp-") {
		return nil, fmt.Errorf("SLACK_APP_TOKEN は xapp- で始まる App-Level Token を設定してください")
	}

	for id := range strings.SplitSeq(os.Getenv("ALLOWED_USER_IDS"), ",") {
		switch id = strings.TrimSpace(id); id {
		case "":
		case "*":
			cfg.AllowAll = true
		default:
			cfg.AllowedIDs[id] = struct{}{}
		}
	}
	if !cfg.AllowAll && len(cfg.AllowedIDs) == 0 {
		return nil, fmt.Errorf("ALLOWED_USER_IDS に許可するユーザー ID を設定してください（全員許可する場合は *）")
	}
	return cfg, nil
}

// IsAllowed は実行を許可されたユーザーかを返す。許可リストにないユーザーは拒否する。
func (c *Config) IsAllowed(userID string) bool {
	if c.AllowAll {
		return true
	}
	_, ok := c.AllowedIDs[userID]
	return ok
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
