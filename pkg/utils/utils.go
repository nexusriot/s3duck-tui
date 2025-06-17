package utils

import (
	"github.com/atotto/clipboard"
	"math/rand"
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
func CopyToClipboard(text string) {
	_ = clipboard.WriteAll(text)
}
