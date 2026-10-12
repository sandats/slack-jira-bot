package textutil

import "testing"

func TestOneLineAndTruncate(t *testing.T) {
	if got := OneLine("a\n  b\t c"); got != "a b c" {
		t.Errorf("OneLine: %q", got)
	}
	if got := Truncate("あいうえお", 3); got != "あいう" {
		t.Errorf("Truncate: %q", got)
	}
}
