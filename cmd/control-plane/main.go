package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"google.golang.org/grpc"

	"github.com/paul/flexctl/internal/agentpb"
	"github.com/paul/flexctl/internal/agentstream"
	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/db"
	"github.com/paul/flexctl/internal/envs"
	"github.com/paul/flexctl/internal/headscale"
	"github.com/paul/flexctl/internal/httperr"
	"github.com/paul/flexctl/internal/imagetemplates"
	"github.com/paul/flexctl/internal/nodes"
	"github.com/paul/flexctl/internal/policy"
	"github.com/paul/flexctl/internal/sshkeys"
	"github.com/paul/flexctl/internal/users"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	addr := os.Getenv("FLEX_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	dsn := os.Getenv("FLEX_DB_DSN")
	if dsn == "" {
		dsn = "postgres://flex:flex@localhost:5432/flex?sslmode=disable"
	}
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := db.Open(dbCtx, dsn)
	dbCancel()
	if err != nil {
		slog.Error("db open", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	secret := []byte(os.Getenv("FLEX_SESSION_SECRET"))
	if len(secret) < 32 {
		slog.Error("FLEX_SESSION_SECRET must be at least 32 bytes")
		os.Exit(1)
	}
	signer := auth.NewSessionSigner(secret)

	hsURL := os.Getenv("FLEX_HEADSCALE_URL")
	if hsURL == "" {
		hsURL = "http://localhost:8088"
	}
	hsKey := os.Getenv("FLEX_HEADSCALE_API_KEY")
	if hsKey == "" {
		slog.Error("FLEX_HEADSCALE_API_KEY is required (run `make headscale-init` to generate)")
		os.Exit(1)
	}
	hsClient := headscale.NewClient(hsURL, hsKey, 5*time.Second)
	pol := policy.New(pool, hsClient)

	initCtx, initCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := pol.Initialize(initCtx); err != nil {
		initCancel()
		slog.Error("policy initialize", "err", err)
		os.Exit(1)
	}
	initCancel()

	usersSvc := users.NewService(pool)
	usersH := users.NewHandlers(usersSvc, signer, pool, pol)
	nodesSvc := nodes.NewService(pool)
	nodesH := nodes.NewHandlers(nodesSvc)

	envsSvc := envs.NewService(pool)
	tplSvc := imagetemplates.NewService(pool)

	// Stub dispatcher — Task 12에서 agentstream 기반 구현으로 교체
	envDispatcher := &noopEnvDispatcher{}
	envsH := envs.NewHandlers(envsSvc, envDispatcher)

	imageTemplatesHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tpls, err := tplSvc.List(r.Context())
		if err != nil {
			slog.Error("image templates list", "err", err)
			httperr.Write(w, http.StatusInternalServerError, "internal error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tpls)
	})

	usersH.Mount(r)
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession(signer))
		usersH.MountAuthed(r)
		sshkeys.NewHandlers(sshkeys.NewService(pool)).Mount(r)
		nodesH.MountAuthed(r)
		envsH.Mount(r)
		r.Get("/v1/image-templates", imageTemplatesHandler)
	})
	nodesH.MountPublic(r)

	r.Get("/v1/health", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 1*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "application/json")
		if err := pool.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"db_down"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	grpcAddr := os.Getenv("FLEX_GRPC_ADDR")
	if grpcAddr == "" {
		grpcAddr = ":9090"
	}
	grpcLis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		slog.Error("grpc listen", "err", err, "addr", grpcAddr)
		os.Exit(1)
	}
	grpcSrv := grpc.NewServer()
	agentpb.RegisterAgentServer(grpcSrv, agentstream.NewServer(nodesSvc))
	go func() {
		slog.Info("grpc serving", "addr", grpcAddr)
		if err := grpcSrv.Serve(grpcLis); err != nil {
			slog.Error("grpc serve", "err", err)
			os.Exit(1)
		}
	}()

	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("control-plane listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "err", err)
	}
	grpcSrv.GracefulStop()
}

// noopEnvDispatcher는 Task 12에서 agentstream 기반 구현으로 교체된다.
type noopEnvDispatcher struct{}

func (noopEnvDispatcher) Create(_ context.Context, _ uuid.UUID) error { return nil }
func (noopEnvDispatcher) Stop(_ context.Context, _ uuid.UUID) error   { return nil }
func (noopEnvDispatcher) Start(_ context.Context, _ uuid.UUID) error  { return nil }
func (noopEnvDispatcher) Delete(_ context.Context, _ uuid.UUID) error { return nil }
