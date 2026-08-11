package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/frux/promptd/internal/config"
	"github.com/frux/promptd/internal/store"
)

type Definition struct {
	ID       string
	Schedule config.Schedule
	Misfire  string
}

type Event struct {
	JobID       string
	ScheduledAt time.Time
	Misfired    bool
}

type StateStore interface {
	SchedulerState(context.Context, string) (store.SchedulerState, error)
	SetSchedulerState(context.Context, string, *time.Time, *time.Time) error
}

type Trigger func(Event)

type Engine struct {
	state   StateStore
	trigger Trigger
	now     func() time.Time
	entries map[string]*entry
	pending []Event
}

type Option func(*Engine)

func WithClock(now func() time.Time) Option {
	return func(engine *Engine) {
		engine.now = now
	}
}

type entry struct {
	calculator calculator
	next       time.Time
}

func New(ctx context.Context, state StateStore, definitions []Definition, trigger Trigger, options ...Option) (*Engine, error) {
	if state == nil {
		return nil, fmt.Errorf("scheduler state store is nil")
	}
	if trigger == nil {
		return nil, fmt.Errorf("scheduler trigger is nil")
	}
	engine := &Engine{
		state:   state,
		trigger: trigger,
		now:     func() time.Time { return time.Now().UTC() },
	}
	for _, option := range options {
		option(engine)
	}
	entries, pending, err := engine.prepare(ctx, definitions)
	if err != nil {
		return nil, err
	}
	engine.entries = entries
	engine.pending = pending
	return engine, nil
}

func (e *Engine) Run(ctx context.Context) error {
	e.dispatch(e.pending)
	e.pending = nil

	for {
		next, hasNext := earliest(e.entries)
		var timer *time.Timer
		var timerSignal <-chan time.Time
		if hasNext {
			delay := next.Sub(e.now())
			if delay < 0 {
				delay = 0
			}
			timer = time.NewTimer(delay)
			timerSignal = timer.C
		}

		select {
		case <-ctx.Done():
			stopTimer(timer)
			return nil
		case <-timerSignal:
			if err := e.processDue(ctx); err != nil {
				return err
			}
		}
	}
}

func (e *Engine) prepare(ctx context.Context, definitions []Definition) (map[string]*entry, []Event, error) {
	entries := make(map[string]*entry, len(definitions))
	var pending []Event
	now := e.now().UTC()

	for _, definition := range definitions {
		if definition.ID == "" {
			return nil, nil, fmt.Errorf("scheduler job id is empty")
		}
		if _, exists := entries[definition.ID]; exists {
			return nil, nil, fmt.Errorf("duplicate scheduler job %q", definition.ID)
		}
		if definition.Misfire != "skip" && definition.Misfire != "run_once" {
			return nil, nil, fmt.Errorf("job %q has unsupported misfire policy %q", definition.ID, definition.Misfire)
		}
		calculator, err := newCalculator(definition.Schedule)
		if err != nil {
			return nil, nil, fmt.Errorf("build schedule for %q: %w", definition.ID, err)
		}
		state, err := e.state.SchedulerState(ctx, definition.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, fmt.Errorf("scheduler state for %q is missing", definition.ID)
		}
		if err != nil {
			return nil, nil, err
		}

		var next time.Time
		last := state.LastScheduledAt
		if state.NextRun == nil {
			next, err = calculator.Initial(now)
			if err != nil {
				return nil, nil, fmt.Errorf("initialize schedule for %q: %w", definition.ID, err)
			}
		} else {
			next = state.NextRun.UTC()
			if !next.After(now) {
				if definition.Misfire == "run_once" {
					pending = append(pending, Event{JobID: definition.ID, ScheduledAt: next, Misfired: true})
				}
				lastValue := next
				last = &lastValue
				next, err = calculator.Advance(next, now)
				if err != nil {
					return nil, nil, fmt.Errorf("advance missed schedule for %q: %w", definition.ID, err)
				}
			}
		}
		if state.NextRun == nil || !next.Equal(state.NextRun.UTC()) {
			if err := e.state.SetSchedulerState(ctx, definition.ID, &next, last); err != nil {
				return nil, nil, fmt.Errorf("persist schedule for %q: %w", definition.ID, err)
			}
		}
		entries[definition.ID] = &entry{calculator: calculator, next: next}
	}
	return entries, pending, nil
}

func (e *Engine) processDue(ctx context.Context) error {
	now := e.now().UTC()
	ids := make([]string, 0, len(e.entries))
	for id := range e.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		entry := e.entries[id]
		if entry.next.After(now) {
			continue
		}
		scheduledAt := entry.next
		next, err := entry.calculator.Advance(scheduledAt, now)
		if err != nil {
			return fmt.Errorf("advance schedule for %q: %w", id, err)
		}
		if err := e.state.SetSchedulerState(ctx, id, &next, &scheduledAt); err != nil {
			return fmt.Errorf("persist due schedule for %q: %w", id, err)
		}
		entry.next = next
		e.trigger(Event{JobID: id, ScheduledAt: scheduledAt})
	}
	return nil
}

func (e *Engine) dispatch(events []Event) {
	for _, event := range events {
		e.trigger(event)
	}
}

func earliest(entries map[string]*entry) (time.Time, bool) {
	var result time.Time
	for _, entry := range entries {
		if result.IsZero() || entry.next.Before(result) {
			result = entry.next
		}
	}
	return result, !result.IsZero()
}

func stopTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func Definitions(jobs map[string]config.Job) []Definition {
	ids := make([]string, 0, len(jobs))
	for id := range jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	definitions := make([]Definition, 0, len(ids))
	for _, id := range ids {
		job := jobs[id]
		definitions = append(definitions, Definition{ID: id, Schedule: job.Schedule, Misfire: job.Run.Misfire})
	}
	return definitions
}
