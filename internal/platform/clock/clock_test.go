package clock

import (
	"testing"
	"time"
)

func TestSystemNow(t *testing.T) {
	t.Parallel()
	before := time.Now().UTC()
	got := System{}.Now()
	after := time.Now().UTC()
	if got.Location() != time.UTC {
		t.Fatalf("System.Now location = %v, want UTC", got.Location())
	}
	if got.Before(before) || got.After(after) {
		t.Fatalf("System.Now = %v, outside [%v, %v]", got, before, after)
	}
}

func TestFakeNowAdvanceAndSet(t *testing.T) {
	t.Parallel()
	zone := time.FixedZone("UTC+2", 2*60*60)
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, zone)
	fake := NewFake(start)

	if !fake.Now().Equal(start) {
		t.Fatalf("NewFake Now = %v, want instant %v", fake.Now(), start)
	}
	if fake.Now().Location() != time.UTC {
		t.Fatalf("NewFake Now location = %v, want UTC", fake.Now().Location())
	}

	fake.Advance(90 * time.Minute)
	want := start.Add(90 * time.Minute)
	if !fake.Now().Equal(want) {
		t.Fatalf("after Advance Now = %v, want %v", fake.Now(), want)
	}

	fake.Advance(-2 * time.Hour)
	want = want.Add(-2 * time.Hour)
	if !fake.Now().Equal(want) {
		t.Fatalf("after negative Advance Now = %v, want %v", fake.Now(), want)
	}

	set := time.Date(2030, 6, 7, 8, 9, 10, 11, time.FixedZone("UTC-7", -7*60*60))
	fake.Set(set)
	if !fake.Now().Equal(set) {
		t.Fatalf("after Set Now = %v, want instant %v", fake.Now(), set)
	}
	if fake.Now().Location() != time.UTC {
		t.Fatalf("after Set Now location = %v, want UTC", fake.Now().Location())
	}
}

func TestFakeDoesNotAliasCallerTime(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	fake := NewFake(start)
	first := fake.Now()
	fake.Advance(time.Hour)
	if first.Equal(fake.Now()) {
		t.Fatal("Now returned a value that changed with the fake clock")
	}
	if !first.Equal(start) {
		t.Fatalf("previously returned Now changed: %v, want %v", first, start)
	}
}
