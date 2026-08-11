//go:build !linux && !darwin

package supervisor

import "time"

func blockIgnoringTermination() {
	ignoreTermination()
	for {
		time.Sleep(time.Second)
	}
}

func ignoreTermination() {}
