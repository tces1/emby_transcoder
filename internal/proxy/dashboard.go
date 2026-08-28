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
	UpdatedAt time.Time                   `json:"updated_at"`
	Workers   []inputproxy.WorkerSnapshot `json:"workers"`
	Sessions  []dashboardSession          `json:"sessions"`
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
header{display:flex;align-items:center;justify-content:space-between;padding:18px 24px;border-bottom:1px solid #20314c;background:#0d1728;position:sticky;top:0;z-index:2}
h1{font-size:20px;margin:0}.sub{color:#8297b5;font-size:12px;margin-top:3px}button{background:#1c2b42;color:#cbd8ea;border:1px solid #344966;border-radius:8px;padding:8px 12px;cursor:pointer}
main{max-width:1280px;margin:auto;padding:22px}.section-title{font-size:13px;letter-spacing:.08em;color:#89a0bf;text-transform:uppercase;margin:8px 0 12px}
.workers{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:14px}.card{background:#101b2d;border:1px solid #243753;border-radius:14px;padding:16px;box-shadow:0 10px 30px #0003}
.worker-head,.session-head{display:flex;justify-content:space-between;gap:12px}.badge{padding:4px 9px;border-radius:999px;background:#263850;color:#a9bad1;font-size:12px}
.badge.active{background:#123f3b;color:#5eead4}.badge.error{background:#4b2028;color:#fda4af}.metric{font-size:25px;font-weight:750;margin:15px 0 3px}.muted{color:#8297b5}.route{white-space:nowrap;overflow:hidden;text-overflow:ellipsis;margin-top:12px;color:#b7c5d9}
.sessions{display:grid;gap:14px}.pipeline{display:grid;grid-template-columns:1fr auto 1fr auto 1fr auto 1fr;align-items:stretch;gap:9px;margin-top:16px}
.node{border:1px solid #2b405f;background:#0b1525;border-radius:11px;padding:13px;min-width:0}.node.active{border-color:#1fb9a7;box-shadow:inset 0 0 0 1px #1fb9a744}.node-title{color:#8fa5c3;font-size:12px}.node-value{font-size:17px;font-weight:700;margin-top:7px;overflow:hidden;text-overflow:ellipsis}.arrow{display:grid;place-items:center;color:#4c6381;font-size:20px}
.charts{display:grid;grid-template-columns:1fr 1fr;gap:12px;margin-top:16px}.chart-card{border:1px solid #2b405f;background:#0b1525;border-radius:11px;padding:12px 13px}.chart-head{display:flex;justify-content:space-between;gap:10px;align-items:baseline;color:#8fa5c3;font-size:12px}.chart-head b{color:#e8f0fb;font-size:13px}
.coverage,.tank{position:relative;height:14px;margin:10px 0 8px;border-radius:999px;background:#162438;overflow:hidden;border:1px solid #243753}
.coverage .seg{position:absolute;top:0;bottom:0;border-radius:2px}.seg.cached{background:#2dd4bf}.seg.downloading{background:#fbbf24}
.tank-fill{height:100%;background:linear-gradient(90deg,#22d3ee,#2dd4bf);border-radius:999px}
.tank .mark{position:absolute;top:-3px;bottom:-3px;width:2px;background:#fda4af;opacity:.85}.tank .mark.resume{background:#fbbf24}
.spark{display:block;width:100%;height:44px}.legend{display:flex;gap:12px;color:#8297b5;font-size:11px;margin-top:4px}.dot{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:5px;vertical-align:middle}
.empty{text-align:center;padding:35px;color:#8297b5}@media(max-width:760px){.workers,.charts{grid-template-columns:1fr}.pipeline{grid-template-columns:1fr}.arrow{transform:rotate(90deg)}}
</style></head><body><header><div><h1>Emby Transcoder</h1><div class="sub" id="updated">正在连接状态服务…</div></div><form method="post" action="/emby_transcoder/logout"><button>退出</button></form></header>
<main><div class="section-title">下载 Worker</div><div class="workers" id="workers"></div><div class="section-title" style="margin-top:24px">转码状态机</div><div class="sessions" id="sessions"></div></main>
<script>
const esc=v=>String(v??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const rate=n=>{n=Number(n||0);if(n>=1073741824)return(n/1073741824).toFixed(2)+' GiB/s';if(n>=1048576)return(n/1048576).toFixed(2)+' MiB/s';if(n>=1024)return(n/1024).toFixed(1)+' KiB/s';return n.toFixed(0)+' B/s'};
const bytes=n=>{n=Number(n||0);if(n>=1073741824)return(n/1073741824).toFixed(2)+' GiB';if(n>=1048576)return(n/1048576).toFixed(1)+' MiB';if(n>=1024)return(n/1024).toFixed(1)+' KiB';return n.toFixed(0)+' B'};
const seconds=n=>{n=Number(n||0);if(n>=60)return Math.floor(n/60)+'m '+(n%60).toFixed(0)+'s';return n.toFixed(0)+'s'};
const badge=s=>s==='downloading'||s==='forwarding'||s==='probing'||s==='running'?'active':s==='idle'||s==='disabled'||s==='paused'?'':'error';
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
 return '<div class="tank"><div class="tank-fill" style="width:'+fill.toFixed(1)+'%"></div><i class="mark resume" style="left:'+resumePct.toFixed(1)+'%" title="恢复阈值"></i><i class="mark pause" style="left:99.4%" title="暂停阈值"></i></div>';
}
function render(data){
 document.getElementById('updated').textContent='最后更新 '+new Date(data.updated_at).toLocaleTimeString();
 document.getElementById('workers').innerHTML=(data.workers||[]).map(function(w){return '<div class="card"><div class="worker-head"><b>Worker '+esc(w.id)+'</b><span class="badge '+badge(w.state)+'">'+esc(w.state)+'</span></div><div class="metric">'+rate(w.download_bps)+'</div><div class="muted">累计 '+bytes(w.total_bytes)+'</div><div class="route">'+esc(w.route||'等待任务')+' · '+esc(w.byte_range||'-')+'</div><div class="muted">'+esc(w.video_name||'暂无视频')+'</div></div>'}).join('');
 const sessions=data.sessions||[];
 const live=new Set(sessions.map(s=>s.id));
 for(const id of hist.keys()){if(!live.has(id))hist.delete(id)}
 document.getElementById('sessions').innerHTML=sessions.length?sessions.map(s=>{
   const ws=(data.workers||[]).filter(w=>w.session_id===s.id),down=ws.reduce((n,w)=>n+Number(w.download_bps||0),0);
   const routes=[...new Set((s.routes||[]).concat(ws.filter(w=>w.state==='downloading'||w.state==='probing').map(w=>w.route)).filter(Boolean))].join(' + ')||'等待线路';
   const cache=s.cache||{};
   const row=pushHist(s.id,{down:down,cached:Number(cache.cached_bytes||0),tbuf:Number(s.buffer_seconds||0)});
   const cachedLabel=cache.size?bytes(cache.cached_bytes||0)+' / '+bytes(cache.size):'等待下载';
   const pending=Number(cache.pending_bytes||0)>0?' · 在途 '+bytes(cache.pending_bytes):'';
   return '<div class="card"><div class="session-head"><div><b>'+esc(s.video_name||s.id)+'</b><div class="muted">'+esc(s.hardware_pipeline||'software')+'</div></div><span class="badge '+badge(s.state)+'">'+esc(s.state)+'</span></div><div class="pipeline"><div class="node '+(down>0?'active':'')+'"><div class="node-title">线路</div><div class="node-value">'+esc(routes)+'</div></div><div class="arrow">→</div><div class="node '+(down>0?'active':'')+'"><div class="node-title">下载</div><div class="node-value">'+rate(down)+'</div></div><div class="arrow">→</div><div class="node '+(s.state==='running'?'active':'')+'"><div class="node-title">FFmpeg</div><div class="node-value">'+Number(s.transcode_speed||0).toFixed(2)+'×</div></div><div class="arrow">→</div><div class="node '+(s.upload_bps>0?'active':'')+'"><div class="node-title">HLS 上传</div><div class="node-value">'+rate(s.upload_bps)+'</div></div></div><div class="charts"><div class="chart-card"><div class="chart-head"><b>下载缓存</b><span>'+esc(cachedLabel)+pending+'</span></div>'+coverage(cache)+spark(row.down,'#67e8f9')+'<div class="legend"><span><i class="dot" style="background:#2dd4bf"></i>已缓存</span><span><i class="dot" style="background:#fbbf24"></i>下载中</span><span><i class="dot" style="background:#67e8f9"></i>速率</span><span>窗口 '+bytes(cache.window_bytes||0)+'</span></div></div><div class="chart-card"><div class="chart-head"><b>转码缓冲</b><span>'+seconds(s.buffer_seconds)+' / 暂停 '+seconds(s.buffer_pause_seconds)+'</span></div>'+tank(s)+spark(row.tbuf,'#22d3ee')+'<div class="legend"><span>已生成 '+seconds(s.generated_seconds)+'</span><span>片长 '+seconds(s.runtime_seconds)+'</span><span>恢复 '+seconds(s.buffer_resume_seconds)+'</span></div></div></div></div>';
 }).join(''):'<div class="card empty">当前没有转码任务</div>';
}
async function refresh(){try{const r=await fetch('/emby_transcoder/api/status',{cache:'no-store'});if(r.status===401){location.reload();return}if(!r.ok)throw new Error();render(await r.json())}catch(e){document.getElementById('updated').textContent='状态服务暂时不可用'}}
refresh();setInterval(refresh,1000);
</script></body></html>`
