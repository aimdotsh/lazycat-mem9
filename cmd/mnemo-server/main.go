package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/f00700f/lazycat-mem9/internal/domain"
	"github.com/f00700f/lazycat-mem9/internal/config"
	"github.com/f00700f/lazycat-mem9/internal/embed"
	"github.com/f00700f/lazycat-mem9/internal/handler"
	"github.com/f00700f/lazycat-mem9/internal/llm"
	"github.com/f00700f/lazycat-mem9/internal/middleware"
	"github.com/f00700f/lazycat-mem9/internal/repository"
	"github.com/f00700f/lazycat-mem9/internal/service"
	"github.com/f00700f/lazycat-mem9/internal/tenant"
)

type bootstrapState struct {
	mu      sync.RWMutex
	handler http.Handler
	initErr error
}

func (s *bootstrapState) setReady(handler http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handler = handler
	s.initErr = nil
}

func (s *bootstrapState) setError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initErr = err
}

func (s *bootstrapState) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	handler := s.handler
	initErr := s.initErr
	s.mu.RUnlock()

	if handler != nil {
		handler.ServeHTTP(w, r)
		return
	}
	if initErr != nil {
		serveBootstrapError(w, r, initErr)
		return
	}
	serveBootstrapLoading(w, r)
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	bootstrap := &bootstrapState{}
	rl := middleware.NewRateLimiter(cfg.RateLimit, cfg.RateBurst)
	defer rl.Stop()

	var (
		db           *sql.DB
		tenantPool   *tenant.TenantPool
		workerCancel context.CancelFunc = func() {}
	)

	httpSrv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      bootstrap,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		router, initDB, initPool, cancelWorker, err := buildReadyHandler(cfg, logger, rl)
		if err != nil {
			logger.Error("service bootstrap failed", "err", err)
			bootstrap.setError(err)
			return
		}
		db = initDB
		tenantPool = initPool
		workerCancel = cancelWorker
		bootstrap.setReady(router)
	}()

	// Graceful shutdown.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig)

		workerCancel()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			logger.Error("shutdown error", "err", err)
		}
		if tenantPool != nil {
			tenantPool.Close()
		}
		if db != nil {
			_ = db.Close()
		}
	}()

	logger.Info("starting mnemo server", "port", cfg.Port)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}
	logger.Info("server stopped")
}

