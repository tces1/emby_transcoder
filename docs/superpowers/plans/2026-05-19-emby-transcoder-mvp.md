# EmbyTranscoder MVP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a Go native Emby/Jellyfin playback proxy that can rewrite PlaybackInfo for selected clients and serve local FFmpeg HLS sessions.

**Architecture:** Use the Go standard library for HTTP proxying, JSON rewriting, process management, and tests. Split the implementation into config loading, client policy matching, PlaybackInfo rewriting, reverse proxy routing, and FFmpeg transcode session management.

**Tech Stack:** Go 1.26, `net/http`, `httputil.ReverseProxy`, `encoding/json`, `os/exec`, table-driven Go tests.

---

### Task 1: Project Skeleton

**Files:**
- Create: `go.mod`
- Create: `cmd/emby-transcoder/main.go`
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write failing config tests**

```go
func TestDefaultConfigIsUsable(t *testing.T) {
	cfg := config.Default()
	if cfg.Server.Listen != ":8097" {
		t.Fatalf("listen = %q", cfg.Server.Listen)
	}
	if cfg.Transcode.FFmpegPath == "" {
		t.Fatal("ffmpeg path should default")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config`
Expected: fail because the package does not exist yet.

- [ ] **Step 3: Implement config defaults and JSON loading**

Create `internal/config/config.go` with `Config`, `Server`, `Upstream`, `Transcode`, `ClientProfile`, `Default()`, and `Load(path string)`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config`
Expected: pass.

### Task 2: Client Policy Matching

**Files:**
- Create: `internal/policy/policy.go`
- Test: `internal/policy/policy_test.go`

- [ ] **Step 1: Write failing client matching tests**

```go
func TestShouldTranscodeMatchesUserAgent(t *testing.T) {
	headers := http.Header{"User-Agent": {"Yamby TV"}}
	result := policy.ShouldTranscode(headers, []config.ClientProfile{{Name: "yamby", Match: []string{"Yamby"}, Transcode: true}})
	if !result.Enabled || result.ProfileName != "yamby" {
		t.Fatalf("unexpected result: %+v", result)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/policy`
Expected: fail because the package does not exist yet.

- [ ] **Step 3: Implement case-insensitive matching over `User-Agent` and `X-Emby-Authorization`**

Create `ShouldTranscode(headers http.Header, profiles []config.ClientProfile) Result`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/policy`
Expected: pass.

### Task 3: PlaybackInfo Rewriter

**Files:**
- Create: `internal/emby/playback.go`
- Test: `internal/emby/playback_test.go`

- [ ] **Step 1: Write failing rewrite tests**

```go
func TestRewritePlaybackInfoInjectsTranscodingURL(t *testing.T) {
	input := []byte(`{"MediaSources":[{"Id":"source1","SupportsDirectPlay":true}]}`)
	out, changed, err := emby.RewritePlaybackInfo(input, "item123", "http://proxy.local")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if !bytes.Contains(out, []byte(`/streambridge/transcode/`)) {
		t.Fatalf("missing local transcode url: %s", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/emby`
Expected: fail because the package does not exist yet.

- [ ] **Step 3: Implement JSON rewriting**

Parse JSON into `map[string]any`, rewrite `MediaSources`, and return unchanged JSON when `MediaSources` is absent.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/emby`
Expected: pass.

### Task 4: Transcode Session Manager

**Files:**
- Create: `internal/transcode/manager.go`
- Create: `internal/transcode/handler.go`
- Test: `internal/transcode/manager_test.go`

- [ ] **Step 1: Write failing manager tests**

```go
func TestManagerRejectsWhenSessionLimitReached(t *testing.T) {
	m := transcode.NewManager(transcode.Options{MaxSessions: 1, TempDir: t.TempDir()})
	_, err := m.Ensure("one", transcode.Request{InputURL: "http://upstream/one"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Ensure("two", transcode.Request{InputURL: "http://upstream/two"})
	if !errors.Is(err, transcode.ErrTooManySessions) {
		t.Fatalf("err = %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/transcode`
Expected: fail because the package does not exist yet.

- [ ] **Step 3: Implement session tracking and file serving**

Implement `Manager`, `Session`, `Ensure`, `Touch`, `Stop`, `Close`, and a `Handler` that serves session files from the temp directory.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/transcode`
Expected: pass.

### Task 5: Proxy Server

**Files:**
- Create: `internal/proxy/server.go`
- Test: `internal/proxy/server_test.go`

- [ ] **Step 1: Write failing proxy tests**

```go
func TestPlaybackInfoIsRewrittenForMatchedClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"MediaSources":[{"Id":"source1","SupportsDirectPlay":true}]}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstream.URL = upstream.URL
	cfg.Clients = []config.ClientProfile{{Name: "yamby", Match: []string{"Yamby"}, Transcode: true}}

	srv, err := proxy.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/emby/Items/item123/PlaybackInfo", nil)
	req.Header.Set("User-Agent", "Yamby TV")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "/streambridge/transcode/") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/proxy`
Expected: fail because the package does not exist yet.

- [ ] **Step 3: Implement proxy routing**

Route `/streambridge/transcode/` to transcode handler, PlaybackInfo to custom fetch-and-rewrite logic, and all other requests to `httputil.ReverseProxy`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/proxy`
Expected: pass.

### Task 6: CLI and Documentation

**Files:**
- Modify: `cmd/emby-transcoder/main.go`
- Create: `README.md`
- Create: `config.example.json`

- [ ] **Step 1: Wire CLI flags**

Support `-config` and load defaults when no config file is supplied.

- [ ] **Step 2: Build**

Run: `go build ./cmd/emby-transcoder`
Expected: build succeeds.

- [ ] **Step 3: Full verification**

Run: `go test ./...`
Expected: all tests pass.

