package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/frux/promptd/internal/config"
	"github.com/frux/promptd/internal/store"
)

func TestIntervalCalculatorStaysAnchored(t *testing.T) {
	calculator := intervalCalculator{interval: 15 * time.Minute}
	current := time.Date(2026, 8, 11, 8, 0, 0, 0, time.UTC)
	now := current.Add(47 * time.Minute)
	next, err := calculator.Advance(current, now)
	if err != nil {
		t.Fatal(err)
	}
	want := current.Add(time.Hour)
	if !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}
}

func TestCronCalculatorUsesConfiguredTimezone(t *testing.T) {
	calculator, err := newCalculator(config.Schedule{Cron: "0 9 * * *", Timezone: "Asia/Yekaterinburg"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 11, 3, 30, 0, 0, time.UTC)
	next, err := calculator.Initial(now)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 8, 11, 4, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}
}

func TestEngineRunsOneMissedOccurrence(t *testing.T) {
	now := time.Date(2026, 8, 11, 9, 30, 0, 0, time.UTC)
	missed := now.Add(-2 * time.Hour)
	state := newMemoryState("report", &missed)
	events := make(chan Event, 1)
	engine, err := New(context.Background(), state, []Definition{{
		ID: "report",
		Schedule: config.Schedule{
			Every:    config.Duration(time.Hour),
			Timezone: "Local",
		},
		Misfire: "run_once",
	}}, func(event Event) { events <- event }, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- engine.Run(ctx) }()
	select {
	case event := <-events:
		if event.JobID != "report" || !event.Misfired || !event.ScheduledAt.Equal(missed) {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("missed event was not triggered")
	}
	cancel()
	if err := <-finished; err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	persisted, err := state.SchedulerState(context.Background(), "report")
	if err != nil {
		t.Fatal(err)
	}
	wantNext := now.Add(time.Hour)
	if persisted.NextRun == nil || !persisted.NextRun.Equal(wantNext) {
		t.Fatalf("next run = %v, want %v", persisted.NextRun, wantNext)
	}
}

func TestEngineSkipsMissedOccurrence(t *testing.T) {
	now := time.Date(2026, 8, 11, 9, 30, 0, 0, time.UTC)
	missed := now.Add(-2 * time.Hour)
	state := newMemoryState("report", &missed)
	engine, err := New(context.Background(), state, []Definition{{
		ID: "report",
		Schedule: config.Schedule{
			Every:    config.Duration(time.Hour),
			Timezone: "Local",
		},
		Misfire: "skip",
	}}, func(Event) {}, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if len(engine.pending) != 0 {
		t.Fatalf("pending events = %#v, want none", engine.pending)
	}
	persisted, err := state.SchedulerState(context.Background(), "report")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.NextRun == nil || !persisted.NextRun.Equal(now.Add(time.Hour)) {
		t.Fatalf("next run = %v", persisted.NextRun)
	}
}

func TestEngineFiresInterval(t *testing.T) {
	state := newMemoryState("heartbeat", nil)
	events := make(chan Event, 1)
	engine, err := New(context.Background(), state, []Definition{{
		ID: "heartbeat",
		Schedule: config.Schedule{
			Every:    config.Duration(20 * time.Millisecond),
			Timezone: "Local",
		},
		Misfire: "skip",
	}}, func(event Event) { events <- event })
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- engine.Run(ctx) }()
	select {
	case event := <-events:
		if event.JobID != "heartbeat" || event.Misfired {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("interval event was not triggered")
	}
	cancel()
	if err := <-finished; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

type memoryState struct {
	mu     sync.Mutex
	states map[string]store.SchedulerState
}

func newMemoryState(jobID string, next *time.Time) *memoryState {
	return &memoryState{states: map[string]store.SchedulerState{
		jobID: {JobID: jobID, NextRun: next},
	}}
}

func (s *memoryState) SchedulerState(_ context.Context, jobID string) (store.SchedulerState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, exists := s.states[jobID]
	if !exists {
		return store.SchedulerState{}, store.ErrNotFound
	}
	return cloneState(state), nil
}

func (s *memoryState) SetSchedulerState(_ context.Context, jobID string, next, last *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, exists := s.states[jobID]
	if !exists {
		return errors.New("missing state")
	}
	state.NextRun = cloneTime(next)
	state.LastScheduledAt = cloneTime(last)
	s.states[jobID] = state
	return nil
}

func cloneState(state store.SchedulerState) store.SchedulerState {
	state.NextRun = cloneTime(state.NextRun)
	state.LastScheduledAt = cloneTime(state.LastScheduledAt)
	return state
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := *value
	return &result
}
