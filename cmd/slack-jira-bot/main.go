package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/sandats/slack-jira-bot/internal/config"
	"github.com/sandats/slack-jira-bot/internal/jira"
	"github.com/sandats/slack-jira-bot/internal/slackapp"
)

func main() {
	os.Exit(run())
}

// run はアプリ本体。終了コードを返す（defer を確実に実行するため os.Exit は main だけで呼ぶ）。
func run() int {
	host, _ := os.Hostname()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("pod", host))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("設定エラー", "error", err)
		return 1
	}

	api := slack.New(cfg.SlackBotToken,
		slack.OptionAppLevelToken(cfg.SlackAppToken),
		// slack-go のデフォルトはタイムアウトなし。trigger_id の期限（3 秒）もあるため短めにする
		slack.OptionHTTPClient(&http.Client{Timeout: 10 * time.Second}),
	)
	sm := socketmode.New(api)
	app := slackapp.New(cfg, api, sm, jira.New(cfg.Jira))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Bot Token の不正は Socket Mode の接続では検知できないため、起動時に確認する
	authCtx, authCancel := context.WithTimeout(ctx, 10*time.Second)
	auth, err := api.AuthTestContext(authCtx)
	authCancel()
	if err != nil {
		slog.Error("Slack の Bot Token を検証できませんでした（SLACK_BOT_TOKEN を確認してください）", "error", err)
		return 1
	}

	// ヘルスチェック用の HTTP サーバー（Slack からの受信には使わない）
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if app.Connected() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("connected"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not connected"))
	})
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("ヘルスチェックサーバーが停止しました", "error", err)
		}
	}()

	// イベントループ（sm.Events は slack-go 側で close されないため、ctx の終了で抜ける）
	loopErr := make(chan error, 1)
	go func() {
		err := slackapp.RunEventLoop(ctx, sm.Events, app.HandleEvent)
		if err != nil {
			stop() // RunContext も止める
		}
		loopErr <- err
	}()

	if cfg.AllowAll {
		slog.Warn("ALLOWED_USER_IDS=* のため、全ユーザーに操作を許可しています")
	}
	slog.Info("起動します",
		"bot_user", auth.UserID,
		"team", auth.Team,
		"set_reporter", cfg.Jira.SetReporter,
		"allow_all", cfg.AllowAll,
		"allowed_users", len(cfg.AllowedIDs),
		"close_transition", cfg.Jira.Transitions["close"].ID,
		"cancel_transition", cfg.Jira.Transitions["cancel"].ID,
	)

	code := 0
	// RunContext は ctx の終了か、再接続できない失敗（トークン不正など）のときに返る
	if err := sm.RunContext(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("Socket Mode が停止しました", "error", err)
		code = 1
	}
	stop() // 異常終了時もイベントループを抜けさせる

	// イベントループの終了を待ってから処理中タスクを待つ（Wait 中に spawn されないようにする）
	if err := <-loopErr; err != nil {
		slog.Error("イベントループが停止しました", "error", err)
		code = 1
	}

	// 終了処理: 処理中の Jira 呼び出しなどの完了を待つ（terminationGracePeriodSeconds 内）
	slog.Info("終了します", "exit_code", code)
	done := make(chan struct{})
	go func() { app.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		slog.Warn("処理中のタスクが残ったまま終了します")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return code
}
