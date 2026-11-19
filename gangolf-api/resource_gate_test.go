package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestResourceMutexZeroValueAndTryLock(t *testing.T) {
	var m resourceMutex
	if !m.TryLock() {
		t.Fatal("zero value unavailable")
	}
	if m.TryLock() {
		t.Fatal("TryLock acquired held lock")
	}
	m.Unlock()
	if err := lockMutex(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	if m.TryLock() {
		t.Fatal("context acquisition did not exclude TryLock")
	}
	done := make(chan struct{})
	go func() { m.Unlock(); close(done) }()
	<-done
	m.Lock()
	m.Unlock()
}

func TestResourceMutexTryLockDoesNotWaitForRegistration(t *testing.T) {
	var m resourceMutex
	m.waitMu.Lock()
	defer m.waitMu.Unlock()
	result := make(chan bool, 1)
	go func() { result <- m.TryLock() }()
	select {
	case ok := <-result:
		if !ok {
			t.Fatal("free native mutex unavailable")
		}
		m.mu.Unlock()
	case <-time.After(time.Second):
		t.Fatal("TryLock waited for registration")
	}
}

func TestResourceMutexReleaseWakesWithoutClockAdvance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var m resourceMutex
		m.Lock()
		result := make(chan error, 1)
		go func() { result <- lockMutex(context.Background(), &m) }()
		synctest.Wait()
		before := time.Now()
		m.Unlock()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		m.Unlock()
		if elapsed := time.Since(before); elapsed != 0 {
			t.Fatalf("release required clock advancement: %s", elapsed)
		}
	})
}

func TestResourceMutexCancellationDoesNotConsumeOwnership(t *testing.T) {
	for _, held := range []bool{false, true} {
		var m resourceMutex
		if held {
			m.Lock()
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := lockMutex(ctx, &m); !errors.Is(err, context.Canceled) {
			t.Fatalf("held=%v: %v", held, err)
		}
		if held {
			if m.TryLock() {
				t.Fatal("cancel released owner's lock")
			}
			m.Unlock()
		}
		if !m.TryLock() {
			t.Fatal("canceled acquisition consumed ownership")
		}
		m.Unlock()
	}
}

func TestResourceMutexCanceledWaiterDoesNotStrandSurvivor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var m resourceMutex
		m.Lock()
		ctx, cancel := context.WithCancel(context.Background())
		canceled, survivor := make(chan error, 1), make(chan error, 1)
		go func() { canceled <- lockMutex(ctx, &m) }()
		go func() { survivor <- lockMutex(context.Background(), &m) }()
		synctest.Wait()
		before := time.Now()
		cancel()
		if err := <-canceled; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if m.TryLock() {
			t.Fatal("cancel released owner's lock")
		}
		m.Unlock()
		if err := <-survivor; err != nil {
			t.Fatal(err)
		}
		m.Unlock()
		if time.Since(before) != 0 {
			t.Fatal("cancel/release needed timer")
		}
	})
}

func TestResourceMutexCancelReleaseRacePreservesOwnership(t *testing.T) {
	for i := 0; i < 500; i++ {
		var m resourceMutex
		m.Lock()
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- lockMutex(ctx, &m) }()
		start := make(chan struct{})
		var released sync.WaitGroup
		released.Add(2)
		go func() { defer released.Done(); <-start; cancel() }()
		go func() { defer released.Done(); <-start; m.Unlock() }()
		close(start)
		err := <-result
		released.Wait()
		if err == nil {
			if m.TryLock() {
				t.Fatal("successful acquisition has no ownership")
			}
			m.Unlock()
		} else if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if !m.TryLock() {
			t.Fatal("cancel/release lost availability")
		}
		m.Unlock()
	}
}

func TestResourceMutexMixedOwners(t *testing.T) {
	var m resourceMutex
	var wg sync.WaitGroup
	count := 0
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(native bool) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if native {
					m.Lock()
				} else if err := lockMutex(context.Background(), &m); err != nil {
					t.Error(err)
					return
				}
				count++
				m.Unlock()
			}
		}(worker%2 == 0)
	}
	wg.Wait()
	if count != 800 {
		t.Fatalf("mixed owners lost updates: %d", count)
	}
	if !m.TryLock() {
		t.Fatal("mixed owners stranded lock")
	}
	m.Unlock()
}

func TestResourceMutexInvalidUnlockRetainsNativeFatal(t *testing.T) {
	if os.Getenv("GANGOLF_TEST_INVALID_UNLOCK") == "1" {
		defer func() { _ = recover() }()
		var m resourceMutex
		m.Unlock()
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestResourceMutexInvalidUnlockRetainsNativeFatal$")
	command.Env = append(os.Environ(), "GANGOLF_TEST_INVALID_UNLOCK=1")
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "fatal error: sync: unlock of unlocked mutex") {
		t.Fatalf("invalid Unlock lost native fatal: err=%v output=%s", err, output)
	}
}
