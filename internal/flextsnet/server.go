package flextsnet

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"tailscale.com/tsnet"
)

type Config struct {
	StateDir   string
	Hostname   string
	AuthKey    string
	ControlURL string
	Logf       func(format string, args ...any) // nil → io.Discard equivalent
}

// Start brings up an embedded Tailscale node in this process. Caller must
// Close when done. State is persisted under cfg.StateDir; existing state is
// reused (no new pre-auth key required on subsequent boots).
func Start(ctx context.Context, cfg Config) (*tsnet.Server, error) {
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	srv := &tsnet.Server{
		Dir:        cfg.StateDir,
		Hostname:   cfg.Hostname,
		AuthKey:    cfg.AuthKey,
		ControlURL: cfg.ControlURL,
		Ephemeral:  false,
		Logf:       logf,
	}
	if err := srv.Start(); err != nil {
		return nil, fmt.Errorf("tsnet start: %w", err)
	}
	if _, err := srv.Up(ctx); err != nil {
		_ = srv.Close()
		return nil, fmt.Errorf("tsnet up: %w", err)
	}
	return srv, nil
}

// Dial performs an in-process TCP dial via the embedded Tailscale node. host
// should be a tailnet hostname (MagicDNS) or IP, port a numeric string.
func Dial(ctx context.Context, srv *tsnet.Server, host, port string) (net.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return srv.Dial(dctx, "tcp", net.JoinHostPort(host, port))
}

// Pipe copies bidirectionally between conn and stdio. Returns when either
// side closes.
func Pipe(conn net.Conn, stdin io.Reader, stdout io.Writer) error {
	errCh := make(chan error, 2)
	go func() {
		_, err := io.Copy(conn, stdin)
		errCh <- err
		_ = conn.Close()
	}()
	go func() {
		_, err := io.Copy(stdout, conn)
		errCh <- err
	}()
	// Wait for one side to finish; the other will unblock from the Close above.
	err := <-errCh
	<-errCh
	return err
}
