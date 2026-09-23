package convert

import (
	"fmt"
	"os"
	"strings"
)

// Version is written into the labels this package produces.
const Version = "1.2.0"

// progressBar draws a one-line bar on stderr when it is a terminal.
func progressBar() func(done, total int64) {
	fi, err := os.Stderr.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return nil
	}
	last := -1
	return func(done, total int64) {
		if total == 0 {
			return
		}
		pct := int(done * 100 / total)
		if pct == last {
			return
		}
		last = pct
		const width = 30
		n := pct * width / 100
		fmt.Fprintf(os.Stderr, "\r  [%s%s] %3d%%", strings.Repeat("#", n), strings.Repeat("-", width-n), pct)
	}
}
