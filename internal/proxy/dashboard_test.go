package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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

	status := httptest.NewRecorder()
	server.ServeHTTP(status, httptest.NewRequest(http.MethodGet, dashboardPrefix+"/api/status", nil))
	if status.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status code = %d", status.Code)
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
