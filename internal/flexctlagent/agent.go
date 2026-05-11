package flexctlagent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/paul/flexctl/internal/agentpb"
	"github.com/paul/flexctl/internal/envdocker"
	"github.com/paul/flexctl/internal/envlifecycle"
)

// lockedClientStream wraps Agent_StreamClient.Send with a mutex so the
// heartbeat goroutine and envlifecycle.Dispatcher can both send concurrently.
type lockedClientStream struct {
	mu sync.Mutex
	s  agentpb.Agent_StreamClient
}

func (l *lockedClientStream) Send(msg *agentpb.AgentMessage) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.s.Send(msg)
}

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
	DockerClient      envdocker.DockerClient
	GPUAllocator      *envlifecycle.GPUAllocator
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
	ls := &lockedClientStream{s: stream}

	// Send Register
	gpus := a.detectGPUs(ctx)
	if err := ls.Send(&agentpb.AgentMessage{
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

	// Build and send EnvStateSnapshot if docker/allocator are configured.
	var disp *envlifecycle.Dispatcher
	if a.cfg.DockerClient != nil && a.cfg.GPUAllocator != nil {
		disp = envlifecycle.NewDispatcher(a.cfg.DockerClient, a.cfg.GPUAllocator, ls)
		envIDs, err := disp.BuildEnvStateSnapshot(streamCtx)
		if err == nil {
			_ = ls.Send(&agentpb.AgentMessage{
				Payload: &agentpb.AgentMessage_EnvSnapshot{
					EnvSnapshot: &agentpb.EnvStateSnapshot{RunningEnvIds: envIDs},
				},
			})
		}
	}

	// Concurrently: heartbeat sender + control-message receiver
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
				if err := ls.Send(&agentpb.AgentMessage{
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
			msg, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					errCh <- nil
				} else {
					errCh <- err
				}
				return
			}
			if disp != nil {
				switch p := msg.GetPayload().(type) {
				case *agentpb.ControlMessage_CreateEnv:
					go func(cmd *agentpb.CreateEnv) { _ = disp.HandleCreate(context.Background(), cmd) }(p.CreateEnv)
				case *agentpb.ControlMessage_StopEnv:
					go func(cmd *agentpb.StopEnv) { _ = disp.HandleStop(context.Background(), cmd) }(p.StopEnv)
				case *agentpb.ControlMessage_StartEnv:
					go func(cmd *agentpb.StartEnv) { _ = disp.HandleStart(context.Background(), cmd) }(p.StartEnv)
				case *agentpb.ControlMessage_DeleteEnv:
					go func(cmd *agentpb.DeleteEnv) { _ = disp.HandleDelete(context.Background(), cmd) }(p.DeleteEnv)
				}
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
	// ±25% jitter: range [d*0.75, d*1.25).
	quarter := int64(d) / 4
	if quarter <= 0 {
		return d
	}
	jitter := time.Duration(rand.Int64N(quarter*2) - quarter) //nolint:gosec — non-crypto
	return d + jitter
}
