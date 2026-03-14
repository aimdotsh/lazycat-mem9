package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/f00700f/lazycat-mem9/internal/domain"
	"github.com/f00700f/lazycat-mem9/internal/embed"
	"github.com/f00700f/lazycat-mem9/internal/llm"
	"github.com/f00700f/lazycat-mem9/internal/metrics"
	"github.com/f00700f/lazycat-mem9/internal/middleware"
	"github.com/f00700f/lazycat-mem9/internal/repository"
	"github.com/f00700f/lazycat-mem9/internal/service"
)

// Server holds the HTTP handlers and their dependencies.
type Server struct {
	tenant      *service.TenantService
	uploadTasks repository.UploadTaskRepo
	uploadDir   string
	embedder    *embed.Embedder
	llmClient   *llm.Client
	autoModel   string
	ftsEnabled  bool
	ingestMode  service.IngestMode
	dbBackend   string
	apiKey      string
	logger      *slog.Logger
	svcCache    sync.Map
}

// NewServer creates a new HTTP handler server.
func NewServer(
	tenantSvc *service.TenantService,
	uploadTasks repository.UploadTaskRepo,
	uploadDir string,
	embedder *embed.Embedder,
	llmClient *llm.Client,
	autoModel string,
	ftsEnabled bool,
	ingestMode service.IngestMode,
	dbBackend string,
	apiKey string,
	logger *slog.Logger,
) *Server {
	return &Server{
		tenant:      tenantSvc,
		uploadTasks: uploadTasks,
		uploadDir:   uploadDir,
		embedder:    embedder,
		llmClient:   llmClient,
		autoModel:   autoModel,
		ftsEnabled:  ftsEnabled,
		ingestMode:  ingestMode,
		dbBackend:   dbBackend,
		apiKey:      apiKey,
		logger:      logger,
	}
}

func currentBaseURL(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme = forwarded
	}
	host := r.Host
	if forwardedHost := r.Header.Get("X-Forwarded-Host"); forwardedHost != "" {
		host = forwardedHost
	}
	return scheme + "://" + host
}

// resolvedSvc holds the correct service instances for a request.
// Services are always backed by the tenant's dedicated DB.
type resolvedSvc struct {
	memory *service.MemoryService
	ingest *service.IngestService
}

type tenantSvcKey string

// resolveServices returns the correct services for a request.
func (s *Server) resolveServices(auth *domain.AuthInfo) resolvedSvc {
	if auth.TenantID == "" {
		key := tenantSvcKey(fmt.Sprintf("db-%p", auth.TenantDB))
		if cached, ok := s.svcCache.Load(key); ok {
			return cached.(resolvedSvc)
		}
		memRepo := repository.NewMemoryRepo(s.dbBackend, auth.TenantDB, s.autoModel, s.ftsEnabled)
		svc := resolvedSvc{
			memory: service.NewMemoryService(memRepo, s.llmClient, s.embedder, s.autoModel, s.ingestMode),
			ingest: service.NewIngestService(memRepo, s.llmClient, s.embedder, s.autoModel, s.ingestMode),
		}
		s.svcCache.Store(key, svc)
		return svc
	}
	key := tenantSvcKey(fmt.Sprintf("%s-%p", auth.TenantID, auth.TenantDB))
	if cached, ok := s.svcCache.Load(key); ok {
		return cached.(resolvedSvc)
	}
	memRepo := repository.NewMemoryRepo(s.dbBackend, auth.TenantDB, s.autoModel, s.ftsEnabled)
	svc := resolvedSvc{
		memory: service.NewMemoryService(memRepo, s.llmClient, s.embedder, s.autoModel, s.ingestMode),
		ingest: service.NewIngestService(memRepo, s.llmClient, s.embedder, s.autoModel, s.ingestMode),
	}
	s.svcCache.Store(key, svc)
	return svc
}

