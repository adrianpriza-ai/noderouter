package proxy

// Schedule.Next is what decides when the proxy list is refetched, so its two
// hard edges matter: a calendar schedule crossing a DST boundary (where the
// wall clock jumps and "00:00" is not always 24 hours away), and the iteration
// limit that keeps an impossible schedule from spinning forever.
//
// time/tzdata is imported so the DST cases below do not depend on the host
// having a zoneinfo database installed.

import (
	"testing"
	"time"

	_ "time/tzdata"
)

func mustParse(t *testing.T, expr string) *Schedule {
	t.Helper()
	s, err := ParseSchedule(expr)
	if err != nil {
		t.Fatalf("ParseSchedule(%q): %v", expr, err)
	}
	return s
}

// A daily midnight schedule must fire once per calendar day, and "once per day"
// has to mean one firing even on the 23- and 25-hour days DST creates.
func TestNextFiresOncePerDayAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no zoneinfo: %v", err)
	}
	s := mustParse(t, "0 0 * * *")

	// Spring forward: 2024-03-10, 02:00 jumps to 03:00, so that day is 23h.
	// Fall back: 2024-11-03, 02:00 repeats, so that day is 25h.
	for _, day := range []string{"2024-03-09", "2024-03-10", "2024-03-11"} {
		from, err := time.ParseInLocation("2006-01-02 15:04", day+" 12:00", ny)
		if err != nil {
			t.Fatal(err)
		}
		next := s.Next(from)
		if next.Hour() != 0 || next.Minute() != 0 {
			t.Errorf("Next after %s = %s, want local midnight", day, next)
		}
		// Whatever the offset did, the next firing is the following day.
		wantDay := from.AddDate(0, 0, 1)
		if next.Day() != wantDay.Day() {
			t.Errorf("Next after %s landed on day %d, want %d", day, next.Day(), wantDay.Day())
		}
	}

	// Walking day by day across both transitions must stay on midnight and
	// never stall or repeat a day.
	cur := time.Date(2024, 3, 8, 12, 0, 0, 0, ny)
	for i := 0; i < 8; i++ {
		next := s.Next(cur)
		if next.Hour() != 0 || next.Minute() != 0 {
			t.Fatalf("iteration %d: Next(%s) = %s, want local midnight", i, cur, next)
		}
		if !next.After(cur) {
			t.Fatalf("iteration %d: Next(%s) = %s did not move forward", i, cur, next)
		}
		// Land mid-morning the next day so the next call is a fresh question.
		cur = next.Add(10 * time.Hour)
		if h := cur.Hour(); h != 10 && h != 11 {
			// 10:00 local may be 09:00/11:00 across a shift; only flag a real jump.
			if h < 9 || h > 11 {
				t.Fatalf("iteration %d: drifted to %s", i, cur)
			}
		}
	}
}

// The wall-clock schedule must keep meaning wall-clock time: a daily slot fires
// at that local time, whatever the UTC offset did the day before.
func TestNextTracksWallClockNotElapsedTime(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no zoneinfo: %v", err)
	}
	// 01:30 exists on every New York day, including both DST transition days.
	s := mustParse(t, "30 1 * * *")

	for _, day := range []string{"2024-03-09", "2024-03-10", "2024-11-02", "2024-11-03"} {
		from, err := time.ParseInLocation("2006-01-02 15:04", day+" 12:00", ny)
		if err != nil {
			t.Fatal(err)
		}
		next := s.Next(from)
		if next.Hour() != 1 || next.Minute() != 30 {
			t.Errorf("Next after %s %s = %s, want 01:30 local", day, from.Location(), next)
		}
		if next.Day() != from.Day()+1 {
			t.Errorf("Next after midday %s = %s, want the following day", from.Format("2006-01-02"), next.Format("2006-01-02"))
		}
	}
}

// Known limitation, pinned so it cannot regress silently. A slot that falls
// inside a spring-forward gap never happens: New York jumps 02:00 -> 03:00, so
// "30 2" is asked for on a day that has no 02:30. time.Date normalises the
// request to a real instant at a different wall-clock time, the fields never
// match, and Next burns its whole iteration budget and reports the zero time.
//
// That is why the scheduler treats a zero time as "stop scheduling" - and why
// a zone that shifts at midnight (America/Santiago, Asia/Beirut, ...) would
// silently stop refreshing proxies on the default "0 0 * * *" schedule. Fixing
// it means treating the requested slot as "the instant nearest this wall-clock
// time" rather than requiring an exact hour and minute match.
func TestSlotInsideSpringForwardGapNeverFires(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no zoneinfo: %v", err)
	}
	s := mustParse(t, "30 2 * * *")

	from := time.Date(2024, 3, 9, 3, 0, 0, 0, ny)
	if got := s.Next(from); !got.IsZero() {
		t.Errorf("Next = %s; once the gap is handled this should become the nearest real instant to 02:30", got)
	}
}

// @every is a fixed duration, so it is immune to DST by construction. That
// contrast is the reason both forms exist and is worth pinning.
func TestEveryIsDSTImmune(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no zoneinfo: %v", err)
	}
	s := mustParse(t, "@every 90m")

	// 2024-03-10 01:30 local, an hour before the clocks jump.
	from := time.Date(2024, 3, 10, 1, 30, 0, 0, ny)
	got := s.Next(from)
	want := from.Add(90 * time.Minute)
	if !got.Equal(want) {
		t.Errorf("Next(%s) = %s, want exactly 90m later (%s)", from, got, want)
	}
}

