package creds

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestProfileTokenConcurrentTransactions(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	const workers = 48
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if _, err := (Store{Dir: s.Dir}).EnsureProfileToken(fmt.Sprintf("profile-%02d", i)); err != nil {
				t.Errorf("ensure profile-%d: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	got, err := s.ProfileTokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != workers {
		t.Fatalf("got %d tokens, want %d", len(got), workers)
	}
	for i := range workers {
		if got[fmt.Sprintf("profile-%02d", i)] == "" {
			t.Errorf("profile-%02d was lost", i)
		}
	}

	start = make(chan struct{})
	results := make(chan string, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tok, err := (Store{Dir: s.Dir}).EnsureProfileToken("shared")
			if err != nil {
				t.Errorf("ensure shared: %v", err)
				return
			}
			results <- tok
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	shared := ""
	for tok := range results {
		if shared == "" {
			shared = tok
		} else if tok != shared {
			t.Error("same-name token differed across concurrent EnsureProfileToken calls")
		}
	}
	got, err = s.ProfileTokens()
	if err != nil || got["shared"] != shared {
		t.Fatalf("stored shared token does not match concurrent EnsureProfileToken results (err %v)", err)
	}
}

func TestProfileTokenConcurrentCreateAndDelete(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if _, err := s.EnsureProfileToken("remove"); err != nil {
		t.Fatal(err)
	}
	const creates = 40
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		if err := (Store{Dir: s.Dir}).DeleteProfileToken("remove"); err != nil {
			t.Errorf("delete token: %v", err)
		}
	}()
	for i := range creates {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if _, err := (Store{Dir: s.Dir}).EnsureProfileToken(fmt.Sprintf("created-%02d", i)); err != nil {
				t.Errorf("ensure created-%d: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	got, err := s.ProfileTokens()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := got["remove"]; exists {
		t.Error("deleted token was restored by a concurrent writer")
	}
	for i := range creates {
		if got[fmt.Sprintf("created-%02d", i)] == "" {
			t.Errorf("concurrent token created-%02d was lost", i)
		}
	}
}

func TestLockFileChildProcess(t *testing.T) {
	mode := os.Getenv("CREDS_LOCK_CHILD_MODE")
	if mode == "" {
		return
	}
	path := os.Getenv("CREDS_LOCK_CHILD_PATH")
	if mode == "wait" {
		if err := os.WriteFile(path+".ready", nil, 0o600); err != nil {
			t.Fatal(err)
		}
		unlock, err := LockFile(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".acquired", nil, 0o600); err != nil {
			unlock()
			t.Fatal(err)
		}
		unlock()
		return
	}
	if mode == "exit" {
		if _, err := LockFile(context.Background(), path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".acquired", nil, 0o600); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}
	t.Fatalf("unknown child mode %q", mode)
}

func TestLockFileInterprocessContentionAndExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens")
	unlock, err := LockFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	cmdCtx, cmdCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cmdCancel()
	cmd := exec.CommandContext(cmdCtx, os.Args[0], "-test.run=^TestLockFileChildProcess$")
	cmd.Env = append(os.Environ(), "CREDS_LOCK_CHILD_MODE=wait", "CREDS_LOCK_CHILD_PATH="+path)
	if err := cmd.Start(); err != nil {
		unlock()
		t.Fatal(err)
	}
	ready := path + ".ready"
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			unlock()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("child did not start lock attempt")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if release, err := LockFile(ctx, path); err != context.DeadlineExceeded {
		if release != nil {
			release()
		}
		unlock()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("contended LockFile = %v, want context deadline", err)
	}
	if _, err := os.Stat(path + ".acquired"); err == nil {
		unlock()
		_ = cmd.Wait()
		t.Fatal("child acquired lock while parent held it")
	}
	unlock()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(path + ".acquired"); err != nil && !os.IsNotExist(err) {
		unlock()
		t.Fatal(err)
	}
	cmdCtx2, cmdCancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cmdCancel2()
	cmd = exec.CommandContext(cmdCtx2, os.Args[0], "-test.run=^TestLockFileChildProcess$")
	cmd.Env = append(os.Environ(), "CREDS_LOCK_CHILD_MODE=exit", "CREDS_LOCK_CHILD_PATH="+path)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".acquired"); err != nil {
		t.Fatal("child did not acquire the lock before exit")
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	unlock, err = LockFile(ctx2, path)
	if err != nil {
		t.Fatal("lock remained held after child exit")
	}
	unlock()
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatalf("sidecar not persistent: %v", err)
	}
}
