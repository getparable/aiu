package core

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestRefreshPersistsAfterFrontendCancelAndHandBackFailure(t *testing.T) {
	c := testConfig(t, newFakeAPI(t))
	c.UseDPAPI = runtime.GOOS == "windows"
	writeClaudeCreds(t, c, "old-access", "old-refresh", 1)
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		io.WriteString(w, `{"access_token":"replacement-access","refresh_token":"replacement-refresh","expires_in":3600}`)
	}))
	defer server.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	c.ClaudeTokenURLs = []string{server.URL}
	c.HTTP = server.Client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	r := &Record{Provider: Claude, Email: "fixture@example.test", AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: 1}
	go func() { _, err := c.ensureFresh(ctx, r, nil, nil, false); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh never started")
	}
	// A concurrent external write makes hand-back fail after the provider has
	// rotated. Cancellation must still let AIU preserve its issued replacement.
	if err := os.WriteFile(filepath.Join(c.ClaudeDir, ".credentials.json"), []byte("corrupt external document"), 0o600); err != nil {
		t.Fatal(err)
	}
	cancel()
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not settle")
	}
	stored, err := c.tokenGet(r.StoreKey())
	if err != nil || stored == nil || stored.RefreshToken != "replacement-refresh" {
		t.Fatal("issued replacement was not recoverable after cancel and hand-back failure")
	}
}

func TestCancelledUsageDoesNotWaitForMachineStagger(t *testing.T) {
	c := testConfig(t, newFakeAPI(t))
	c.writeCache(cache{machineKey: &cacheEntry{AttemptedAt: time.Now().UnixMilli()}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	_, err := c.fetchUsage(ctx, &Record{Provider: Claude, Email: "fixture@example.test"}, nil, nil)
	if err != context.Canceled || time.Since(started) > time.Second {
		t.Fatalf("cancelled usage remained in cache wait: %v", err)
	}
}
