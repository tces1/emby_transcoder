package proxy

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"emby-transcoder/internal/config"
	"emby-transcoder/internal/inputproxy"
	"emby-transcoder/internal/transcode"
)

const (
	dashboardPrefix     = "/emby_transcoder"
	dashboardCookieName = "emby_transcoder_session"
	dashboardSessionTTL = 12 * time.Hour
)

type dashboardAuthStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time
}

type dashboardSession struct {
	transcode.SessionStatus
	Routes []string                  `json:"routes,omitempty"`
	Cache  *inputproxy.CacheSnapshot `json:"cache,omitempty"`
}

type dashboardStatus struct {
	UpdatedAt   time.Time                   `json:"updated_at"`
	Workers     []inputproxy.WorkerSnapshot `json:"workers"`
	RouteChecks []inputproxy.RouteSnapshot  `json:"route_checks"`
	Sessions    []dashboardSession          `json:"sessions"`
}

func newDashboardAuthStore() *dashboardAuthStore {
	return &dashboardAuthStore{sessions: make(map[string]time.Time)}
}

func isDashboardPath(path string) bool {
	return path == dashboardPrefix || path == dashboardPrefix+"/" || strings.HasPrefix(path, dashboardPrefix+"/")
}

func (s *Server) serveDashboard(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == dashboardPrefix+"/login":
		s.dashboardLogin(w, r)
	case r.URL.Path == dashboardPrefix+"/logout":
		s.dashboardLogout(w, r)
	case r.URL.Path == dashboardPrefix+"/api/status":
		if !s.dashboardAuthorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.dashboardStatus(w)
	case r.URL.Path == dashboardPrefix+"/api/config":
		if !s.dashboardAuthorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.dashboardConfig(w, r)
	case r.URL.Path == dashboardPrefix+"/api/restart":
		if !s.dashboardAuthorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.dashboardRestart(w, r)
	case r.URL.Path == dashboardPrefix || r.URL.Path == dashboardPrefix+"/":
		if strings.TrimSpace(s.cfg.Server.DashboardPassword) == "" {
			http.Error(w, "dashboard password is not configured", http.StatusServiceUnavailable)
			return
		}
		if s.dashboardAuthorized(r) {
			writeDashboardHTML(w, dashboardHTML)
			return
		}
		writeDashboardHTML(w, dashboardLoginHTML)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) dashboardLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid login request", http.StatusBadRequest)
		return
	}
	password := r.Form.Get("password")
	if !s.dashboardPasswordValid(password) {
		writeDashboardHTMLStatus(w, dashboardLoginFailedHTML, http.StatusUnauthorized)
		return
	}
	if s.setDashboardSession(w, r) {
		http.Redirect(w, r, dashboardPrefix, http.StatusSeeOther)
	}
}

func (s *Server) dashboardLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if cookie, err := r.Cookie(dashboardCookieName); err == nil {
		s.dashboard.mu.Lock()
		delete(s.dashboard.sessions, cookie.Value)
		s.dashboard.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     dashboardCookieName,
		Value:    "",
		Path:     dashboardPrefix,
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   dashboardSecureCookie(r),
	})
	http.Redirect(w, r, dashboardPrefix, http.StatusSeeOther)
}

func (s *Server) dashboardAuthorized(r *http.Request) bool {
	if s.dashboard == nil {
		return false
	}
	cookie, err := r.Cookie(dashboardCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	now := time.Now()
	s.dashboard.mu.Lock()
	defer s.dashboard.mu.Unlock()
	expires, ok := s.dashboard.sessions[cookie.Value]
	if !ok || now.After(expires) {
		delete(s.dashboard.sessions, cookie.Value)
		return false
	}
	return true
}

func (s *Server) setDashboardSession(w http.ResponseWriter, r *http.Request) bool {
	if s.dashboard == nil {
		s.dashboard = newDashboardAuthStore()
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		http.Error(w, "cannot create dashboard session", http.StatusInternalServerError)
		return false
	}
	id := hex.EncodeToString(raw)
	expires := time.Now().Add(dashboardSessionTTL)
	s.dashboard.mu.Lock()
	for existing, existingExpiry := range s.dashboard.sessions {
		if time.Now().After(existingExpiry) {
			delete(s.dashboard.sessions, existing)
		}
	}
	s.dashboard.sessions[id] = expires
	s.dashboard.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     dashboardCookieName,
		Value:    id,
		Path:     dashboardPrefix,
		Expires:  expires,
		MaxAge:   int(dashboardSessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   dashboardSecureCookie(r),
	})
	return true
}

