package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Small shared helpers. The v2 transcript scraper lived here too; v3 reads a
// turn's result off the `claude -p` event stream instead, so it is gone
// (DESIGN §11).

// htmlEscape escapes special HTML characters for Telegram HTML parse mode.
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// truncate shortens a string to n characters (rune-safe enough for previews).
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// hasFlag reports whether args contains a bare flag, e.g. `ccc doctor --fix`.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// hookLog writes to the debug log (the name predates v3; the file is the same
// one the listener has always used, so existing logs stay readable).
func hookLog(format string, args ...interface{}) {
	f, err := os.OpenFile(filepath.Join(cacheDir(), "hook-debug.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}
