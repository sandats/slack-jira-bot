package slackapp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/slack-go/slack/socketmode"
)

// waitResult は RunEventLoop の戻り値を待つ。返らなければ失敗にする。
func waitResult(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("RunEventLoop が返りません")
		return nil
	}
}

// slack-go は Events を close しないため、ctx の終了だけでループを抜けられること
func TestRunEventLoop_StopsOnContextCancel(t *testing.T) {
	events := make(chan socketmode.Event) // close しない
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- RunEventLoop(ctx, events, func(socketmode.Event) {}) }()

	cancel()
	if err := waitResult(t, res); err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

func TestRunEventLoop_HandlesEventsInOrder(t *testing.T) {
	events := make(chan socketmode.Event, 3)
	events <- socketmode.Event{Type: socketmode.EventTypeConnecting}
	events <- socketmode.Event{Type: socketmode.EventTypeConnected}
	close(events)

	var got []socketmode.EventType
	err := RunEventLoop(context.Background(), events, func(e socketmode.Event) { got = append(got, e.Type) })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != socketmode.EventTypeConnecting || got[1] != socketmode.EventTypeConnected {
		t.Errorf("handled = %v", got)
	}
}

func TestRunEventLoop_InvalidAuthReturnsError(t *testing.T) {
	events := make(chan socketmode.Event, 2)
	events <- socketmode.Event{Type: socketmode.EventTypeInvalidAuth}
	events <- socketmode.Event{Type: socketmode.EventTypeConnected} // 処理されてはいけない

	handled := 0
	res := make(chan error, 1)
	go func() {
		res <- RunEventLoop(context.Background(), events, func(socketmode.Event) { handled++ })
	}()
	if err := waitResult(t, res); !errors.Is(err, ErrInvalidAuth) {
		t.Errorf("err = %v, want ErrInvalidAuth", err)
	}
	if handled != 0 {
		t.Errorf("handled = %d, want 0", handled)
	}
}