// Router builds the chi router with all routes and middleware.
func (s *Server) Router(
	tenantMW func(http.Handler) http.Handler,
	rateLimitMW func(http.Handler) http.Handler,
	apiKeyMW func(http.Handler) http.Handler,
) http.Handler {
	r := chi.NewRouter()

	// Global middleware.
	r.Use(chimw.Recoverer)
	r.Use(chimw.RequestID)
	r.Use(requestLogger(s.logger))
	r.Use(rateLimitMW)
	r.Use(metrics.Middleware)

	// Health check.
	r.Get("/", s.homepage)
	r.Get("/SKILL.md", s.skillDoc)
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		respond(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.Get("/metrics", promhttp.Handler().ServeHTTP)

	// Provision a new tenant — no auth, no body.
	r.Post("/v1alpha1/mem9s", s.provisionMem9s)

	// Tenant-scoped routes — tenantMW resolves {tenantID} to DB connection.
	r.Route("/v1alpha1/mem9s/{tenantID}", func(r chi.Router) {
		r.Use(tenantMW)

		// Memory CRUD.
		r.Post("/memories", s.createMemory)
		r.Get("/memories", s.listMemories)
		r.Get("/memories/{id}", s.getMemory)
		r.Put("/memories/{id}", s.updateMemory)
		r.Delete("/memories/{id}", s.deleteMemory)

		// Imports (async file ingest).
		r.Post("/imports", s.createTask)
		r.Get("/imports", s.listTasks)
		r.Get("/imports/{id}", s.getTask)

	})

	r.Route("/v1alpha2/mem9s", func(r chi.Router) {
		r.Use(apiKeyMW)

		r.Post("/memories", s.createMemory)
		r.Get("/memories", s.listMemories)
		r.Get("/memories/{id}", s.getMemory)
		r.Put("/memories/{id}", s.updateMemory)
		r.Delete("/memories/{id}", s.deleteMemory)

		r.Post("/imports", s.createTask)
		r.Get("/imports", s.listTasks)
		r.Get("/imports/{id}", s.getTask)
	})

	return r
}

func (s *Server) homepage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	baseURL := html.EscapeString(currentBaseURL(r))
	apiKeyValue := html.EscapeString(s.apiKey)
	apiKeyHeader := html.EscapeString(middleware.APIKeyHeader)
	agentIDHeader := html.EscapeString(middleware.AgentIDHeader)
	page := strings.NewReplacer(
		"__BASE_URL__", baseURL,
		"__API_KEY__", apiKeyValue,
		"__API_KEY_HEADER__", apiKeyHeader,
		"__AGENT_ID_HEADER__", agentIDHeader,
	).Replace(`<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>mem9</title>
  <style>
    :root { color-scheme: light; }
    * { box-sizing: border-box; }
    body { margin: 0; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background:
      radial-gradient(circle at top left, #dfead8 0, transparent 28%),
      linear-gradient(180deg, #f7f7f2 0%%, #eef1e7 100%%); color: #18230f; }
    main { max-width: 1080px; margin: 0 auto; padding: 40px 20px 72px; }
    h1 { margin: 0 0 12px; font-size: 46px; line-height: 1.02; letter-spacing: -0.03em; }
    h2 { margin: 0 0 14px; font-size: 22px; }
    p { line-height: 1.6; margin: 0 0 10px; }
    .lead { max-width: 760px; color: #3f4b34; }
    .grid { display: grid; grid-template-columns: 3fr 7fr; gap: 20px; margin-top: 24px; align-items: start; }
    .stack { display: grid; gap: 20px; }
    .card { background: rgba(255, 253, 248, .9); backdrop-filter: blur(10px); border: 1px solid #d9dfcf; border-radius: 18px; padding: 20px; box-shadow: 0 10px 34px rgba(24,35,15,.08); }
    .badge { display: inline-block; padding: 6px 10px; border-radius: 999px; background: #18230f; color: #f4f6ee; font-size: 12px; letter-spacing: .08em; text-transform: uppercase; }
    .top-status { display: flex; align-items: center; gap: 12px; margin-bottom: 0.8rem; }
    .row { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
    label { display: block; font-size: 13px; font-weight: 600; margin-bottom: 6px; color: #304126; }
    input, textarea, select, button { font: inherit; }
    input, textarea, select { width: 100%%; padding: 12px 14px; border-radius: 12px; border: 1px solid #c9d3bd; background: #fbfcf8; color: #18230f; }
    textarea { min-height: 120px; resize: vertical; }
    button { border: 0; border-radius: 12px; padding: 12px 16px; background: #2f6f3e; color: white; font-weight: 700; cursor: pointer; }
    button.secondary { background: #e8edde; color: #24321d; }
    .actions { display: flex; gap: 10px; flex-wrap: wrap; margin-top: 14px; }
    code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
    pre { overflow: auto; white-space: pre-wrap; word-break: break-word; overflow-wrap: anywhere; background: linear-gradient(135deg, #e0f1ff, #c6e2fd); color: #1d3b5f; padding: 16px; border-radius: 12px; margin: 0; }
    .result { min-height: 220px; }
    .muted { color: #5c6752; font-size: 14px; }
    .list { display: grid; gap: 10px; margin-top: 14px; }
    .item { padding: 12px; border-radius: 12px; background: #f5f8f0; border: 1px solid #d8e0ce; }
    .item strong { display: block; margin-bottom: 6px; }
    .item-actions { display: flex; gap: 8px; margin-top: 10px; flex-wrap: wrap; }
    .mini { padding: 8px 10px; border-radius: 10px; font-size: 13px; }
    .status-ok { color: #1c6a37; font-weight: 700; }
    .status-bad { color: #aa2f2f; font-weight: 700; }
    .status-info { color: #46613a; font-weight: 700; }
    @media (max-width: 900px) {
      .grid { grid-template-columns: 1fr; }
      .row { grid-template-columns: 1fr; }
      h1 { font-size: 38px; }
    }
  </style>
</head>
<body>
  <main>
    <div class="top-status">
      <span class="badge">mem9 control panel</span>
      <span id="globalStatus" class="status-ok" aria-live="polite">Healthy</span>
    </div>
    <h1>Shared memory for agents, now with a built-in console.</h1>
    <p class="lead">这个实例运行在懒猫微服 + MySQL 单机模式。你可以直接在这里检查健康状态、写入记忆、搜索记忆，并查看原始 JSON 返回。</p>

    <div class="grid">
      <div class="stack">
        <section class="card">
          <h2>Connection</h2>
          <div class="row">
            <div>
              <label for="apiKey">API Key</label>
              <input id="apiKey" placeholder="安装时填写的 API Key">
            </div>
            <div>
              <label for="agentId">Agent ID <span style="font-size:12px; color:#5c6752;">(used for logging, optional)</span></label>
              <input id="agentId" value="web-console">
            </div>
          </div>
          <p class="muted">请求头使用 <code>%s</code> 和 <code>%s</code>。</p>
          <div class="actions">
            <button id="healthBtn" class="secondary" type="button">Check API Key</button>
          </div>
          <div class="actions">
            <span id="connText" class="muted">API connection not checked yet.</span>
          </div>
        </section>

        <section class="card">
          <h2>Create Memory</h2>
          <label for="content">Content</label>
          <textarea id="content" placeholder="例如：用户偏好先本地 build，再推镜像到懒猫仓库"></textarea>
          <div class="row">
            <div>
              <label for="tags">Tags</label>
              <input id="tags" placeholder="lazycat,docker">
            </div>
            <div>
              <label for="metadata">Metadata JSON</label>
              <input id="metadata" value='{"source":"web-console"}'>
            </div>
          </div>
          <div class="actions">
            <button id="createBtn" type="button">Create</button>
            <span id="createText" class="muted">Create requires API Key and is processed asynchronously.</span>
          </div>
        </section>

        <section class="card">
          <h2>Search Memories</h2>
          <div class="row">
            <div>
              <label for="query">Query</label>
              <input id="query" placeholder="lazycat">
            </div>
            <div>
              <label for="limit">Limit</label>
              <select id="limit">
                <option value="10">10</option>
                <option value="20">20</option>
                <option value="50">50</option>
              </select>
            </div>
          </div>
          <div class="actions">
            <button id="searchBtn" type="button">Search</button>
            <button id="recentBtn" class="secondary" type="button">Load Recent</button>
            <span id="searchText" class="muted">Search requires API Key.</span>
          </div>
          <div id="memoryList" class="list"></div>
        </section>
      </div>

      <div class="stack">
        <section class="card">
          <h2>Raw Response</h2>
          <pre id="result" class="result">Ready.</pre>
        </section>

        <section class="card">
          <h2>API Notes</h2>
          <p><code>GET /healthz</code> health check</p>
          <p><code>POST /v1alpha2/mem9s/memories</code> create memory</p>
          <p><code>GET /v1alpha2/mem9s/memories?q=lazycat</code> search memories</p>
          <p><code>GET /v1alpha2/mem9s/memories/{id}</code> fetch one memory</p>
          <p><code>DELETE /v1alpha2/mem9s/memories/{id}</code> delete one memory</p>
          <p class="muted">搜索参数名是 <code>q</code>，不是 <code>query</code>。</p>
        </section>

        <section class="card">
          <h2>OpenClaw Config</h2>
          <pre id="openclawConfig">Fill API Key to generate config.</pre>
          <div class="actions">
            <button id="copyConfigBtn" class="secondary" type="button">Copy Config</button>
          </div>
        </section>

        <section class="card">
          <h2>Agent Prompt</h2>
          <p class="muted">把下面这段让能够阅读文档的智能体自动执行。</p>
          <pre id="agentPrompt">阅读 __BASE_URL__/SKILL.md ，按照说明为 OpenClaw 安装并配置 mem9</pre>
        </section>

        <section class="card">
          <h2>curl Example</h2>
          <pre id="curlExample">Waiting for API Key...</pre>
        </section>
      </div>
    </div>

    <section class="card" style="margin-top:20px;">
      <h2>本项目使用说明</h2>
      <p>这是一个运行在懒猫微服上的 mem9 单机版，目标是给一套 OpenClaw 提供持久化、可同步的共享记忆。</p>
      <p>当前模式是单记忆池：一个 API Key 对应一套共享记忆。多个 OpenClaw 实例使用同一个 <code>apiUrl + apiKey</code> 时，会同步到同一个记忆池。</p>
      <p>OpenClaw 接入地址：<code>__BASE_URL__</code>。推荐把首页右侧生成的 <code>openclaw.json</code> 片段直接复制到项目根目录配置中。</p>
      <p>当前版本支持：健康检查、记忆创建、关键词搜索、查看单条、删除、OpenClaw 配置生成、SKILL.md 说明页。</p>
      <p>当前版本不包含官方 mem9.ai 的完整 <code>your-memory</code> 前端，也不做多 tenant 管理；重点是稳定支撑一套 OpenClaw 共享记忆。</p>
      <p>常用接口：</p>
      <p><code>GET /healthz</code></p>
      <p><code>POST /v1alpha2/mem9s/memories</code></p>
      <p><code>GET /v1alpha2/mem9s/memories?q=keyword</code></p>
      <p><code>GET /v1alpha2/mem9s/memories/{id}</code></p>
      <p><code>DELETE /v1alpha2/mem9s/memories/{id}</code></p>
    </section>
  </main>
  <script>
    (function () {
      var apiKeyInput = document.getElementById('apiKey');
      var agentIdInput = document.getElementById('agentId');
      var connText = document.getElementById('connText');
      var createText = document.getElementById('createText');
      var searchText = document.getElementById('searchText');
      var result = document.getElementById('result');
      var memoryList = document.getElementById('memoryList');
      var createBtn = document.getElementById('createBtn');
      var searchBtn = document.getElementById('searchBtn');
      var recentBtn = document.getElementById('recentBtn');
      var healthBtn = document.getElementById('healthBtn');
      var openclawConfig = document.getElementById('openclawConfig');
      var curlExample = document.getElementById('curlExample');
      var globalStatus = document.getElementById('globalStatus');
      var copyConfigBtn = document.getElementById('copyConfigBtn');
      var configValid = false;
      var agentPrompt = document.getElementById('agentPrompt');
      var apiKeyHeader = "__API_KEY_HEADER__";
      var agentIdHeader = "__AGENT_ID_HEADER__";
      var validateTimer = null;

      try {
        var saved = localStorage.getItem('mem9_api_key');
        if (saved) {
          apiKeyInput.value = saved;
        }
      } catch (e) {}

      function setStatus(node, text, className) {
        node.textContent = text;
        node.className = className || 'muted';
      }

      function show(data) {
        result.textContent = typeof data === 'string' ? data : JSON.stringify(data, null, 2);
      }

      function buildHeaders(withJSON) {
        var apiKey = apiKeyInput.value.replace(/^\\s+|\\s+$/g, '');
        if (!apiKey) {
          throw new Error('API Key is required');
        }
        try {
          localStorage.setItem('mem9_api_key', apiKey);
        } catch (e) {}
        var h = {};
        h[apiKeyHeader] = apiKey;
        h[agentIdHeader] = agentIdInput.value.replace(/^\\s+|\\s+$/g, '') || 'web-console';
        if (withJSON) {
          h['Content-Type'] = 'application/json';
        }
        return h;
      }

      function renderConfig(apiKey, valid) {
        configValid = valid;
        if (!valid) {
          openclawConfig.textContent = 'API Key 无效或未校验，复制前请先确认连接状态。';
          copyConfigBtn.disabled = true;
        } else {
          openclawConfig.textContent = JSON.stringify({
            plugins: {
              slots: { memory: 'openclaw' },
              entries: {
                openclaw: {
                  enabled: true,
                  config: {
                    apiUrl: window.location.origin,
                    apiKey: apiKey
                  }
                }
              }
            }
          }, null, 2);
          copyConfigBtn.disabled = false;
        }
      }

      function updateOpenClawConfig() {
        var apiKey = apiKeyInput.value.replace(/^\\s+|\\s+$/g, '');
        var effectiveApiKey = apiKey || '<your-api-key>';
        renderConfig(effectiveApiKey, configValid);
        curlExample.textContent =
          "curl -X POST '" + window.location.origin + "/v1alpha2/mem9s/memories' \\\n" +
          "  -H 'Content-Type: application/json' \\\n" +
          "  -H 'X-API-Key: " + effectiveApiKey + "' \\\n" +
          "  -H 'X-Mnemo-Agent-Id: curl-demo' \\\n" +
          "  -d '{\"content\":\"用户偏好优先选择懒猫可移植项目\",\"tags\":[\"lazycat\"]}'";
        agentPrompt.textContent =
          "阅读 " + window.location.origin + "/SKILL.md ，按照说明为 OpenClaw 安装并配置 mem9";
      }

      function renderMemories(items) {
        memoryList.innerHTML = '';
        if (!items || !items.length) {
          memoryList.innerHTML = '<div class="muted">No memories found.</div>';
          return;
        }
        items.forEach(function (item) {
          var div = document.createElement('div');
          div.className = 'item';
          div.innerHTML =
            '<strong>' + (item.content || '(empty)') + '</strong>' +
            '<div class="muted">id: ' + (item.id || '') + '</div>' +
            '<div class="muted">tags: ' + (((item.tags || []).join(', ')) || '-') + '</div>' +
            '<div class="item-actions">' +
            '<button class="mini secondary memory-action" type="button" data-action="get" data-id="' + (item.id || '') + '">View JSON</button>' +
            '<button class="mini secondary memory-action" type="button" data-action="delete" data-id="' + (item.id || '') + '">Delete</button>' +
            '</div>';
          memoryList.appendChild(div);
        });
      }

      function requestJSON(url, options) {
        return fetch(url, options).then(function (res) {
          return res.text().then(function (text) {
            var data;
            try {
              data = text ? JSON.parse(text) : {};
            } catch (e) {
              data = { raw: text };
            }
            return { ok: res.ok, status: res.status, data: data };
          });
        });
      }

      function validateApiKey() {
        var apiKey = apiKeyInput.value.replace(/^\s+|\s+$/g, '');
        if (!apiKey) {
          setStatus(connText, 'API Key not provided.', 'muted');
          renderConfig(apiKey, false);
          return;
        }
        setStatus(connText, 'Checking API key...', 'status-info');
        requestJSON('/v1alpha2/mem9s/memories?limit=1', { headers: buildHeaders(false) })
          .then(function (resp) {
            if (resp.ok) {
              setStatus(connText, 'Connected.', 'status-ok');
              renderConfig(apiKey, true);
            } else {
              setStatus(connText, resp.data.error || 'Invalid API key.', 'status-bad');
              renderConfig(apiKey, false);
            }
          })
          .catch(function (err) {
            setStatus(connText, 'API validation failed.', 'status-bad');
            renderConfig(apiKey, false);
            show({ error: String(err) });
          });
      }

      function checkHealth() {
        healthBtn.disabled = true;
        if (globalStatus) {
          setStatus(globalStatus, 'Checking health...', 'status-info');
        }
        requestJSON('/healthz', {})
          .then(function (resp) {
            show(resp.data);
            var healthLabel = resp.ok ? 'Healthy' : 'Unhealthy';
            if (globalStatus) {
              setStatus(globalStatus, healthLabel, resp.ok ? 'status-ok' : 'status-bad');
            }
          })
          .catch(function (err) {
            if (globalStatus) {
              setStatus(globalStatus, 'Unhealthy', 'status-bad');
            }
            show({ error: String(err) });
          })
          .finally(function () {
            healthBtn.disabled = false;
          });
      }

      function searchMemories(loadRecent) {
        var url = '/v1alpha2/mem9s/memories?limit=' + encodeURIComponent(document.getElementById('limit').value);
        if (!loadRecent) {
          url += '&q=' + encodeURIComponent(document.getElementById('query').value.replace(/^\\s+|\\s+$/g, ''));
        }
        setStatus(searchText, loadRecent ? 'Loading recent memories...' : 'Searching...', 'status-info');
        requestJSON(url, { headers: buildHeaders(false) })
          .then(function (resp) {
            show(resp.data);
            renderMemories(resp.data.memories || []);
            setStatus(searchText, resp.ok ? (loadRecent ? 'Recent memories loaded.' : 'Search completed.') : 'Search failed.', resp.ok ? 'status-ok' : 'status-bad');
          })
          .catch(function (err) {
            setStatus(searchText, 'Search failed.', 'status-bad');
            show({ error: String(err) });
          })
          .finally(function () {
            searchBtn.disabled = false;
            recentBtn.disabled = false;
          });
      }

      function createMemory() {
        createBtn.disabled = true;
        setStatus(createText, 'Submitting memory...', 'status-info');
        var metadataText = document.getElementById('metadata').value.replace(/^\\s+|\\s+$/g, '');
        var payload = {
          content: document.getElementById('content').value.replace(/^\\s+|\\s+$/g, ''),
          tags: document.getElementById('tags').value.split(',').map(function (s) { return s.replace(/^\\s+|\\s+$/g, ''); }).filter(Boolean)
        };
        if (metadataText) {
          payload.metadata = JSON.parse(metadataText);
        }
        requestJSON('/v1alpha2/mem9s/memories', {
          method: 'POST',
          headers: buildHeaders(true),
          body: JSON.stringify(payload)
        })
          .then(function (resp) {
            show(resp.data);
            if (resp.ok) {
              setStatus(createText, 'Memory accepted. Refreshing recent list...', 'status-ok');
              setTimeout(function () { searchMemories(true); }, 1000);
            } else {
              setStatus(createText, resp.data.error || 'Create failed.', 'status-bad');
            }
          })
          .catch(function (err) {
            setStatus(createText, 'Create failed.', 'status-bad');
            show({ error: String(err) });
          })
          .finally(function () {
            createBtn.disabled = false;
          });
      }

      function getMemory(id) {
        requestJSON('/v1alpha2/mem9s/memories/' + encodeURIComponent(id), { headers: buildHeaders(false) })
          .then(function (resp) { show(resp.data); })
          .catch(function (err) { show({ error: String(err) }); });
      }

      function deleteMemory(id) {
        requestJSON('/v1alpha2/mem9s/memories/' + encodeURIComponent(id), { method: 'DELETE', headers: buildHeaders(false) })
          .then(function (resp) {
            if (resp.status === 204) {
              show({ deleted: id });
            } else {
              show(resp.data);
            }
            searchMemories(true);
          })
          .catch(function (err) { show({ error: String(err) }); });
      }

      healthBtn.onclick = function () {
        validateApiKey();
        checkHealth();
      };
      createBtn.onclick = function () {
        try {
          createMemory();
        } catch (err) {
          setStatus(createText, 'Create failed.', 'status-bad');
          show({ error: String(err) });
          createBtn.disabled = false;
        }
      };
      searchBtn.onclick = function () {
        searchBtn.disabled = true;
        try {
          searchMemories(false);
        } catch (err) {
          setStatus(searchText, 'Search failed.', 'status-bad');
          show({ error: String(err) });
          searchBtn.disabled = false;
        }
      };
      recentBtn.onclick = function () {
        recentBtn.disabled = true;
        try {
          searchMemories(true);
        } catch (err) {
          setStatus(searchText, 'Load recent failed.', 'status-bad');
          show({ error: String(err) });
          recentBtn.disabled = false;
        }
      };
      memoryList.onclick = function (event) {
        var target = event.target;
        if (!target || !target.getAttribute) return;
        var action = target.getAttribute('data-action');
        var id = target.getAttribute('data-id');
        if (!action || !id) return;
        if (action === 'get') getMemory(id);
        if (action === 'delete') deleteMemory(id);
      };
      copyConfigBtn.onclick = function () {
        updateOpenClawConfig();
        if (navigator.clipboard && navigator.clipboard.writeText) {
          navigator.clipboard.writeText(openclawConfig.textContent)
            .then(function () { show({ copied: true, target: 'openclaw.json config' }); })
            .catch(function (err) { show({ error: 'Copy failed', detail: String(err) }); });
        } else {
          show({ error: 'Clipboard API unavailable' });
        }
      };
      apiKeyInput.oninput = function () {
        updateOpenClawConfig();
        if (validateTimer) {
          clearTimeout(validateTimer);
        }
        validateTimer = setTimeout(validateApiKey, 300);
      };

      updateOpenClawConfig();
      checkHealth();
      validateApiKey();
    })();
  </script>
</body>
</html>`)
	_, _ = fmt.Fprint(w, page)
}

