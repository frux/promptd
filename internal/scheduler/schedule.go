package scheduler

import (
	"fmt"
	"math"
	"time"

	"github.com/frux/promptd/internal/config"
	"github.com/robfig/cron/v3"
)

type calculator interface {
	Initial(time.Time) (time.Time, error)
	Advance(time.Time, time.Time) (time.Time, error)
}

type intervalCalculator struct {
	interval time.Duration
}

func (c intervalCalculator) Initial(now time.Time) (time.Time, error) {
	return now.Add(c.interval), nil
}

func (c intervalCalculator) Advance(current, now time.Time) (time.Time, error) {
	if current.After(now) {
		return current, nil
	}
	elapsed := now.Sub(current)
	steps := int64(elapsed/c.interval) + 1
	if steps > math.MaxInt64/int64(c.interval) {
		return time.Time{}, fmt.Errorf("interval schedule overflow")
	}
	return current.Add(time.Duration(steps) * c.interval), nil
}

type cronCalculator struct {
	schedule cron.Schedule
}

func (c cronCalculator) Initial(now time.Time) (time.Time, error) {
	return requireNext(c.schedule.Next(now))
}

func (c cronCalculator) Advance(_ time.Time, now time.Time) (time.Time, error) {
	return requireNext(c.schedule.Next(now))
}

func newCalculator(schedule config.Schedule) (calculator, error) {
	if schedule.Every != 0 {
		interval := schedule.Every.Value()
		if interval <= 0 {
			return nil, fmt.Errorf("interval must be positive")
		}
		return intervalCalculator{interval: interval}, nil
	}
	if schedule.Cron == "" {
		return nil, fmt.Errorf("schedule has neither cron nor interval")
	}
	expression := "CRON_TZ=" + schedule.Timezone + " " + schedule.Cron
	parsed, err := cron.ParseStandard(expression)
	if err != nil {
		return nil, fmt.Errorf("parse cron schedule: %w", err)
	}
	return cronCalculator{schedule: parsed}, nil
}

func requireNext(next time.Time) (time.Time, error) {
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("schedule has no future occurrence")
	}
	return next.UTC(), nil
}
