//go:build linux || darwin

package supervisor

import (
	"os/signal"
	"syscall"
	"time"
)

func blockIgnoringTermination() {
	ignoreTermination()
	for {
		time.Sleep(time.Second)
	}
}

func ignoreTermination() {
	signal.Ignore(syscall.SIGTERM)
}
