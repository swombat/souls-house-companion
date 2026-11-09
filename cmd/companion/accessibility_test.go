package main

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/swombat/souls-house-companion/internal/axtrust"
)

func TestWatchAccessibilityPromptsOnlyForOwnerRequest(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	prompts, checks := 0, 0
	check := func(prompt bool) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		checks++
		if prompt {
			prompts++
		}
		return false, nil
	}
	if err := axtrust.Request(dir); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { watchAccessibility(ctx, dir, check); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := checks
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no observation at start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if prompts != 1 {
		t.Fatalf("prompts = %d", prompts)
	}
	obs, err := axtrust.Read(dir)
	if err != nil || !obs.Requested || obs.Trusted {
		t.Fatalf("observation %+v err %v", obs, err)
	}
}

func TestAccessibilityStatusBeforeAnyCheck(t *testing.T) {
	if got := accessibilityStatus(t.TempDir()); got != "not_checked" {
		t.Fatalf("status = %v", got)
	}
}

func TestProbeChildParsesBoundedAnswer(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := dir + "/" + name
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if trusted, err := probeChild(write("yes", `echo '{"trusted":true}'`)); err != nil || !trusted {
		t.Fatalf("trusted=%v err=%v", trusted, err)
	}
	if _, err := probeChild(write("extra", `echo '{"trusted":true,"x":1}'`)); err == nil {
		t.Fatal("accepted unknown field")
	}
	if _, err := probeChild(write("big", `head -c 4096 /dev/zero`)); err == nil {
		t.Fatal("accepted oversized output")
	}
	if _, err := probeChild(write("fail", `exit 2`)); err == nil {
		t.Fatal("accepted failed probe")
	}
}