func (s *Server) skillDoc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	baseURL := currentBaseURL(r)
	apiKeyValue := s.apiKey
	doc := strings.NewReplacer(
		"__BASE_URL__", baseURL,
		"__API_KEY__", apiKeyValue,
	).Replace(`---
name: mem9 (Lazycat edition)
version: 1.0.0
description: |
  配置并接入部署在懒猫微服上的 mem9 单实例。

  Use when users say:
  - "安装 mem9"
  - "配置长期记忆"
  - "同步 OpenClaw 记忆"
  - "连接 mem9"

keywords:
  - mem9
  - lazycat
  - openclaw
  - persistent memory
metadata:
  openclaw:
    emoji: "🧠"
---

# mem9 for OpenClaw

这是适配 __BASE_URL__ 的懒猫版 mem9 skill。

与官方 https://mem9.ai/SKILL.md 的区别：

- 本部署是单实例、单记忆池，不提供“创建新 API Key”流程。
- 固定使用当前部署时设置的 API Key："__API_KEY__"。
- 仍然使用与官方一致的 "v1alpha2" API、"X-API-Key" 请求头和 OpenClaw mem9 插件。
- 多个 OpenClaw 实例只要使用同一个 "apiUrl + apiKey"，就会共享同一套记忆。

## 使用时机

当用户要做以下事情时使用本 skill：

- 给 OpenClaw 增加长期记忆。
- 把本地记忆同步到云端。
- 让多台设备共享同一套 OpenClaw 记忆。
- 导入已有 "memory" / "session" 文件到 mem9。

## Definition of Done

任务完成前，以下条件必须全部满足：

1. mem9 插件已安装。
2. "openclaw.json" 已正确配置。
3. OpenClaw 已重启。
4. __BASE_URL__/v1alpha2/mem9s/memories 可访问。
5. 用户已经收到完整的接入说明、恢复说明和导入建议。

## Step 1 - 安装 mem9 插件

优先安装官方 mem9 插件；国内网络差时，可临时使用 npmmirror：

~~~bash
NPMJS_TIME="$(curl -o /dev/null -sS --connect-timeout 2 --max-time 6 -w '%{time_total}' https://registry.npmjs.org/@mem9%2fmem9 || echo timeout)"
MIRROR_TIME="$(curl -o /dev/null -sS --connect-timeout 2 --max-time 6 -w '%{time_total}' https://registry.npmmirror.com/@mem9%2fmem9 || echo timeout)"

if [ "$MIRROR_TIME" != "timeout" ] && { [ "$NPMJS_TIME" = "timeout" ] || awk "BEGIN { exit !($NPMJS_TIME > 2 && $MIRROR_TIME < $NPMJS_TIME) }"; }; then
  NPM_CONFIG_REGISTRY=https://registry.npmmirror.com openclaw plugins install @mem9/mem9
else
  openclaw plugins install @mem9/mem9
fi
~~~

安装完成后，告知用户：

> mem9 plugin installed. Next I need to configure OpenClaw and restart it so the plugin can load.

## Step 2 - 检查 OpenClaw 版本

~~~bash
openclaw --version
~~~

规则：

- 如果版本 ">= 2.2.0"，配置中建议带上 "plugins.allow"。
- 如果版本 "< 2.2.0"，可以不写 "plugins.allow"。
- 如果版本无法确定，先不要盲改配置，先确认用户当前 OpenClaw 版本。

## Step 3 - 配置 openclaw.json

### OpenClaw >= 2.2.0

如果已有 "openclaw.json"，优先用 merge-safe 的方式更新，而不是整文件覆盖：

~~~bash
jq --arg api_key "__API_KEY__" '
  .plugins.slots.memory = "mem9" |
  if (.plugins.entries.mem9.config // {}) == {}
  then
    .plugins.entries.mem9 = {
      enabled: true,
      config: {
        apiUrl: "__BASE_URL__",
        apiKey: $api_key
      }
    }
  else
    .plugins.entries.mem9.enabled = true |
    .plugins.entries.mem9.config.apiUrl = "__BASE_URL__" |
    .plugins.entries.mem9.config.apiKey = $api_key
  end |
  .plugins.allow = ((.plugins.allow // []) + ["mem9"] | unique)
' openclaw.json > tmp.json && mv tmp.json openclaw.json
~~~

如果文件不存在，可创建：

~~~json
{
  "plugins": {
    "slots": { "memory": "mem9" },
    "allow": ["mem9"],
    "entries": {
      "mem9": {
        "enabled": true,
        "config": {
          "apiUrl": "__BASE_URL__",
          "apiKey": "__API_KEY__"
        }
      }
    }
  }
}
~~~

### OpenClaw < 2.2.0

~~~bash
jq --arg api_key "__API_KEY__" '
  .plugins.slots.memory = "mem9" |
  if (.plugins.entries.mem9.config // {}) == {}
  then
    .plugins.entries.mem9 = {
      enabled: true,
      config: {
        apiUrl: "__BASE_URL__",
        apiKey: $api_key
      }
    }
  else
    .plugins.entries.mem9.enabled = true |
    .plugins.entries.mem9.config.apiUrl = "__BASE_URL__" |
    .plugins.entries.mem9.config.apiKey = $api_key
  end
' openclaw.json > tmp.json && mv tmp.json openclaw.json
~~~

## Step 4 - 重启 OpenClaw

配置写入后，必须重启 OpenClaw，让 mem9 插件重新加载。

在自动化环境里，先发消息通知用户，再执行重启，避免重启导致消息没送达。

## Step 5 - 验证接入

先验证服务本身：

~~~bash
curl -sf __BASE_URL__/healthz && echo OK
~~~

再验证 API Key：

~~~bash
curl -sf \
  -H "X-API-Key: __API_KEY__" \
  -H "X-Mnemo-Agent-Id: openclaw-verify" \
  "__BASE_URL__/v1alpha2/mem9s/memories?limit=1" \
  && echo "API OK" || echo "API FAILED"
~~~

"API OK" 即视为接入成功。空结果也是成功，不代表异常，只说明当前记忆池还没有内容。

## Step 6 - 导入历史记忆

如果用户说“import memories to mem9”，按下面规则处理：

1. 扫描最近的本地记忆/会话文件。
2. 默认导入最近 15 个文件。
3. 可并行上传。

推荐扫描路径：

~~~text
./memory.json
./memories.json
./memories/*.json
./sessions/*.json
./session/*.json
~~~

读取 agent id：

~~~bash
AGENT_ID="$(jq -r '(.agents.list[0].id) // (.plugins.entries.mem9.config.agentName) // empty' openclaw.json 2>/dev/null)"
AGENT_FIELD=""
[ -n "$AGENT_ID" ] && AGENT_FIELD="-F agent_id=$AGENT_ID"
~~~

导入示例：

~~~bash
API="__BASE_URL__/v1alpha2/mem9s"
KEY="__API_KEY__"

curl -sX POST "$API/imports" \
  -H "X-API-Key: $KEY" \
  $AGENT_FIELD \
  -F "file=@memory.json" \
  -F "file_type=memory"
~~~

## 本部署与官方版的关键差异

### 1. 不创建新 API Key

官方版会调用：

~~~bash
curl -sX POST https://api.mem9.ai/v1alpha1/mem9s
~~~

懒猫版不要做这一步。这里没有自助 provision 流程，直接使用固定 API Key：

~~~text
__API_KEY__
~~~

### 2. API 写法基本相同

同步写法不变，仍然是：

- "POST /v1alpha2/mem9s/memories"
- "GET /v1alpha2/mem9s/memories"
- "POST /v1alpha2/mem9s/imports"
- Header: "X-API-Key"

差异只在于：

- base URL 变成 "__BASE_URL__"
- 当前只有一个共享 memory pool

### 3. 单实例共享记忆池

在这个部署里：

- "apiKey" 本质上就是唯一的记忆池标识。
- 不再需要向用户解释多 tenant 创建流程。
- 多个 OpenClaw 实例只要配置相同的 "apiUrl + apiKey"，就会自动同步到同一套记忆。

## API Reference

Base:

~~~text
__BASE_URL__
~~~

常用接口：

| Method | Path | Description |
| ------ | ---- | ----------- |
| GET | /healthz | 健康检查 |
| POST | /v1alpha2/mem9s/memories | 创建记忆 |
| GET | /v1alpha2/mem9s/memories | 搜索记忆 |
| GET | /v1alpha2/mem9s/memories/{id} | 获取单条 |
| PUT | /v1alpha2/mem9s/memories/{id} | 更新单条 |
| DELETE | /v1alpha2/mem9s/memories/{id} | 删除单条 |
| POST | /v1alpha2/mem9s/imports | 导入文件 |
| GET | /v1alpha2/mem9s/imports | 查看导入任务 |

## 常用示例

~~~bash
API="__BASE_URL__/v1alpha2/mem9s"
KEY="__API_KEY__"

curl -sX POST "$API/memories" \
  -H "Content-Type: application/json" \
  -H "X-API-Key: $KEY" \
  -H "X-Mnemo-Agent-Id: curl-demo" \
  -d '{"content":"用户偏好优先选择懒猫可移植项目","tags":["lazycat"]}'

curl -s \
  -H "X-API-Key: $KEY" \
  -H "X-Mnemo-Agent-Id: curl-demo" \
  "$API/memories?q=lazycat&limit=5"
~~~

## Troubleshooting

| Symptom | Fix |
| ------- | --- |
| 401 invalid API key | 确认 apiKey 使用的是当前部署时设置的值 "__API_KEY__"。 |
| 404 | 确认 URL 是 "__BASE_URL__/v1alpha2/..."，不要写成官方域名。 |
| 插件未加载 | 确认 plugins.slots.memory = "mem9"、plugins.entries.mem9.enabled = true，并重启 OpenClaw。 |
| 看不到旧记忆 | 确认所有实例都在用同一个 "apiUrl + apiKey"。 |

## 最终交付给用户的话术

完成接入后，应明确告知用户：

~~~text
mem9 已接入当前 OpenClaw。

当前使用的是懒猫版单实例 mem9：
- API URL: __BASE_URL__
- API Key: __API_KEY__

后续如果你想在另一台机器继续使用同一套记忆，只需要安装 mem9 插件，并写入相同的 apiUrl 和 apiKey。

如果你要把历史记忆导入到 mem9，我可以继续扫描本地 memory/session 文件并帮你上传。
~~~
`)
	_, _ = fmt.Fprint(w, doc)
}

