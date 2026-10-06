// Command relay serves the PowerFleet Relay web UI and runs replay jobs.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
	_ "time/tzdata" // zone names work in the distroless image

	"github.com/A-REDA-EL/powerfleet-relay/internal/api"
	"github.com/A-REDA-EL/powerfleet-relay/internal/relay"
	"github.com/A-REDA-EL/powerfleet-relay/internal/secret"
	"github.com/A-REDA-EL/powerfleet-relay/internal/store"
	"github.com/A-REDA-EL/powerfleet-relay/internal/traccar"
	"github.com/A-REDA-EL/powerfleet-relay/web"
)

const cleanShutdownKey = "clean_shutdown"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	password := os.Getenv("RELAY_ADMIN_PASSWORD")
	if len(password) < 8 {
		return errors.New("RELAY_ADMIN_PASSWORD must be set (at least 8 characters)")
	}
	box, err := secret.New(os.Getenv("RELAY_SECRET_KEY"))
	if err != nil {
		return err
	}
	dataDir := env("RELAY_DATA_DIR", "/data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(dataDir, "relay.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	srv := &api.Server{Store: st, Box: box, AdminPassword: password, UI: web.FS(), Log: log}
	mgr := relay.NewManager(st, srv.OpenSource, log)
	srv.Manager = mgr

	ctx := context.Background()
	if err := bootstrapDatabase(ctx, srv, st, log); err != nil {
		return err
	}
	var clean bool
	_, _ = st.GetSetting(ctx, cleanShutdownKey, &clean)
	_ = st.PutSetting(ctx, cleanShutdownKey, false)
	mgr.ResumeInterrupted(ctx, clean)

	httpServer := &http.Server{
		Addr:              env("RELAY_LISTEN", ":8090"),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", httpServer.Addr)
		errc <- httpServer.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errc:
		return err
	case sig := <-stop:
		log.Info("shutting down", "signal", sig.String())
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	mgr.Shutdown(60 * time.Second)
	return st.PutSetting(ctx, cleanShutdownKey, true)
}

// bootstrapDatabase stores a Traccar connection from RELAY_DB_* variables if none is configured yet.
func bootstrapDatabase(ctx context.Context, srv *api.Server, st *store.Store, log *slog.Logger) error {
	if os.Getenv("RELAY_DB_USER") == "" {
		return nil
	}
	var existing map[string]any
	if ok, err := st.GetSetting(ctx, "traccar_db", &existing); err != nil || ok {
		return err
	}
	port, _ := strconv.Atoi(env("RELAY_DB_PORT", "3306"))
	cfg := traccar.Config{
		Mode: env("RELAY_DB_MODE", "tcp"), Host: os.Getenv("RELAY_DB_HOST"), Port: port,
		Socket: os.Getenv("RELAY_DB_SOCKET"), User: os.Getenv("RELAY_DB_USER"),
		Password: os.Getenv("RELAY_DB_PASSWORD"), Database: env("RELAY_DB_NAME", "traccar"),
		TimeZone: os.Getenv("RELAY_DB_TIMEZONE"),
	}
	log.Info("storing Traccar connection from environment", "mode", cfg.Mode, "host", cfg.Host, "socket", cfg.Socket)
	return srv.SaveDB(ctx, cfg)
}

// healthcheck is used by the container HEALTHCHECK (the image has no shell or curl).
func healthcheck() int {
	_, port, err := net.SplitHostPort(env("RELAY_LISTEN", ":8090"))
	if err != nil {
		return 1
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/api/health")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
