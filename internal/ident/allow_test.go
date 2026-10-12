package ident

import (
	"testing"
	"time"
)

func TestAllowListAwaitLateAdd(t *testing.T) {
	a := NewAllowList(time.Minute)
	go func() {
		time.Sleep(150 * time.Millisecond)
		a.Add("ABCD")
	}()
	start := time.Now()
	if !a.Await("abcd") {
		t.Fatal("fingerprint added during the grace period was rejected")
	}
	if time.Since(start) > IntroductionGrace {
		t.Fatal("Await returned later than the grace period")
	}
	if !a.Allowed("abcd") {
		t.Fatal("Allowed disagrees with Await")
	}
}

func TestAllowListAwaitGivesUp(t *testing.T) {
	old := IntroductionGrace
	IntroductionGrace = 100 * time.Millisecond
	defer func() { IntroductionGrace = old }()
	a := NewAllowList(time.Minute)
	a.Add("other")
	start := time.Now()
	if a.Await("abcd") {
		t.Fatal("unknown fingerprint was admitted")
	}
	if time.Since(start) < IntroductionGrace {
		t.Fatal("Await gave up before the grace period")
	}
}

func TestAllowListExpiry(t *testing.T) {
	a := NewAllowList(50 * time.Millisecond)
	a.Add("abcd")
	if !a.Allowed("abcd") {
		t.Fatal("fresh entry not allowed")
	}
	time.Sleep(80 * time.Millisecond)
	if a.Allowed("abcd") {
		t.Fatal("expired entry still allowed")
	}
}