// respond writes a JSON response.
func respond(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if data != nil {
		if err := json.NewEncoder(w).Encode(data); err != nil {
			slog.Error("failed to encode response", "err", err)
		}
	}
}

// respondError writes a JSON error response.
func respondError(w http.ResponseWriter, status int, msg string) {
	respond(w, status, map[string]string{"error": msg})
}

// handleError maps domain errors to HTTP status codes.
func (s *Server) handleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrWriteConflict):
		respondError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, domain.ErrConflict):
		respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, domain.ErrDuplicateKey):
		respondError(w, http.StatusConflict, "duplicate key: "+err.Error())
	case errors.Is(err, domain.ErrValidation):
		respondError(w, http.StatusBadRequest, err.Error())
	default:
		s.logger.Error("internal error", "err", err)
		respondError(w, http.StatusInternalServerError, "internal server error")
	}
}

// decode reads and JSON-decodes the request body.
func decode(r *http.Request, dst any) error {
	if r.Body == nil {
		return &domain.ValidationError{Message: "request body required"}
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		return &domain.ValidationError{Message: "invalid JSON: " + err.Error()}
	}
	return nil
}

// authInfo extracts AuthInfo from context.
func authInfo(r *http.Request) *domain.AuthInfo {
	return middleware.AuthFromContext(r.Context())
}

// requestLogger returns a middleware that logs each request.
// It uses the chi route pattern (e.g. /v1alpha1/mem9s/{tenantID}/memories)
// instead of the raw URL path to avoid logging sensitive tenant IDs.
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			// Use route pattern to avoid exposing sensitive path params (e.g. tenantID).
			routeCtx := chi.RouteContext(r.Context())
			path := r.URL.Path
			if routeCtx != nil {
				if pattern := routeCtx.RoutePattern(); pattern != "" {
					path = pattern
				}
			}
			logger.Info("request",
				"method", r.Method,
				"path", path,
				"status", ww.Status(),
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", chimw.GetReqID(r.Context()),
			)
		})
	}
}