func dashboardSecureCookie(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) dashboardPasswordValid(candidate string) bool {
	expected := s.cfg.Server.DashboardPassword
	if expected == "" || len(candidate) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(expected)) == 1
}

func (s *Server) dashboardStatus(w http.ResponseWriter) {
	status := dashboardStatus{UpdatedAt: time.Now()}
	if s.inputProxy != nil {
		status.Workers = s.inputProxy.Snapshot()
		status.RouteChecks = s.inputProxy.RouteSnapshots()
	} else {
		workers := s.cfg.Transcode.DownloadWorkers
		if workers < 1 {
			workers = 1
		}
		if workers > 2 {
			workers = 2
		}
		for index := range workers {
			status.Workers = append(status.Workers, inputproxy.WorkerSnapshot{ID: index + 1, State: "disabled"})
		}
	}
	if s.transcodeManager != nil {
		sessions := s.transcodeManager.StatusSnapshot()
		sort.Slice(sessions, func(i, j int) bool {
			return sessions[i].ID < sessions[j].ID
		})
		var routes map[string][]string
		caches := map[string]inputproxy.CacheSnapshot{}
		if s.inputProxy != nil {
			routes = s.inputProxy.SessionRoutes()
			for _, cache := range s.inputProxy.CacheSnapshots() {
				if cache.SessionID != "" {
					caches[cache.SessionID] = cache
				}
			}
		}
		status.Sessions = make([]dashboardSession, 0, len(sessions))
		for _, session := range sessions {
			view := dashboardSession{SessionStatus: session, Routes: routes[session.ID]}
			if cache, ok := caches[session.ID]; ok {
				copied := cache
				view.Cache = &copied
			}
			status.Sessions = append(status.Sessions, view)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(status)
}

func (s *Server) dashboardConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		editable := s.cfg
		if strings.TrimSpace(s.configPath) != "" {
			loaded, err := config.Load(s.configPath)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			editable = loaded
		}
		editable.Path = ""
		editable.Server.DashboardPassword = ""
		if len(editable.Upstream.URLs) > 0 {
			editable.Upstream.URL = ""
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(editable)
	case http.MethodPut:
		if strings.TrimSpace(s.configPath) == "" {
			http.Error(w, "configuration file path is not available", http.StatusServiceUnavailable)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "cannot read configuration", http.StatusBadRequest)
			return
		}
		next, err := config.Parse(data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if next.Server.DashboardPassword == "" {
			next.Server.DashboardPassword = s.cfg.Server.DashboardPassword
		}
		next.Path = s.configPath
		if err := config.Save(s.configPath, next); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPut)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) dashboardRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.restartFunc == nil {
		http.Error(w, "service restart is not available", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = io.WriteString(w, `{"ok":true}`)
	go func() {
		time.Sleep(250 * time.Millisecond)
		s.restartOnce.Do(s.restartFunc)
	}()
}

func writeDashboardHTML(w http.ResponseWriter, page string) {
	writeDashboardHTMLStatus(w, page, http.StatusOK)
}

func writeDashboardHTMLStatus(w http.ResponseWriter, page string, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, page)
}

const dashboardLoginHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Emby Transcoder 登录</title><style>
body{margin:0;background:#0b1220;color:#e5edf8;font:15px system-ui;display:grid;place-items:center;min-height:100vh}
.card{width:min(420px,calc(100% - 40px));background:#131d2f;border:1px solid #263653;border-radius:16px;padding:28px;box-shadow:0 20px 60px #0008}
h1{margin:0 0 8px;font-size:24px}.muted{color:#8fa3bf;margin-bottom:22px}input{box-sizing:border-box;width:100%;padding:12px;border-radius:9px;border:1px solid #344766;background:#09111f;color:white}
button{width:100%;margin-top:14px;padding:12px;border:0;border-radius:9px;background:#2dd4bf;color:#04201d;font-weight:700;cursor:pointer}
</style></head><body><form class="card" method="post" action="/emby_transcoder/login"><h1>Emby Transcoder</h1><div class="muted">输入配置文件中的后台密码</div><input name="password" type="password" required autocomplete="current-password" placeholder="后台密码"><button>进入状态页</button></form></body></html>`

const dashboardLoginFailedHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>登录失败</title><style>body{background:#0b1220;color:#e5edf8;font:15px system-ui;display:grid;place-items:center;min-height:100vh}.card{background:#131d2f;padding:28px;border-radius:16px}a{color:#2dd4bf}</style></head>
<body><div class="card"><h2>密码错误</h2><p><a href="/emby_transcoder">返回重试</a></p></div></body></html>`

const dashboardHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Emby Transcoder 状态</title><style>
:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#08111f;color:#e8f0fb;font:14px system-ui,-apple-system,sans-serif}
header{display:flex;align-items:center;justify-content:space-between;padding:18px 24px;border-bottom:1px solid #20314c;background:#0d1728;position:sticky;top:0;z-index:2}.actions{display:flex;gap:6px;align-items:center;padding:4px;border:1px solid #2b405f;border-radius:12px;background:#091321}.actions form{margin:0}.actions button{display:grid;place-items:center;width:72px;height:36px;padding:0;border-color:transparent;background:transparent}.actions button:hover{background:#243754}.actions button:active{background:#123f3b;color:#5eead4}
h1{font-size:20px;margin:0}.sub{color:#8297b5;font-size:12px;margin-top:3px}button{background:#1c2b42;color:#cbd8ea;border:1px solid #344966;border-radius:8px;padding:8px 12px;cursor:pointer}
main{max-width:1280px;margin:auto;padding:22px}.section-title{font-size:13px;letter-spacing:.08em;color:#89a0bf;text-transform:uppercase;margin:8px 0 12px}
.workers{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:14px}.card{background:#101b2d;border:1px solid #243753;border-radius:14px;padding:16px;box-shadow:0 10px 30px #0003}
.worker-head,.session-head{display:flex;justify-content:space-between;gap:12px}.badge{padding:4px 9px;border-radius:999px;background:#263850;color:#a9bad1;font-size:12px}
.badge.active{background:#123f3b;color:#5eead4}.badge.error{background:#4b2028;color:#fda4af}.metric{font-size:25px;font-weight:750;margin:15px 0 3px}.muted{color:#8297b5}.route{white-space:nowrap;overflow:hidden;text-overflow:ellipsis;margin-top:12px;color:#b7c5d9}
.route-list{display:grid;gap:8px}.route-row{display:grid;grid-template-columns:minmax(150px,1.1fr) auto minmax(150px,1.1fr) auto minmax(180px,1fr);align-items:center;gap:10px;padding:11px 13px;border:1px solid #243753;border-radius:10px;background:#0b1525}.route-host{font-weight:650;overflow:hidden;text-overflow:ellipsis}.route-meta{color:#8297b5;font-size:12px}.route-arrow{color:#4c6381}
.sessions{display:grid;gap:14px}.pipeline{display:grid;grid-template-columns:1fr auto 1fr auto 1fr auto 1fr;align-items:stretch;gap:9px;margin-top:16px}
.node{border:1px solid #2b405f;background:#0b1525;border-radius:11px;padding:13px;min-width:0}.node.active{border-color:#1fb9a7;box-shadow:inset 0 0 0 1px #1fb9a744}.node-title{color:#8fa5c3;font-size:12px}.node-value{font-size:17px;font-weight:700;margin-top:7px;overflow:hidden;text-overflow:ellipsis}.arrow{display:grid;place-items:center;color:#4c6381;font-size:20px}
.charts{display:grid;grid-template-columns:1fr 1fr;gap:12px;margin-top:16px}.chart-card{border:1px solid #2b405f;background:#0b1525;border-radius:11px;padding:12px 13px}.chart-head{display:flex;justify-content:space-between;gap:10px;align-items:baseline;color:#8fa5c3;font-size:12px}.chart-head b{color:#e8f0fb;font-size:13px}
.legend{display:flex;flex-wrap:wrap;gap:12px;color:#8297b5;font-size:11px;margin-top:4px}.dot{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:5px;vertical-align:middle}
.coverage,.tank{position:relative;height:14px;margin:10px 0 8px;border-radius:999px;background:#162438;overflow:hidden;border:1px solid #243753}
.coverage .seg{position:absolute;top:0;bottom:0;border-radius:2px}.seg.cached{background:#2dd4bf}.seg.downloading{background:#fbbf24}
.tank-fill{height:100%;background:linear-gradient(90deg,#22d3ee,#2dd4bf);border-radius:999px}
.tank .mark{position:absolute;top:-3px;bottom:-3px;width:2px;background:#fda4af;opacity:.85}.tank .mark.resume{background:#fbbf24}
.spark{display:block;width:100%;height:44px}
.hidden{display:none}.config-card{background:#101b2d;border:1px solid #243753;border-radius:14px;padding:18px}.config-card textarea{width:100%;min-height:520px;resize:vertical;background:#08111f;color:#dce8f8;border:1px solid #344966;border-radius:10px;padding:14px;font:13px ui-monospace,SFMono-Regular,Menlo,monospace;line-height:1.5}.config-actions{display:flex;gap:10px;margin-top:12px}.config-actions .primary{background:#123f3b;color:#5eead4;border-color:#1f766c}.config-actions .danger{background:#4b2028;color:#fda4af;border-color:#7f303d}.message{min-height:20px;margin-top:10px;color:#8fa3bf}
.empty{text-align:center;padding:35px;color:#8297b5}@media(max-width:760px){.workers,.charts{grid-template-columns:1fr}.pipeline{grid-template-columns:1fr}.arrow{transform:rotate(90deg)}.route-row{grid-template-columns:1fr}.route-arrow{display:none}}
</style></head><body><header><div><h1>Emby Transcoder</h1><div class="sub" id="updated">正在连接状态服务…</div></div><div class="actions"><button type="button" id="statusTab">状态</button><button type="button" id="configTab">配置</button><button type="button" id="langButton">EN</button><form method="post" action="/emby_transcoder/logout"><button id="logoutButton">退出</button></form></div></header>
<main><section id="statusView"><div class="section-title" id="workersTitle">下载 Worker</div><div class="workers" id="workers"></div><div class="section-title" id="routesTitle" style="margin-top:24px">URL 可用性</div><div class="route-list" id="routes"></div><div class="section-title" id="sessionsTitle" style="margin-top:24px">转码状态机</div><div class="sessions" id="sessions"></div></section><section id="configView" class="hidden"><div class="section-title" id="configTitle">服务配置</div><div class="config-card"><div class="muted" id="configHint">后台密码留空表示保持原密码。保存后点击重启服务使配置生效。</div><textarea id="configText" spellcheck="false"></textarea><div class="config-actions"><button type="button" class="primary" id="saveConfig">保存配置</button><button type="button" class="danger" id="restartService">重启服务</button></div><div class="message" id="configMessage"></div></div></section></main>
<script>
const esc=v=>String(v??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const rate=n=>{n=Number(n||0);if(n>=1073741824)return(n/1073741824).toFixed(2)+' GiB/s';if(n>=1048576)return(n/1048576).toFixed(2)+' MiB/s';if(n>=1024)return(n/1024).toFixed(1)+' KiB/s';return n.toFixed(0)+' B/s'};
const bytes=n=>{n=Number(n||0);if(n>=1073741824)return(n/1073741824).toFixed(2)+' GiB';if(n>=1048576)return(n/1048576).toFixed(1)+' MiB';if(n>=1024)return(n/1024).toFixed(1)+' KiB';return n.toFixed(0)+' B'};
const seconds=n=>{n=Number(n||0);if(n>=60)return Math.floor(n/60)+'m '+(n%60).toFixed(0)+'s';return n.toFixed(0)+'s'};
const messages={
 zh:{status:'状态',config:'配置',logout:'退出',workers:'下载 Worker',routes:'URL 可用性',sessions:'转码状态机',configTitle:'服务配置',configHint:'后台密码留空表示保持原密码。保存后点击重启服务使配置生效。',save:'保存配置',restart:'重启服务',connecting:'正在连接状态服务…',updated:'最后更新 ',unavailable:'状态服务暂时不可用',total:'累计 ',current:'当前 ',recent:'最近 ',waiting:'等待任务',noVideo:'暂无视频',noRoutes:'当前没有待检测的媒体 URL',noSessions:'当前没有转码任务',notResolved:'尚未解析',failures:'失败 ',hedgeLosses:'竞速取消 ',line:'线路',download:'下载',ffmpeg:'FFmpeg',upload:'HLS 上传',cache:'下载缓存',pendingBytes:' · 在途 ',waitingDownload:'等待下载',cached:'已缓存',downloadingLegend:'下载中',speed:'速率',window:'窗口 ',buffer:'转码缓冲',pause:' / 暂停 ',generated:'已生成 ',duration:'片长 ',resume:'恢复 ',loadConfigError:'读取配置失败',saved:'配置已保存，重启后生效',saveError:'保存失败：',restartConfirm:'确定要重启服务吗？当前转码任务会中断。',restarting:'服务正在重启…',restartError:'重启失败：'},
 en:{status:'Status',config:'Config',logout:'Log out',workers:'Download Workers',routes:'URL Availability',sessions:'Transcode State',configTitle:'Service Configuration',configHint:'Leave the dashboard password empty to keep the current password. Restart the service after saving.',save:'Save Configuration',restart:'Restart Service',connecting:'Connecting to status service…',updated:'Last updated ',unavailable:'Status service unavailable',total:'Total ',current:'Current ',recent:'Last ',waiting:'Waiting for task',noVideo:'No active video',noRoutes:'No media URLs to inspect',noSessions:'No active transcode sessions',notResolved:'Not resolved',failures:'Failures ',hedgeLosses:'Race cancellations ',line:'Routes',download:'Download',ffmpeg:'FFmpeg',upload:'HLS Upload',cache:'Download Cache',pendingBytes:' · in flight ',waitingDownload:'Waiting for download',cached:'Cached',downloadingLegend:'Downloading',speed:'Speed',window:'Window ',buffer:'Transcode Buffer',pause:' / pause ',generated:'Generated ',duration:'Duration ',resume:'Resume ',loadConfigError:'Failed to load configuration',saved:'Configuration saved; restart to apply',saveError:'Save failed: ',restartConfirm:'Restart the service? Active transcodes will be interrupted.',restarting:'Service is restarting…',restartError:'Restart failed: '}
};
let lang=localStorage.getItem('emby-transcoder-lang')||((navigator.language||'').toLowerCase().startsWith('zh')?'zh':'en');
const t=k=>(messages[lang]&&messages[lang][k])||k;
const badge=s=>s==='downloading'||s==='forwarding'||s==='probing'||s==='running'||s==='active'||s==='ready'?'active':s==='idle'||s==='disabled'||s==='paused'||s==='pending'||s==='duplicate'||s==='standby'?'':'error';
const stateLabels={zh:{downloading:'下载中',forwarding:'转发中',probing:'探测中',running:'转码中',idle:'空闲',disabled:'未启用',paused:'已暂停',error:'错误',exited:'已结束',pending:'待探测',ready:'可用',active:'已采纳',standby:'故障待命',duplicate:'同源重复',unsupported:'不支持 Range',rejected:'内容不一致',unhealthy:'线路异常'},en:{downloading:'Downloading',forwarding:'Forwarding',probing:'Probing',running:'Transcoding',idle:'Idle',disabled:'Disabled',paused:'Paused',error:'Error',exited:'Exited',pending:'Pending',ready:'Available',active:'Active',standby:'Failover standby',duplicate:'Duplicate origin',unsupported:'Range unsupported',rejected:'Content mismatch',unhealthy:'Unhealthy'}};
const reasonLabels={zh:{not_probed:'尚未探测',checking_range_support:'检查 Range 支持',request_error:'请求失败',timeout:'探测超时',invalid_request:'请求无效',range_unsupported:'不支持字节范围',range_supported:'Range 可用',same_final_host:'最终域名重复',validator_matched:'校验信息一致',size_mismatch:'文件大小不一致',etag_mismatch:'ETag 不一致',primary_fingerprint_error:'主线路指纹失败',candidate_fingerprint_error:'候选线路指纹失败',fingerprint_mismatch:'内容指纹不一致',fingerprint_matched:'内容指纹一致',accepted:'已加入下载线路',failover_standby:'主线路故障后启用',consecutive_failures:'连续下载失败'},en:{not_probed:'Not probed',checking_range_support:'Checking Range support',request_error:'Request failed',timeout:'Probe timed out',invalid_request:'Invalid request',range_unsupported:'Byte ranges unsupported',range_supported:'Range available',same_final_host:'Duplicate final host',validator_matched:'Validator matched',size_mismatch:'File size mismatch',etag_mismatch:'ETag mismatch',primary_fingerprint_error:'Primary fingerprint failed',candidate_fingerprint_error:'Candidate fingerprint failed',fingerprint_mismatch:'Content fingerprint mismatch',fingerprint_matched:'Content fingerprint matched',accepted:'Accepted for downloads',failover_standby:'Used after primary failure',consecutive_failures:'Consecutive failures'}};
const label=s=>(stateLabels[lang]&&stateLabels[lang][s])||s;
const reasonLabel=s=>(reasonLabels[lang]&&reasonLabels[lang][s])||s;
const hist=new Map();
function pushHist(id,sample){
 const row=hist.get(id)||{down:[],cached:[],tbuf:[]};
 row.down.push(sample.down);row.cached.push(sample.cached);row.tbuf.push(sample.tbuf);
 if(row.down.length>90){row.down.shift();row.cached.shift();row.tbuf.shift()}
 hist.set(id,row);return row;
}
function spark(values,color){
 const pts=(values||[]).map(Number).filter(n=>isFinite(n));
 if(pts.length<2)return '<svg class="spark" viewBox="0 0 120 36" preserveAspectRatio="none"></svg>';
 const min=Math.min(0,...pts),max=Math.max(1,...pts);
 const d=pts.map((v,i)=>{const x=2+i*116/Math.max(pts.length-1,1);const y=34-((v-min)/(max-min||1))*32;return x.toFixed(1)+','+y.toFixed(1)}).join(' ');
 return '<svg class="spark" viewBox="0 0 120 36" preserveAspectRatio="none"><polyline fill="none" stroke="'+color+'" stroke-width="1.8" stroke-linejoin="round" stroke-linecap="round" points="'+d+'"/></svg>';
}
function coverage(cache){
 if(!cache||!cache.size)return '<div class="coverage"></div>';
 return '<div class="coverage">'+(cache.ranges||[]).map(r=>{
  const left=100*Number(r.start)/cache.size,width=100*(Number(r.end)-Number(r.start)+1)/cache.size;
  return '<i class="seg '+esc(r.state)+'" style="left:'+left.toFixed(3)+'%;width:'+Math.max(width,0.12).toFixed(3)+'%"></i>';
 }).join('')+'</div>';
}
function tank(s){
 const pause=Math.max(Number(s.buffer_pause_seconds)||300,1),buf=Math.max(Number(s.buffer_seconds)||0,0),resume=Math.max(Number(s.buffer_resume_seconds)||0,0);
 const fill=Math.min(100,100*buf/pause),resumePct=Math.min(100,100*resume/pause);
 return '<div class="tank"><div class="tank-fill" style="width:'+fill.toFixed(1)+'%"></div><i class="mark resume" style="left:'+resumePct.toFixed(1)+'%"></i><i class="mark pause" style="left:99.4%"></i></div>';
}
let lastData=null;
function render(data){
 lastData=data;
 document.getElementById('updated').textContent=t('updated')+new Date(data.updated_at).toLocaleTimeString();
 document.getElementById('workers').innerHTML=(data.workers||[]).map(function(w){const live=w.state==='downloading'||w.state==='forwarding'||w.state==='probing';return '<div class="card"><div class="worker-head"><b>Worker '+esc(w.id)+'</b><span class="badge '+badge(w.state)+'">'+esc(label(w.state))+'</span></div><div class="metric">'+rate(w.download_bps)+'</div><div class="muted">'+t('total')+bytes(w.total_bytes)+'</div><div class="route">'+(live?t('current'):t('recent'))+esc(w.route||t('waiting'))+' · '+esc(w.byte_range||'-')+'</div><div class="muted">'+esc(w.video_name||t('noVideo'))+(w.generation_id?' · G'+esc(w.generation_id):'')+'</div></div>'}).join('');
 const checks=data.route_checks||[];
 document.getElementById('routes').innerHTML=checks.length?checks.map(r=>'<div class="route-row"><div><div class="route-host">'+esc(r.entry||'-')+'</div><div class="route-meta">'+esc(r.session_id||'')+(r.generation_id?' · G'+esc(r.generation_id):'')+'</div></div><div class="route-arrow">→</div><div class="route-host">'+esc(r.final||t('notResolved'))+'</div><span class="badge '+badge(r.state)+'">'+esc(label(r.state))+'</span><div class="route-meta">'+esc(reasonLabel(r.reason)||'-')+' · '+t('failures')+esc(r.failures||0)+' · '+t('hedgeLosses')+esc(r.hedge_losses||0)+'</div></div>').join(''):'<div class="card empty">'+t('noRoutes')+'</div>';
 const sessions=data.sessions||[];
 const live=new Set(sessions.map(s=>s.id));
 for(const id of hist.keys()){if(!live.has(id))hist.delete(id)}
 document.getElementById('sessions').innerHTML=sessions.length?sessions.map(s=>{
   const ws=(data.workers||[]).filter(w=>w.session_id===s.id),down=ws.reduce((n,w)=>n+Number(w.download_bps||0),0);
   const routes=[...new Set((s.routes||[]).concat(ws.filter(w=>w.state==='downloading'||w.state==='probing').map(w=>w.route)).filter(Boolean))].join(' + ')||t('waiting');
   const cache=s.cache||{};
   const row=pushHist(s.id,{down:down,cached:Number(cache.cached_bytes||0),tbuf:Number(s.buffer_seconds||0)});
   const cachedLabel=cache.size?bytes(cache.cached_bytes||0)+' / '+bytes(cache.size):t('waitingDownload');
   const pending=Number(cache.pending_bytes||0)>0?t('pendingBytes')+bytes(cache.pending_bytes):'';
   return '<div class="card"><div class="session-head"><div><b>'+esc(s.video_name||s.id)+'</b><div class="muted">'+esc(s.hardware_pipeline||'software')+'</div></div><span class="badge '+badge(s.state)+'">'+esc(label(s.state))+'</span></div><div class="pipeline"><div class="node '+(down>0?'active':'')+'"><div class="node-title">'+t('line')+'</div><div class="node-value">'+esc(routes)+'</div></div><div class="arrow">→</div><div class="node '+(down>0?'active':'')+'"><div class="node-title">'+t('download')+'</div><div class="node-value">'+rate(down)+'</div></div><div class="arrow">→</div><div class="node '+(s.state==='running'?'active':'')+'"><div class="node-title">'+t('ffmpeg')+'</div><div class="node-value">'+Number(s.transcode_speed||0).toFixed(2)+'×</div></div><div class="arrow">→</div><div class="node '+(s.upload_bps>0?'active':'')+'"><div class="node-title">'+t('upload')+'</div><div class="node-value">'+rate(s.upload_bps)+'</div></div></div><div class="charts"><div class="chart-card"><div class="chart-head"><b>'+t('cache')+'</b><span>'+esc(cachedLabel)+pending+'</span></div>'+coverage(cache)+spark(row.down,'#67e8f9')+'<div class="legend"><span><i class="dot" style="background:#2dd4bf"></i>'+t('cached')+'</span><span><i class="dot" style="background:#fbbf24"></i>'+t('downloadingLegend')+'</span><span><i class="dot" style="background:#67e8f9"></i>'+t('speed')+'</span><span>'+t('window')+bytes(cache.window_bytes||0)+'</span></div></div><div class="chart-card"><div class="chart-head"><b>'+t('buffer')+'</b><span>'+seconds(s.buffer_seconds)+t('pause')+seconds(s.buffer_pause_seconds)+'</span></div>'+tank(s)+spark(row.tbuf,'#22d3ee')+'<div class="legend"><span>'+t('generated')+seconds(s.generated_seconds)+'</span><span>'+t('duration')+seconds(s.runtime_seconds)+'</span><span>'+t('resume')+seconds(s.buffer_resume_seconds)+'</span></div></div></div></div>';
 }).join(''):'<div class="card empty">'+t('noSessions')+'</div>';
}
function applyLanguage(){
 document.documentElement.lang=lang==='zh'?'zh-CN':'en';
 document.getElementById('statusTab').textContent=t('status');document.getElementById('configTab').textContent=t('config');document.getElementById('logoutButton').textContent=t('logout');document.getElementById('langButton').textContent=lang==='zh'?'EN':'中文';
 document.getElementById('workersTitle').textContent=t('workers');document.getElementById('routesTitle').textContent=t('routes');document.getElementById('sessionsTitle').textContent=t('sessions');document.getElementById('configTitle').textContent=t('configTitle');document.getElementById('configHint').textContent=t('configHint');document.getElementById('saveConfig').textContent=t('save');document.getElementById('restartService').textContent=t('restart');
 if(lastData)render(lastData);else document.getElementById('updated').textContent=t('connecting');
}
async function refresh(){try{const r=await fetch('/emby_transcoder/api/status',{cache:'no-store'});if(r.status===401){location.reload();return}if(!r.ok)throw new Error();render(await r.json())}catch(e){document.getElementById('updated').textContent=t('unavailable')}}
async function loadConfig(){const message=document.getElementById('configMessage');try{const r=await fetch('/emby_transcoder/api/config',{cache:'no-store'});if(!r.ok)throw new Error(await r.text());document.getElementById('configText').value=JSON.stringify(await r.json(),null,2);message.textContent=''}catch(e){message.textContent=t('loadConfigError')+': '+e.message}}
document.getElementById('statusTab').onclick=()=>{document.getElementById('statusView').classList.remove('hidden');document.getElementById('configView').classList.add('hidden')};
document.getElementById('configTab').onclick=()=>{document.getElementById('statusView').classList.add('hidden');document.getElementById('configView').classList.remove('hidden');loadConfig()};
document.getElementById('langButton').onclick=()=>{lang=lang==='zh'?'en':'zh';localStorage.setItem('emby-transcoder-lang',lang);applyLanguage()};
document.getElementById('saveConfig').onclick=async()=>{const message=document.getElementById('configMessage');try{const value=JSON.parse(document.getElementById('configText').value);const r=await fetch('/emby_transcoder/api/config',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(value)});if(!r.ok)throw new Error(await r.text());message.textContent=t('saved')}catch(e){message.textContent=t('saveError')+e.message}};
document.getElementById('restartService').onclick=async()=>{if(!confirm(t('restartConfirm')))return;const message=document.getElementById('configMessage');try{const r=await fetch('/emby_transcoder/api/restart',{method:'POST'});if(!r.ok)throw new Error(await r.text());message.textContent=t('restarting');setTimeout(()=>location.reload(),2500)}catch(e){message.textContent=t('restartError')+e.message}};
applyLanguage();refresh();setInterval(refresh,1000);
</script></body></html>`
