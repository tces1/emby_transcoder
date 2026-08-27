package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"emby-transcoder/internal/config"
	"emby-transcoder/internal/transcode"
)

func TestDashboardRequiresEmbyAuthentication(t *testing.T) {
	server := newDashboardTestServer(t, "valid-token")

	loginPage := httptest.NewRecorder()
	server.ServeHTTP(loginPage, httptest.NewRequest(http.MethodGet, dashboardPrefix, nil))
	if loginPage.Code != http.StatusOK || !strings.Contains(loginPage.Body.String(), "API Key / Token") {
		t.Fatalf("login page status=%d body=%s", loginPage.Code, loginPage.Body.String())
	}

	status := httptest.NewRecorder()
	server.ServeHTTP(status, httptest.NewRequest(http.MethodGet, dashboardPrefix+"/api/status", nil))
	if status.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status code = %d", status.Code)
	}

	queryToken := httptest.NewRecorder()
	server.ServeHTTP(queryToken, httptest.NewRequest(http.MethodGet, dashboardPrefix+"?api_key=valid-token", nil))
	if queryToken.Code != http.StatusOK || !strings.Contains(queryToken.Body.String(), "API Key / Token") {
		t.Fatalf("query token should not authenticate: status=%d", queryToken.Code)
	}
}

func TestDashboardLoginCreatesOpaqueSessionAndServesStatus(t *testing.T) {
	server := newDashboardTestServer(t, "valid-token")
	form := url.Values{"token": []string{"valid-token"}}
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
	if sessionCookie == nil || sessionCookie.Value == "" || strings.Contains(sessionCookie.Value, "valid-token") {
		t.Fatalf("dashboard session cookie = %#v", sessionCookie)
	}

	pageRequest := httptest.NewRequest(http.MethodGet, dashboardPrefix, nil)
	pageRequest.AddCookie(sessionCookie)
	page := httptest.NewRecorder()
	server.ServeHTTP(page, pageRequest)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "转码状态机") {
		t.Fatalf("dashboard status=%d body=%s", page.Code, page.Body.String())
	}
	if strings.Contains(page.Body.String(), "valid-token") {
		t.Fatal("dashboard page leaked the Emby token")
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

func TestDashboardTokenValidationRejectsLoginRedirect(t *testing.T) {
	upstream, err := url.Parse("http://upstream.local")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	transport := dashboardTransportFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusFound,
			Status:     "302 Found",
			Header:     http.Header{"Location": []string{"http://upstream.local/login"}},
			Body:       io.NopCloser(strings.NewReader("redirect")),
			Request:    req,
		}, nil
	})
	server := &Server{
		upstream:  upstream,
		client:    &http.Client{Transport: transport},
		dashboard: newDashboardAuthStore(),
	}
	form := url.Values{"token": []string{"invalid-token"}}
	request := httptest.NewRequest(http.MethodPost, dashboardPrefix+"/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || calls != 1 {
		t.Fatalf("status=%d redirect calls=%d", response.Code, calls)
	}
}

func newDashboardTestServer(t *testing.T, validToken string) *Server {
	t.Helper()
	upstream, err := url.Parse("http://upstream.local")
	if err != nil {
		t.Fatal(err)
	}
	transport := dashboardTransportFunc(func(req *http.Request) (*http.Response, error) {
		status := http.StatusUnauthorized
		if req.URL.Path == "/emby/Users/Me" &&
			req.URL.RawQuery == "" &&
			req.Header.Get("X-Emby-Token") == validToken {
			status = http.StatusOK
		}
		return &http.Response{
			StatusCode: status,
			Status:     http.StatusText(status),
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    req,
		}, nil
	})
	cfg := config.Default()
	cfg.Transcode.DownloadWorkers = 2
	return &Server{
		cfg:              cfg,
		upstream:         upstream,
		client:           &http.Client{Transport: transport},
		transcodeManager: transcode.NewManager(transcode.Options{}),
		dashboard:        newDashboardAuthStore(),
	}
}

type dashboardTransportFunc func(*http.Request) (*http.Response, error)

func (f dashboardTransportFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