// A schedule that can never fire must give up rather than spin: Next returns
// the zero time, which is the caller's signal to stop scheduling. February 30th
// is the classic impossible one.
func TestNextGivesUpOnImpossibleSchedule(t *testing.T) {
	s := mustParse(t, "0 0 30 2 *")

	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	got := s.Next(from)
	if !got.IsZero() {
		t.Errorf("Next on an impossible schedule = %s, want the zero time", got)
	}
}

// The limit is a guard against a pathological loop, so it has to be reached
// without taking pathologically long, and it must not trip on a schedule that
// can fire.
func TestLimitDoesNotTripOnSparseButValidSchedule(t *testing.T) {
	// Once a year: sparse enough to exercise the day/month skipping.
	s := mustParse(t, "0 0 1 1 *")
	from := time.Date(2024, 2, 2, 0, 0, 0, 0, time.UTC)
	got := s.Next(from)
	want := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("Next(%s) = %s, want %s", from, got, want)
	}
}

// Next must always return something strictly after the time it was handed, or
// the scheduler's timer would fire immediately and spin.
func TestNextIsAlwaysStrictlyForward(t *testing.T) {
	exprs := []string{
		"* * * * *", "0 0 * * *", "0 0 1 1 *", "*/5 * * * *",
		"0 9-17 * * 1-5", "@every 1m", "@hourly", "@daily", "@weekly", "@monthly",
	}
	starts := []time.Time{
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 2, 29, 23, 59, 59, 0, time.UTC),
		time.Date(2024, 12, 31, 23, 59, 0, 0, time.UTC),
		time.Date(2024, 6, 15, 13, 37, 42, 0, time.UTC),
	}
	for _, expr := range exprs {
		s, err := ParseSchedule(expr)
		if err != nil {
			t.Errorf("ParseSchedule(%q): %v", expr, err)
			continue
		}
		for _, from := range starts {
			got := s.Next(from)
			if got.IsZero() {
				t.Errorf("%q: Next(%s) returned the zero time for a schedule that can fire", expr, from)
				continue
			}
			if !got.After(from) {
				t.Errorf("%q: Next(%s) = %s, want a later time", expr, from, got)
			}
		}
	}
}

// A schedule given a time that matches exactly must move to the *next*
// occurrence, not hand back the same instant - otherwise the scheduler would
// fire twice for one slot.
func TestNextFromExactMatchAdvances(t *testing.T) {
	s := mustParse(t, "0 * * * *")
	from := time.Date(2024, 5, 5, 10, 0, 0, 0, time.UTC)
	got := s.Next(from)
	if want := time.Date(2024, 5, 5, 11, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("Next(%s) = %s, want %s", from, got, want)
	}
}

// Seconds are not part of a cron field, so a firing inside the same minute is
// already past and the answer is the next one.
func TestNextTruncatesToTheMinute(t *testing.T) {
	s := mustParse(t, "* * * * *")
	from := time.Date(2024, 5, 5, 10, 0, 30, 500, time.UTC)
	got := s.Next(from)
	if want := time.Date(2024, 5, 5, 10, 1, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("Next(%s) = %s, want %s", from, got, want)
	}
}

// Day-of-month and day-of-week are OR-ed when both are restricted, which is
// standard cron and is the behaviour most surprising when it is wrong.
func TestDayOfMonthAndDayOfWeekAreOred(t *testing.T) {
	s := mustParse(t, "0 0 13 * 5") // the 13th, or any Friday
	// 2024-09-01 is a Sunday; walk two months and collect every hit. Keep the
	// full instant, not just the day: firing times cross into October.
	cur := time.Date(2024, 9, 1, 0, 0, 0, 0, time.UTC)
	var fired []time.Time
	for range 20 {
		next := s.Next(cur)
		if next.IsZero() {
			t.Fatal("schedule reported no firing time")
		}
		fired = append(fired, next)
		cur = next
	}

	var saw13th, sawFriday bool
	for _, when := range fired {
		if when.Day() != 13 && when.Weekday() != time.Friday {
			t.Errorf("fired on %s, want the 13th or a Friday", when.Format("2006-01-02 (Mon)"))
		}
		if when.Day() == 13 {
			saw13th = true
		}
		if when.Weekday() == time.Friday {
			sawFriday = true
		}
	}
	// Both kinds have to actually appear, or the OR is not happening.
	if !saw13th || !sawFriday {
		t.Errorf("expected both a 13th and a Friday among %v, got 13th=%v friday=%v",
			fired, saw13th, sawFriday)
	}
}

// The "7 means Sunday" spelling is not in POSIX but is what people write, and
// getting it wrong makes the router refuse to start at all.
func TestDayOfWeekSevenIsSunday(t *testing.T) {
	seven := mustParse(t, "0 0 * * 7")
	zero := mustParse(t, "0 0 * * 0")

	from := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	if a, b := seven.Next(from), zero.Next(from); !a.Equal(b) {
		t.Errorf("dow 7 gave %s but dow 0 gave %s; they are the same day", a, b)
	}
	if got := seven.Next(from); got.Weekday() != time.Sunday {
		t.Errorf("dow 7 fired on %s, want a Sunday", got.Weekday())
	}
}

// String is what the startup banner and the status page print, so it has to
// echo the expression rather than a rewritten form. Surrounding whitespace is
// trimmed on the way in - that is not a semantic change worth reporting back.
func TestStringEchoesTheExpression(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"0 0 * * *", "0 0 * * *"},
		{"@daily", "@daily"},
		{"*/10 * * * *", "*/10 * * * *"},
		{"  @every 30m  ", "@every 30m"},
		{"0 0 * * 7", "0 0 * * 7"},
	} {
		s := mustParse(t, tc.in)
		if got := s.String(); got != tc.want {
			t.Errorf("ParseSchedule(%q).String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}
