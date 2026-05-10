package flexctlagent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/paul/flexctl/internal/agentpb"
)

// GPU mirrors agentpb.GPU but defined here to avoid coupling test code to proto.
type GPU struct {
	Index  int32
	Model  string
	VRAMMB int32
}

// GPUDetector returns the current GPU inventory. Implementations must be safe
// to call concurrently and return quickly.
type GPUDetector interface {
	Detect(ctx context.Context) []GPU
}

// StaticGPUs returns a GPUDetector that always returns the given inventory.
func StaticGPUs(gpus []GPU) GPUDetector { return staticGPUs(gpus) }

type staticGPUs []GPU

func (s staticGPUs) Detect(_ context.Context) []GPU { return []GPU(s) }

type Config struct {
	GRPCAddress       string        // host:port
	NodeToken         string        // long-lived auth token
	AgentVersion      string        // e.g. "0.1.0"
	HeartbeatInterval time.Duration // default 30s
	BackoffInitial    time.Duration // default 1s
	BackoffMax        time.Duration // default 60s
	GPUDetector       GPUDetector   // optional; nil → empty list
}

type Agent struct {
	cfg Config
}

func New(cfg Config) *Agent {
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 30 * time.Second
	}
	if cfg.BackoffInitial == 0 {
		cfg.BackoffInitial = time.Second
	}
	if cfg.BackoffMax == 0 {
		cfg.BackoffMax = 60 * time.Second
	}
	return &Agent{cfg: cfg}
}

// Run drives the connect→register→heartbeat loop. It returns only when ctx is
// canceled. On every disconnect it sleeps with jittered backoff and retries.
func (a *Agent) Run(ctx context.Context) error {
	backoff := a.cfg.BackoffInitial
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := a.runOnce(ctx)
		if err != nil {
			slog.Warn("agent stream ended", "err", err)
		}
		// Sleep with jitter; cap at BackoffMax.
		delay := withJitter(backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		backoff = nextBackoff(backoff, a.cfg.BackoffMax)
		// On a successful connection (runOnce returned nil), reset backoff.
		// We can't tell precisely from err alone; treat nil err as healthy.
		if err == nil {
			backoff = a.cfg.BackoffInitial
		}
	}
}

func (a *Agent) runOnce(ctx context.Context) error {
	conn, err := grpc.NewClient(a.cfg.GRPCAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	client := agentpb.NewAgentClient(conn)
	md := metadata.Pairs("node-token", a.cfg.NodeToken)
	streamCtx, cancel := context.WithCancel(metadata.NewOutgoingContext(ctx, md))
	defer cancel()

	stream, err := client.Stream(streamCtx)
	if err != nil {
		return err
	}

	// Send Register
	gpus := a.detectGPUs(ctx)
	if err := stream.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_Register{
			Register: &agentpb.Register{
				AgentVersion: a.cfg.AgentVersion,
				Gpus:         gpus,
			},
		},
	}); err != nil {
		return err
	}

	// Wait for RegisterAck
	if _, err := stream.Recv(); err != nil {
		return err
	}

	// Concurrently: heartbeat sender + ack receiver
	errCh := make(chan error, 2)
	go func() {
		ticker := time.NewTicker(a.cfg.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-streamCtx.Done():
				errCh <- streamCtx.Err()
				return
			case <-ticker.C:
				if err := stream.Send(&agentpb.AgentMessage{
					Payload: &agentpb.AgentMessage_Heartbeat{
						Heartbeat: &agentpb.Heartbeat{At: timestamppb.Now()},
					},
				}); err != nil {
					errCh <- err
					return
				}
			}
		}
	}()
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				if errors.Is(err, io.EOF) {
					errCh <- nil
				} else {
					errCh <- err
				}
				return
			}
		}
	}()

	// Wait for either side to terminate.
	return <-errCh
}

func (a *Agent) detectGPUs(ctx context.Context) []*agentpb.GPU {
	if a.cfg.GPUDetector == nil {
		return nil
	}
	gpus := a.cfg.GPUDetector.Detect(ctx)
	out := make([]*agentpb.GPU, 0, len(gpus))
	for _, g := range gpus {
		out = append(out, &agentpb.GPU{Index: g.Index, Model: g.Model, VramMb: g.VRAMMB})
	}
	return out
}

func nextBackoff(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		return max
	}
	return next
}

func withJitter(d time.Duration) time.Duration {
	// ±25% jitter
	jitter := time.Duration(rand.Int64N(int64(d) / 2)) //nolint:gosec — non-crypto
	return d/2 + jitter
}