func buildReadyHandler(
	cfg *config.Config,
	logger *slog.Logger,
	rl *middleware.RateLimiter,
) (http.Handler, *sql.DB, *tenant.TenantPool, context.CancelFunc, error) {
	db, err := openDatabaseWithRetry(cfg, logger)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	logger.Info("database connected", "backend", cfg.DBBackend, "flavor", cfg.DBFlavor)

	if cfg.DBBackend == "tidb" {
		if err := ensureControlPlaneSchema(context.Background(), db); err != nil {
			_ = db.Close()
			return nil, nil, nil, nil, err
		}
	}

	embedder := embed.New(embed.Config{
		APIKey:  cfg.EmbedAPIKey,
		BaseURL: cfg.EmbedBaseURL,
		Model:   cfg.EmbedModel,
		Dims:    cfg.EmbedDims,
	})
	if cfg.EmbedAutoModel != "" {
		if (cfg.DBBackend == "tidb" && cfg.DBFlavor != "mysql") || cfg.DBBackend == "db9" {
			logger.Info("auto-embedding enabled (EMBED_TEXT)", "model", cfg.EmbedAutoModel, "dims", cfg.EmbedAutoDims)
		} else {
			logger.Warn("auto-embedding (EMBED_TEXT) is only supported with TiDB or db9; clearing and falling back to client-side embedding", "model", cfg.EmbedAutoModel, "backend", cfg.DBBackend, "flavor", cfg.DBFlavor)
			cfg.EmbedAutoModel = ""
			cfg.EmbedAutoDims = 0
		}
	} else if embedder != nil {
		logger.Info("client-side embedding configured", "model", cfg.EmbedModel, "dims", cfg.EmbedDims)
	} else {
		logger.Info("no embedding configured, keyword-only search active")
	}
	if cfg.DBBackend == "tidb" && cfg.DBFlavor == "mysql" && embedder != nil {
		logger.Warn("plain MySQL mode disables semantic vector search; ignoring embedding provider configuration")
		embedder = nil
	}

	llmClient := llm.New(llm.Config{
		APIKey:      cfg.LLMAPIKey,
		BaseURL:     cfg.LLMBaseURL,
		Model:       cfg.LLMModel,
		Temperature: cfg.LLMTemperature,
	})
	if llmClient != nil {
		logger.Info("LLM configured for smart ingest", "model", cfg.LLMModel)
	} else {
		logger.Info("no LLM configured, ingest will use raw mode")
	}

	tenantRepo := repository.NewTenantRepo(cfg.DBBackend, db)
	uploadTaskRepo := repository.NewUploadTaskRepo(cfg.DBBackend, db)
	tenantPool := tenant.NewPool(tenant.PoolConfig{
		MaxIdle:     cfg.TenantPoolMaxIdle,
		MaxOpen:     cfg.TenantPoolMaxOpen,
		IdleTimeout: cfg.TenantPoolIdleTimeout,
		TotalLimit:  cfg.TenantPoolTotalLimit,
		Backend:     cfg.DBBackend,
	})

	var zeroClient *tenant.ZeroClient
	if cfg.TiDBZeroEnabled && cfg.DBBackend == "tidb" {
		zeroClient = tenant.NewZeroClient(cfg.TiDBZeroAPIURL)
	} else if cfg.TiDBZeroEnabled {
		logger.Warn("TiDB Zero provisioning is only supported with tidb backend; disabling auto-provisioning", "backend", cfg.DBBackend)
	}
	tenantSvc := service.NewTenantService(tenantRepo, zeroClient, tenantPool, logger, cfg.EmbedAutoModel, cfg.EmbedAutoDims, cfg.FTSEnabled, cfg.DBFlavor)

	if cfg.BootstrapTenantEnabled {
		if err := bootstrapSingleTenant(context.Background(), tenantSvc, tenantRepo, cfg, logger); err != nil {
			tenantPool.Close()
			_ = db.Close()
			return nil, nil, nil, nil, err
		}
	}

	tenantMW := middleware.ResolveTenant(tenantRepo, tenantPool)
	apiKeyMW := middleware.ResolveApiKey(tenantRepo, tenantPool)
	rateMW := rl.Middleware()

	srv := handler.NewServer(tenantSvc, uploadTaskRepo, cfg.UploadDir, embedder, llmClient, cfg.EmbedAutoModel, cfg.FTSEnabled, service.IngestMode(cfg.IngestMode), cfg.DBBackend, cfg.BootstrapTenantID, logger)
	router := srv.Router(tenantMW, rateMW, apiKeyMW)

	workerCtx, workerCancel := context.WithCancel(context.Background())
	uploadWorker := service.NewUploadWorker(
		uploadTaskRepo,
		tenantRepo,
		tenantPool,
		embedder,
		llmClient,
		cfg.EmbedAutoModel,
		cfg.FTSEnabled,
		service.IngestMode(cfg.IngestMode),
		logger,
		cfg.WorkerConcurrency,
	)
	go func() {
		if err := uploadWorker.Run(workerCtx); err != nil {
			logger.Error("upload worker error", "err", err)
		}
	}()

	return router, db, tenantPool, workerCancel, nil
}

func serveBootstrapLoading(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"starting"}`))
	case "/SKILL.md":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("# mem9 is starting\n\nThe service is still warming up. Refresh in a few seconds.\n"))
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="refresh" content="3">
  <title>lazycat-mem9 is starting</title>
  <style>
    body { margin: 0; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: linear-gradient(180deg, #f7f7f2, #eef1e7); color: #18230f; display: grid; place-items: center; min-height: 100vh; }
    .card { width: min(92vw, 560px); background: rgba(255,253,248,.92); border: 1px solid #d9dfcf; border-radius: 18px; padding: 28px; box-shadow: 0 10px 34px rgba(24,35,15,.08); }
    h1 { margin: 0 0 12px; font-size: 28px; }
    p { line-height: 1.6; color: #3f4b34; }
  </style>
</head>
<body>
  <div class="card">
    <h1>lazycat-mem9 正在启动</h1>
    <p>数据库和应用仍在初始化中。页面会在 3 秒后自动刷新，无需手动反复刷新。</p>
  </div>
</body>
</html>`))
	}
}

