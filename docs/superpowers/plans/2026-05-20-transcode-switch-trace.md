# Transcode Switch Trace Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a clear log trace for transcode switch decisions so seek-trigger timing can be inspected from logs.

**Architecture:** Keep the current transcode flow intact. Add a single trace helper and emit a shared `TRACE_SWITCH` prefix at request entry, decision points, session ready, and file-ready points.

**Tech Stack:** Go standard library logging and the existing transcode/proxy packages.

---

### Task 1: Add Trace Helper

**Files:**
- Create: `internal/transcode/trace.go`

- [ ] **Step 1: Add a shared trace logger**

```go
package transcode

import "log"

func traceSwitch(format string, args ...any) {
	log.Printf("TRACE_SWITCH "+format, args...)
}
```

### Task 2: Emit Trace Logs

**Files:**
- Modify: `internal/transcode/handler.go`
- Modify: `internal/transcode/manager.go`

- [ ] **Step 1: Log segment request entry and decision points**
- [ ] **Step 2: Log manager restart/reuse/create decisions**
- [ ] **Step 3: Log when the playlist or segment file becomes ready**

### Task 3: Verify

**Files:**
- Test: `go test ./...`

- [ ] **Step 1: Run tests**

Run: `go test ./...`
Expected: PASS
