package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/db"
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

	usersSvc := users.NewService(pool)
	usersH := users.NewHandlers(usersSvc, signer, pool)
	usersH.Mount(r)
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession(signer))
		usersH.MountAuthed(r)
		sshkeys.NewHandlers(sshkeys.NewService(pool)).Mount(r)
	})

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
}