func serveBootstrapError(w http.ResponseWriter, r *http.Request, err error) {
	switch r.URL.Path {
	case "/healthz":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"status":"error","error":%q}`, err.Error())))
	case "/SKILL.md":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("# mem9 failed to start\n\n" + err.Error() + "\n"))
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("<!doctype html><html lang=\"zh-CN\"><body><h1>lazycat-mem9 启动失败</h1><p>" + err.Error() + "</p></body></html>"))
	}
}

func ensureControlPlaneSchema(ctx context.Context, db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS tenants (
			id               VARCHAR(36)  PRIMARY KEY,
			name             VARCHAR(255) NOT NULL,
			db_host          VARCHAR(255) NOT NULL,
			db_port          INT          NOT NULL,
			db_user          VARCHAR(255) NOT NULL,
			db_password      VARCHAR(255) NOT NULL,
			db_name          VARCHAR(255) NOT NULL,
			db_tls           TINYINT(1)   NOT NULL DEFAULT 0,
			provider         VARCHAR(50)  NOT NULL,
			cluster_id       VARCHAR(255) NULL,
			claim_url        TEXT         NULL,
			claim_expires_at TIMESTAMP    NULL,
			status           VARCHAR(20)  NOT NULL DEFAULT 'provisioning',
			schema_version   INT          NOT NULL DEFAULT 1,
			created_at       TIMESTAMP    DEFAULT CURRENT_TIMESTAMP,
			updated_at       TIMESTAMP    DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			deleted_at       TIMESTAMP    NULL,
			UNIQUE INDEX idx_tenant_name (name),
			INDEX idx_tenant_status (status),
			INDEX idx_tenant_provider (provider)
		)`,
		`CREATE TABLE IF NOT EXISTS upload_tasks (
			task_id       VARCHAR(36)  PRIMARY KEY,
			tenant_id     VARCHAR(36)  NOT NULL,
			file_name     VARCHAR(255) NOT NULL,
			file_path     TEXT         NOT NULL,
			agent_id      VARCHAR(100) NULL,
			session_id    VARCHAR(100) NULL,
			file_type     VARCHAR(20)  NOT NULL,
			total_chunks  INT          NOT NULL DEFAULT 0,
			done_chunks   INT          NOT NULL DEFAULT 0,
			status        VARCHAR(20)  NOT NULL DEFAULT 'pending',
			error_msg     TEXT         NULL,
			created_at    TIMESTAMP    DEFAULT CURRENT_TIMESTAMP,
			updated_at    TIMESTAMP    DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			INDEX idx_upload_tenant (tenant_id),
			INDEX idx_upload_poll (status, created_at)
		)`,
	}
	for _, stmt := range statements {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func bootstrapSingleTenant(
	ctx context.Context,
	tenantSvc *service.TenantService,
	tenantRepo repository.TenantRepo,
	cfg *config.Config,
	logger *slog.Logger,
) error {
	if _, err := tenantRepo.GetByID(ctx, cfg.BootstrapTenantID); err == nil {
		logger.Info("default tenant already exists", "tenant_id", cfg.BootstrapTenantID)
		return nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("check default tenant: %w", err)
	}

	t := &domain.Tenant{
		ID:            cfg.BootstrapTenantID,
		Name:          cfg.BootstrapTenantName,
		DBHost:        cfg.BootstrapTenantDBHost,
		DBPort:        cfg.BootstrapTenantDBPort,
		DBUser:        cfg.BootstrapTenantDBUser,
		DBPassword:    cfg.BootstrapTenantDBPass,
		DBName:        cfg.BootstrapTenantDBName,
		DBTLS:         cfg.BootstrapTenantDBTLS,
		Provider:      cfg.DBFlavor,
		Status:        domain.TenantProvisioning,
		SchemaVersion: 0,
	}
	if err := tenantRepo.Create(ctx, t); err != nil {
		return fmt.Errorf("create default tenant: %w", err)
	}
	if err := tenantSvc.InitSchemaForTenant(ctx, t); err != nil {
		return fmt.Errorf("init default tenant schema: %w", err)
	}
	if err := tenantRepo.UpdateStatus(ctx, t.ID, domain.TenantActive); err != nil {
		return fmt.Errorf("activate default tenant: %w", err)
	}
	if err := tenantRepo.UpdateSchemaVersion(ctx, t.ID, 1); err != nil {
		return fmt.Errorf("set default tenant schema version: %w", err)
	}
	logger.Info("default tenant bootstrapped", "tenant_id", t.ID, "provider", t.Provider)
	return nil
}

func openDatabaseWithRetry(cfg *config.Config, logger *slog.Logger) (*sql.DB, error) {
	var lastErr error
	maxRetries := cfg.DBConnectMaxRetries
	if maxRetries <= 0 {
		maxRetries = 30
	}
	delay := cfg.DBConnectRetryDelay
	if delay <= 0 {
		delay = 2 * time.Second
	}
	for attempt := 1; attempt <= maxRetries; attempt++ {
		db, err := repository.NewDB(cfg.DBBackend, cfg.DSN)
		if err == nil {
			return db, nil
		}
		lastErr = err
		logger.Warn("database not ready, retrying", "attempt", attempt, "max_retries", maxRetries, "retry_delay", delay.String(), "err", err)
		time.Sleep(delay)
	}
	return nil, fmt.Errorf("open database after retries: %w", lastErr)
}
