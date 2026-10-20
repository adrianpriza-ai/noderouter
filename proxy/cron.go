package proxy

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A small 5-field cron parser: "minute hour day-of-month month day-of-week".
// Also understands @hourly / @daily / @midnight / @weekly / @monthly / @yearly
// and @every <duration>. Standard library only, no dependencies.

type Schedule struct {
	expr    string
	minute  [64]bool
	hour    [64]bool
	dom     [64]bool
	month   [64]bool
	dow     [64]bool
	domStar bool
	dowStar bool
	every   time.Duration
	isEvery bool
}

var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var dayNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// ParseSchedule compiles a cron expression.
func ParseSchedule(expr string) (*Schedule, error) {
	s := &Schedule{expr: strings.TrimSpace(expr)}
	if s.expr == "" {
		return nil, fmt.Errorf("empty schedule")
	}

	low := strings.ToLower(s.expr)
	if strings.HasPrefix(low, "@") {
		rest := strings.TrimSpace(low[1:])
		name, arg, _ := strings.Cut(rest, " ")
		switch name {
		case "every":
			d, err := time.ParseDuration(strings.TrimSpace(arg))
			if err != nil || d <= 0 {
				return nil, fmt.Errorf("bad @every duration %q", arg)
			}
			s.isEvery, s.every = true, d
			return s, nil
		case "hourly":
			low = "0 * * * *"
		case "daily", "midnight":
			low = "0 0 * * *"
		case "weekly":
			low = "0 0 * * 0"
		case "monthly":
			low = "0 0 1 * *"
		case "yearly", "annually":
			low = "0 0 1 1 *"
		default:
			return nil, fmt.Errorf("unknown schedule %q", s.expr)
		}
	}

	fields := strings.Fields(low)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron %q needs 5 fields (min hour dom month dow), got %d", s.expr, len(fields))
	}
	var err error
	if s.minute, err = parseField(fields[0], 0, 59, nil, "minute"); err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	if s.hour, err = parseField(fields[1], 0, 23, nil, "hour"); err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	if s.dom, err = parseField(fields[2], 1, 31, nil, "day of month"); err != nil {
		return nil, fmt.Errorf("day of month: %w", err)
	}
	if s.month, err = parseField(fields[3], 1, 12, monthNames, "month"); err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	if s.dow, err = parseField(fields[4], 0, 7, dayNames, "day of week"); err != nil {
		return nil, fmt.Errorf("day of week: %w", err)
	}
	// Some cron implementations use 7 for Sunday (Go's time.Sunday is 0).
	if s.dow[7] {
		s.dow[0] = true
	}
	s.domStar = fields[2] == "*" || fields[2] == "?"
	s.dowStar = fields[4] == "*" || fields[4] == "?"
	return s, nil
}

func parseField(field string, lo, hi int, names map[string]int, fieldName string) ([64]bool, error) {
	var out [64]bool
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return out, fmt.Errorf("%s: empty term in %q", fieldName, field)
		}
		step := 1
		if base, stepStr, ok := strings.Cut(part, "/"); ok {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n <= 0 {
				return out, fmt.Errorf("%s: bad step %q", fieldName, stepStr)
			}
			step = n
			part = base
		}
		start, end := lo, hi
		switch {
		case part == "*" || part == "?":
		default:
			if a, b, ok := strings.Cut(part, "-"); ok {
				var err error
				if start, err = parseValue(a, names); err != nil {
					return out, fmt.Errorf("%s: %w", fieldName, err)
				}
				if end, err = parseValue(b, names); err != nil {
					return out, fmt.Errorf("%s: %w", fieldName, err)
				}
			} else {
				v, err := parseValue(part, names)
				if err != nil {
					return out, fmt.Errorf("%s: %w", fieldName, err)
				}
				start, end = v, v
				if step > 1 { // "5/15" means "from 5 onwards, every 15"
					end = hi
				}
			}
		}
		if start < lo || end > hi || start > end {
			return out, fmt.Errorf("%s: value out of range %d-%d in %q", fieldName, lo, hi, field)
		}
		for v := start; v <= end; v += step {
			out[v] = true
		}
	}
	return out, nil
}

func parseValue(s string, names map[string]int) (int, error) {
	s = strings.TrimSpace(s)
	if names != nil {
		if v, ok := names[s]; ok {
			return v, nil
		}
		if len(s) > 3 {
			if v, ok := names[s[:3]]; ok {
				return v, nil
			}
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("not a number: %q", s)
	}
	return v, nil
}

// matches reports whether t fires the schedule. Day-of-month and day-of-week
// are OR-ed when both are restricted, matching standard cron.
func (s *Schedule) matches(t time.Time) bool {
	if s.isEvery {
		return true
	}
	return s.minute[t.Minute()] && s.hour[t.Hour()] && s.dayMatches(t)
}

// Next returns the first firing time strictly after t.
func (s *Schedule) Next(t time.Time) time.Time {
	if s.isEvery {
		return t.Add(s.every - time.Duration(t.Nanosecond()))
	}
	loc := t.Location()
	cur := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc).Add(time.Minute)
	// A schedule can be sparse, so skip whole units of time that cannot match
	// instead of always stepping a minute. The unit is picked from the fields
	// that already failed to match, so no candidate is ever jumped over.
	const limit = 5 * 366 * 24 * 60
	for i := 0; i < limit; i++ {
		if s.matches(cur) {
			return cur
		}
		switch {
		case !s.dayMatches(cur):
			// Advance to the same wall-clock time tomorrow, then zero to
			// midnight. AddDate is DST-safe (adds exactly one calendar day).
			tomorrow := cur.AddDate(0, 0, 1)
			cur = time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 0, 0, 0, 0, loc)
		case !s.hour[cur.Hour()]:
			// Jump to the next hour boundary. time.Date normalises
			// non-existent times during spring-forward and picks the
			// first occurrence during fall-back.
			cur = time.Date(cur.Year(), cur.Month(), cur.Day(), cur.Hour()+1, 0, 0, 0, loc)
		case !s.minute[cur.Minute()]:
			cur = time.Date(cur.Year(), cur.Month(), cur.Day(), cur.Hour(), cur.Minute()+1, 0, 0, loc)
		default:
			cur = cur.Add(time.Minute)
		}
	}
	return time.Time{}
}

// dayMatches reports whether the day-of-month/month/day-of-week fields allow
// this date at all.
func (s *Schedule) dayMatches(t time.Time) bool {
	if !s.month[int(t.Month())] {
		return false
	}
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return s.dow[int(t.Weekday())]
	case s.dowStar:
		return s.dom[t.Day()]
	default:
		return s.dom[t.Day()] || s.dow[int(t.Weekday())]
	}
}

// String keeps the original expression for logs and the status page.
func (s *Schedule) String() string { return s.expr }
