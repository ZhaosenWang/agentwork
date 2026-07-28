package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// cronParser accepts standard 5-field cron expressions.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ComputeNextRun evaluates cronExpr in the named IANA timezone and returns the
// next activation strictly after `after`, in UTC. This is the building block
// the daemon's schedule tick uses to advance schedule.next_run_at.
func ComputeNextRun(cronExpr, timezone string, after time.Time) (time.Time, error) {
	sched, loc, err := parseCronSchedule(cronExpr, timezone)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(after.In(loc)).UTC(), nil
}

// ValidateCron parses cronExpr + timezone and returns an error if either is
// invalid. Used by ScheduleService.Create to reject bad input with a 400.
func ValidateCron(cronExpr, timezone string) error {
	_, _, err := parseCronSchedule(cronExpr, timezone)
	return err
}

func parseCronSchedule(cronExpr, timezone string) (cron.Schedule, *time.Location, error) {
	// robfig v3 reads an optional "TZ="/"CRON_TZ=" prefix up to the first
	// space and panics when that space is missing. Reject the shape the
	// parser cannot survive instead of turning a typo into a panic.
	if (strings.HasPrefix(cronExpr, "TZ=") || strings.HasPrefix(cronExpr, "CRON_TZ=")) &&
		!strings.Contains(cronExpr, " ") {
		return nil, nil, fmt.Errorf("%w: missing schedule after timezone prefix %q", ErrValidation, cronExpr)
	}
	sched, err := cronParser.Parse(cronExpr)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: invalid timezone %q: %v", ErrValidation, timezone, err)
	}
	return sched, loc, nil
}
