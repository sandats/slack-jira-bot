// Package textutil は文字列の整形に使う小さな関数をまとめる。
package textutil

import "strings"

// Truncate は文字数（rune）で切り詰める。
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// OneLine は改行や連続した空白を 1 つの空白にまとめる。
func OneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
