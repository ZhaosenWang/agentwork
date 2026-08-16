package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The machine-side sidecar's operational log — connect lifecycle,
// dispatch receipt, run lifecycle, link failures. The daemon sees its own
// half of the link; the CLI's half was stdout-only and vanished with the
// terminal, leaving machine-side stalls invisible. One line per event,
// append-only, ~/.agentwork/cli.log (the authoritative copy), echoed to
// the terminal for the live view.
var (
	cliLogMu   sync.Mutex
	cliLogFile *os.File
	// termEcho is the bounded terminal echo queue — a full queue DROPS the
	// line (the file has it). The echo goroutine is the ONLY thing allowed
	// to block on stdout: a stalled terminal must never block a logging
	// call site (a frozen sidecar is worse than a missed echo).
	termEcho = make(chan string, 64)
)

func init() {
	go func() {
		for line := range termEcho {
			fmt.Print(line)
		}
	}()
}

// cliLogf logs one line to the log file (synchronous — the record of
// record) and best-effort echoes it to the terminal (never blocks).
func cliLogf(format string, args ...any) {
	line := fmt.Sprintf("%s %s\n", time.Now().Format("2006-01-02T15:04:05.000Z07:00"), fmt.Sprintf(format, args...))
	cliLogMu.Lock()
	defer cliLogMu.Unlock()
	if cliLogFile == nil {
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		dir := filepath.Join(home, ".agentwork")
		_ = os.MkdirAll(dir, 0o755)
		f, err := os.OpenFile(filepath.Join(dir, "cli.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		cliLogFile = f
	}
	_, _ = cliLogFile.WriteString(line)
	select {
	case termEcho <- line:
	default: // queue full — the file still has it
	}
}
