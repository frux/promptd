//go:build linux || darwin

package supervisor

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestRunKillsDescendantProcesses(t *testing.T) {
	heartbeat := t.TempDir() + "/heartbeat.log"
	output := newNotifyingBuffer()
	spec := helperSpec("spawn-child", output)
	spec.Command = append(spec.Command, heartbeat)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct {
		result Result
		err    error
	}, 1)
	go func() {
		result, err := New(WithGracePeriod(20*time.Millisecond)).Run(ctx, spec)
		finished <- struct {
			result Result
			err    error
		}{result: result, err: err}
	}()

	select {
	case <-output.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("child process did not start")
	}
	cancel()

	select {
	case outcome := <-finished:
		if outcome.err != nil {
			t.Fatalf("Run() error = %v", outcome.err)
		}
		if !outcome.result.Canceled {
			t.Fatalf("result = %#v", outcome.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not terminate process tree")
	}

	time.Sleep(50 * time.Millisecond)
	before, err := os.Stat(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	after, err := os.Stat(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("descendant is still writing: size grew from %d to %d", before.Size(), after.Size())
	}
}
