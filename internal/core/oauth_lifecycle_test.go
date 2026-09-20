package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLoginCancellationClosesCallbackListener(t *testing.T) {
	cb, err := listenCallback(0, "synthetic-state", callbackPath, Claude)
	if err != nil {
		t.Fatal(err)
	}
	s := &LoginSession{callback: cb}
	defer s.Cancel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.WaitForCode(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait: %v", err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cb.port))
	if err != nil {
		t.Fatalf("callback port still held after cancellation: %v", err)
	}
	ln.Close()
}

func TestCancelBeforeCallbackServeStarts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cb := &callbackServer{listener: ln, server: &http.Server{}, result: make(chan callbackResult, 1)}
	cb.close(context.Canceled)
	probe, err := net.Listen("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("cancellation left the unserved listener open: %v", err)
	}
	probe.Close()
}

func TestBrowserDenialEndsLoginWait(t *testing.T) {
	cb, err := listenCallback(0, "synthetic-state", callbackPath, Claude)
	if err != nil {
		t.Fatal(err)
	}
	s := &LoginSession{callback: cb}
	defer s.Cancel()
	res, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/callback?state=synthetic-state&error=access_denied", cb.port))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.WaitForCode(ctx); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("denial did not end login: %v", err)
	}
}

func TestLoginFinishesSavingAfterCancellationDuringExchange(t *testing.T) {
	api := newFakeAPI(t)
	c := testConfig(t, api)
	c.UseDPAPI = runtime.GOOS == "windows"
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if r.Method != "POST" || body["grant_type"] != "authorization_code" || body["code"] != "synthetic-code" || body["code_verifier"] != "synthetic-verifier" {
				t.Error("wrong token exchange protocol")
			}
			close(started)
			<-release
			io.WriteString(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600}`)
		case "/profile":
			if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer synthetic-access" {
				t.Error("wrong profile authorization")
			}
			io.WriteString(w, `{"account":{"email":"fixture@example.test"},"organization":{"uuid":"fixture-org"}}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	c.ClaudeTokenURLs, c.ClaudeProfileURL = []string{server.URL + "/token"}, server.URL+"/profile"
	c.HTTP = server.Client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.CompleteLogin(ctx, &LoginSession{Provider: Claude, verifier: "synthetic-verifier"}, "synthetic-code", "fixture")
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("exchange did not start")
	}
	cancel()
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancellation discarded the successful exchange: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("login did not finish")
	}
	r, err := c.tokenGet("claude:fixture@example.test#fixture-org")
	if err != nil || r == nil || r.RefreshToken != "synthetic-refresh" {
		t.Fatal("issued credentials were not recoverable")
	}
}

func TestCanceledLoginDoesNotBeginExchange(t *testing.T) {
	c := testConfig(t, newFakeAPI(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.CompleteLogin(ctx, &LoginSession{Provider: Codex}, "synthetic-code", "fixture"); !errors.Is(err, context.Canceled) {
		t.Fatalf("already canceled exchange: %v", err)
	}
}
