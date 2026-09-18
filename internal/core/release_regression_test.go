package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestConcurrentTokenWrites(t *testing.T) {
	c := testConfig(t, newFakeAPI(t))
	const count = 30
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			key := strconv.Itoa(i)
			if err := c.tokenSet(key, &Record{Email: key}); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	for i := range count {
		key := strconv.Itoa(i)
		if r, err := c.tokenGet(key); err != nil || r == nil {
			t.Fatalf("lost token %s: record=%v error=%v", key, r, err)
		}
	}
}

func TestConcurrentProcessTokenWrites(t *testing.T) {
	if dir := os.Getenv("AIU_TEST_STORE_CHILD"); dir != "" {
		c := &Config{Dir: dir}
		prefix := os.Getenv("AIU_TEST_STORE_PREFIX")
		for i := range 10 {
			key := fmt.Sprintf("%s-%d", prefix, i)
			if err := c.tokenSet(key, &Record{Email: key}); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	dir := t.TempDir()
	const processes = 6
	var wg sync.WaitGroup
	for i := range processes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestConcurrentProcessTokenWrites$")
			cmd.Env = append(os.Environ(), "AIU_TEST_STORE_CHILD="+dir, "AIU_TEST_STORE_PREFIX="+strconv.Itoa(i))
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("child %d: %v: %s", i, err, output)
			}
		}()
	}
	wg.Wait()
	c := &Config{Dir: dir}
	for i := range processes {
		for j := range 10 {
			key := fmt.Sprintf("%d-%d", i, j)
			if r, err := c.tokenGet(key); err != nil || r == nil {
				t.Fatalf("lost token %s: record=%v error=%v", key, r, err)
			}
		}
	}
}

func TestRefreshDoesNotOverwriteNewLogin(t *testing.T) {
	api := newFakeAPI(t)
	c := testConfig(t, api)
	writeClaudeCreds(t, c, "old-access", "old-refresh", c.now().UnixMilli()-1)
	snapshot := c.ReadClaudeCode()
	writeClaudeCreds(t, c, "new-access", "new-refresh", c.now().Add(time.Hour).UnixMilli())
	api.refresh["old-refresh"] = "rotated-old-access"
	_, err := c.ensureFresh(context.Background(), &Record{Provider: Claude, Email: "old@example.com", AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: 1}, snapshot, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ReadClaudeCode().accessToken(); got != "new-access" {
		t.Fatalf("newer login was overwritten: %s", got)
	}
}

func TestRefreshHandsBackWhenClaudeBecomesActive(t *testing.T) {
	api := newFakeAPI(t)
	c := testConfig(t, api)
	api.refresh["spent"] = "rotated"
	writeClaudeCreds(t, c, "old", "other", c.now().Add(time.Hour).UnixMilli())
	snapshot := c.ReadClaudeCode()
	writeClaudeCreds(t, c, "active", "spent", c.now().UnixMilli()-1)
	_, err := c.ensureFresh(context.Background(), &Record{Provider: Claude, Email: "a@example.com", AccessToken: "active", RefreshToken: "spent", ExpiresAt: 1}, snapshot, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ReadClaudeCode().accessToken(); got != "rotated" {
		t.Fatalf("active Claude login did not receive rotated token: %s", got)
	}
}

func TestRefreshHandsBackWhenCodexBecomesActive(t *testing.T) {
	api := newFakeAPI(t)
	c := testConfig(t, api)
	api.refresh["spent"] = "rotated"
	id := fakeJWT(map[string]any{"email": "a@example.com"})
	doc := map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{"access_token": "active", "refresh_token": "spent", "id_token": id}}
	data, _ := json.Marshal(doc)
	if err := os.MkdirAll(c.CodexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.codexAuthFile(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := c.ensureFresh(context.Background(), &Record{Provider: Codex, Email: "a@example.com", AccessToken: "active", RefreshToken: "spent", IDToken: id, ExpiresAt: 1}, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ReadCodexAuth().accessToken(); got != "rotated" {
		t.Fatalf("active Codex login did not receive rotated token: %s", got)
	}
}

func TestCapturePreservesRotationOnProfileFailure(t *testing.T) {
	api := newFakeAPI(t)
	c := testConfig(t, api)
	writeClaudeCreds(t, c, "old-access", "old-refresh", c.now().UnixMilli()-1)
	api.refresh["old-refresh"] = "rotated-access"
	if _, err := c.CaptureClaudeCode(context.Background(), ""); err == nil {
		t.Fatal("expected profile error")
	}
	if got := c.ReadClaudeCode().accessToken(); got != "rotated-access" {
		t.Fatalf("rotated token was lost: %s", got)
	}
	if got := c.ReadClaudeCode().refreshToken(); got != "rt-rotated-access" {
		t.Fatalf("rotated refresh token was lost: %s", got)
	}
}

func TestRemoveKeepsIndexWhenTokenDeletionFails(t *testing.T) {
	c := testConfig(t, newFakeAPI(t))
	idx := &Index{Version: 1, Accounts: []*IndexEntry{{Email: "a@example.com", Provider: Claude, Label: "a"}}}
	if err := c.saveIndex(idx); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(c.fileStore()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.fileStore(), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RemoveAccount("a", Claude); err == nil {
		t.Fatal("expected deletion error")
	}
	loaded, err := c.LoadIndex()
	if err != nil || len(loaded.Accounts) != 1 {
		t.Fatalf("deletion failure removed index entry: index=%v error=%v", loaded, err)
	}
}
