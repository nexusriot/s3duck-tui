package utils

import (
	"math/rand"

	"github.com/atotto/clipboard"
)

func SplitFunc(r rune) bool {
	return r == '/'
}

func RandStr(n int) string {
	const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Int63()%int64(len(letterBytes))]
	}
	return string(b)
}

// CopyToClipboard puts text on the system clipboard. The error is the
// caller's to show: on Linux it needs xclip, xsel or wl-clipboard, which an
// SSH session or a small board often lacks — and discarding it made every
// "copied" message a lie on exactly those machines.
func CopyToClipboard(text string) error {
	return clipboard.WriteAll(text)
}
