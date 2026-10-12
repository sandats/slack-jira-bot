package slackapp

import (
	"context"
	"errors"

	"github.com/slack-go/slack/socketmode"
)

// ErrInvalidAuth は Socket Mode の認証失敗（再試行しても直らない）を表す。
var ErrInvalidAuth = errors.New("Slack の認証に失敗しました（App-Level Token を確認してください）")

// RunEventLoop は Socket Mode のイベントを handle に渡す。
// slack-go は Events を close しないため ctx の終了で抜ける。
// ctx が終了したら nil、認証失敗のイベントを受けたら ErrInvalidAuth を返す。
func RunEventLoop(ctx context.Context, events <-chan socketmode.Event, handle func(socketmode.Event)) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case evt, ok := <-events:
			if !ok {
				return nil
			}
			if evt.Type == socketmode.EventTypeInvalidAuth {
				return ErrInvalidAuth
			}
			handle(evt)
		}
	}
}
