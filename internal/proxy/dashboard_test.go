package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"emby-transcoder/internal/config"
	"emby-transcoder/internal/transcode"
)

func TestDashboardRequiresPasswordAuthentication(t *testing.T) {
	server := newDashboardTestServer(t, "valid-password")

	loginPage := httptest.NewRecorder()
	server.ServeHTTP(loginPage, httptest.NewRequest(http.MethodGet, dashboardPrefix, nil))
	if loginPage.Code != http.StatusOK || !strings.Contains(loginPage.Body.String(), "后台密码") {
		t.Fatalf("login page status=%d body=%s", loginPage.Code, loginPage.Body.String())
	}

	for method, path := range map[string]string{
		http.MethodGet:  dashboardPrefix + "/api/status",
		http.MethodPut:  dashboardPrefix + "/api/config",
		http.MethodPost: dashboardPrefix + "/api/restart",
	} {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(method, path, nil))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthorized %s %s status code = %d", method, path, response.Code)
		}
	}

}

func TestDashboardIncludesRouteDiagnostics(t *testing.T) {
	for _, expected := range []string{"URL 可用性", "URL Availability", "route_checks", "竞速取消", "最终域名重复", "服务配置", "Service Configuration", "restartService"} {
		if !strings.Contains(dashboardHTML, expected) {
			t.Fatalf("dashboard is missing %q", expected)
		}
	}
}

func TestDashboardLoginCreatesOpaqueSessionAndServesStatus(t *testing.T) {
	server := newDashboardTestServer(t, "valid-password")
	form := url.Values{"password": []string{"valid-password"}}
	loginRequest := httptest.NewRequest(http.MethodPost, dashboardPrefix+"/login", strings.NewReader(form.Encode()))
	loginRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	login := httptest.NewRecorder()
	server.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d body=%s", login.Code, login.Body.String())
	}
	response := login.Result()
	var sessionCookie *http.Cookie
	for _, cookie := range response.Cookies() {
		if cookie.Name == dashboardCookieName {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil || sessionCookie.Value == "" || strings.Contains(sessionCookie.Value, "valid-password") {
		t.Fatalf("dashboard session cookie = %#v", sessionCookie)
	}

	pageRequest := httptest.NewRequest(http.MethodGet, dashboardPrefix, nil)
	pageRequest.AddCookie(sessionCookie)
	page := httptest.NewRecorder()
	server.ServeHTTP(page, pageRequest)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "转码状态机") {
		t.Fatalf("dashboard status=%d body=%s", page.Code, page.Body.String())
	}
	if !strings.Contains(page.Body.String(), "下载缓存") || !strings.Contains(page.Body.String(), "转码缓冲") {
		t.Fatalf("dashboard missing buffer charts: %s", page.Body.String())
	}
	if !strings.Contains(page.Body.String(), "转码中") || !strings.Contains(page.Body.String(), "空闲") {
		t.Fatalf("dashboard missing Chinese state labels: %s", page.Body.String())
	}
	if strings.Contains(page.Body.String(), "valid-password") {
		t.Fatal("dashboard page leaked the configured password")
	}

	statusRequest := httptest.NewRequest(http.MethodGet, dashboardPrefix+"/api/status", nil)
	statusRequest.AddCookie(sessionCookie)
	status := httptest.NewRecorder()
	server.ServeHTTP(status, statusRequest)
	if status.Code != http.StatusOK {
		t.Fatalf("status API code = %d body=%s", status.Code, status.Body.String())
	}
	if !strings.Contains(status.Body.String(), `"workers"`) || !strings.Contains(status.Body.String(), `"sessions"`) {
		t.Fatalf("status API body = %s", status.Body.String())
	}
}

func TestDashboardConfigSaveAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	server := newDashboardTestServer(t, "valid-password")
	server.configPath = path
	server.cfg.Path = path
	if err := config.Save(path, server.cfg); err != nil {
		t.Fatal(err)
	}
	restarted := make(chan struct{})
	server.restartFunc = func() { close(restarted) }

	form := url.Values{"password": []string{"valid-password"}}
	loginRequest := httptest.NewRequest(http.MethodPost, dashboardPrefix+"/login", strings.NewReader(form.Encode()))
	loginRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	login := httptest.NewRecorder()
	server.ServeHTTP(login, loginRequest)
	var sessionCookie *http.Cookie
	for _, cookie := range login.Result().Cookies() {
		if cookie.Name == dashboardCookieName {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil {
		t.Fatal("missing dashboard session cookie")
	}

	getRequest := httptest.NewRequest(http.MethodGet, dashboardPrefix+"/api/config", nil)
	getRequest.AddCookie(sessionCookie)
	getResponse := httptest.NewRecorder()
	server.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusOK || strings.Contains(getResponse.Body.String(), "valid-password") {
		t.Fatalf("config response status=%d body=%s", getResponse.Code, getResponse.Body.String())
	}

	edited := server.cfg
	edited.Server.DashboardPassword = ""
	edited.Upstream.URL = ""
	edited.Upstream.URLs = []string{"https://updated.example"}
	body, err := json.Marshal(edited)
	if err != nil {
		t.Fatal(err)
	}
	putRequest := httptest.NewRequest(http.MethodPut, dashboardPrefix+"/api/config", strings.NewReader(string(body)))
	putRequest.AddCookie(sessionCookie)
	putResponse := httptest.NewRecorder()
	server.ServeHTTP(putResponse, putRequest)
	if putResponse.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", putResponse.Code, putResponse.Body.String())
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Upstream.URL != "https://updated.example" || saved.Server.DashboardPassword != "valid-password" {
		t.Fatalf("saved config = %+v", saved)
	}
	refreshRequest := httptest.NewRequest(http.MethodGet, dashboardPrefix+"/api/config", nil)
	refreshRequest.AddCookie(sessionCookie)
	refreshResponse := httptest.NewRecorder()
	server.ServeHTTP(refreshResponse, refreshRequest)
	if refreshResponse.Code != http.StatusOK ||
		!strings.Contains(refreshResponse.Body.String(), "https://updated.example") ||
		strings.Contains(refreshResponse.Body.String(), `"url":`) {
		t.Fatalf("refreshed config status=%d body=%s", refreshResponse.Code, refreshResponse.Body.String())
	}

	restartRequest := httptest.NewRequest(http.MethodPost, dashboardPrefix+"/api/restart", nil)
	restartRequest.AddCookie(sessionCookie)
	restartResponse := httptest.NewRecorder()
	server.ServeHTTP(restartResponse, restartRequest)
	if restartResponse.Code != http.StatusAccepted {
		t.Fatalf("restart status=%d body=%s", restartResponse.Code, restartResponse.Body.String())
	}
	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("restart callback was not invoked")
	}
}

func TestDashboardStatusIncludesTranscodeBuffer(t *testing.T) {
	cfg := config.Default()
	cfg.Server.DashboardPassword = "valid-password"
	manager := transcode.NewManager(transcode.Options{TempDir: t.TempDir()})
	t.Cleanup(manager.Close)
	session, err := manager.Ensure("item123", transcode.Request{
		InputURL: "http://upstream/video",
		Media:    transcode.MediaInfo{Name: "Buffered Movie"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for segment := 0; segment <= 4; segment++ {
		path := filepath.Join(session.Dir, fmt.Sprintf("segment_%05d.ts", segment))
		if err := os.WriteFile(path, []byte("ts"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manager.RecordSegmentRequest(session.ID, 4)

	server := &Server{
		cfg:              cfg,
		transcodeManager: manager,
		dashboard:        newDashboardAuthStore(),
	}
	form := url.Values{"password": []string{"valid-password"}}
	loginRequest := httptest.NewRequest(http.MethodPost, dashboardPrefix+"/login", strings.NewReader(form.Encode()))
	loginRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	login := httptest.NewRecorder()
	server.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d", login.Code)
	}
	var sessionCookie *http.Cookie
	for _, cookie := range login.Result().Cookies() {
		if cookie.Name == dashboardCookieName {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil {
		t.Fatal("missing dashboard session cookie")
	}

	statusRequest := httptest.NewRequest(http.MethodGet, dashboardPrefix+"/api/status", nil)
	statusRequest.AddCookie(sessionCookie)
	status := httptest.NewRecorder()
	server.ServeHTTP(status, statusRequest)
	body := status.Body.String()
	if status.Code != http.StatusOK {
		t.Fatalf("status API code = %d body=%s", status.Code, body)
	}
	if !strings.Contains(body, `"buffer_seconds":2`) || !strings.Contains(body, `"generated_seconds":10`) {
		t.Fatalf("status API missing buffer fields: %s", body)
	}
	if !strings.Contains(body, `"buffer_pause_seconds":300`) {
		t.Fatalf("status API missing pause threshold: %s", body)
	}
}

func TestDashboardIsDisabledWithoutConfiguredPassword(t *testing.T) {
	server := newDashboardTestServer(t, "")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, dashboardPrefix, nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
}

func newDashboardTestServer(t *testing.T, password string) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Server.DashboardPassword = password
	cfg.Transcode.DownloadWorkers = 2
	return &Server{
		cfg:              cfg,
		transcodeManager: transcode.NewManager(transcode.Options{}),
		dashboard:        newDashboardAuthStore(),
	}
}
