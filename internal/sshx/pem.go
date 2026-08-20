package sshx

import (
	"encoding/pem"
	"strings"
)

func pemBytes(b *pem.Block) []byte { return pem.EncodeToMemory(b) }

// shellQuote single-quotes s for a POSIX shell, escaping embedded quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
