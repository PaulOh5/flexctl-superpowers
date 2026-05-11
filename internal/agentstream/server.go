package agentstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/paul/flexctl/internal/agentpb"
	"github.com/paul/flexctl/internal/envs"
	"github.com/paul/flexctl/internal/headscale"
	"github.com/paul/flexctl/internal/nodes"
	"github.com/paul/flexctl/internal/sshkeys"
	"github.com/paul/flexctl/internal/users"
)

const tokenMetadataKey = "node-token"

type Server struct {
	agentpb.UnimplementedAgentServer

	nodes *nodes.Service
	envs  *envs.Service
	users *users.Service
	keys  *sshkeys.Service
	hs    *headscale.Client

	mu      sync.Mutex
	streams map[uuid.UUID]agentpb.Agent_StreamServer
}

func NewServer(nodesSvc *nodes.Service, envsSvc *envs.Service, usersSvc *users.Service,
	keysSvc *sshkeys.Service, hs *headscale.Client) *Server {
	return &Server{
		nodes: nodesSvc, envs: envsSvc, users: usersSvc, keys: keysSvc, hs: hs,
		streams: map[uuid.UUID]agentpb.Agent_StreamServer{},
	}
}

func (s *Server) registerStream(nodeID uuid.UUID, stream agentpb.Agent_StreamServer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streams[nodeID] = stream
}

func (s *Server) unregisterStream(nodeID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.streams, nodeID)
}

func (s *Server) streamFor(nodeID uuid.UUID) (agentpb.Agent_StreamServer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.streams[nodeID]
	return st, ok
}

func (s *Server) Stream(stream agentpb.Agent_StreamServer) error {
	ctx := stream.Context()

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}
	tokens := md.Get(tokenMetadataKey)
	if len(tokens) == 0 {
		return status.Error(codes.Unauthenticated, "missing node-token")
	}
	node, err := s.nodes.AuthenticateNodeToken(ctx, tokens[0])
	if errors.Is(err, nodes.ErrNodeTokenInvalid) {
		return status.Error(codes.Unauthenticated, "invalid node-token")
	}
	if err != nil {
		slog.Error("authenticate node token", "err", err)
		return status.Error(codes.Internal, "auth failure")
	}

	slog.Info("agent connected", "node_id", node.ID.String(), "name", node.Name)

	s.registerStream(node.ID, stream)
	defer s.unregisterStream(node.ID)
	defer func() {
		bgCtx := context.Background()
		if err := s.nodes.MarkOffline(bgCtx, node.ID); err != nil {
			slog.Warn("mark offline", "err", err, "node_id", node.ID.String())
		}
	}()

	// First message MUST be Register
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	reg := first.GetRegister()
	if reg == nil {
		return status.Error(codes.FailedPrecondition, "first message must be Register")
	}
	gpuJSON, err := encodeGPUs(reg.GetGpus())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "encode gpu_info: %v", err)
	}
	if err := s.nodes.UpdateRegister(ctx, node.ID, reg.GetAgentVersion(), gpuJSON); err != nil {
		slog.Error("update register", "err", err)
		return status.Error(codes.Internal, "update register")
	}
	if err := stream.Send(&agentpb.ControlMessage{
		Payload: &agentpb.ControlMessage_RegisterAck{
			RegisterAck: &agentpb.RegisterAck{NodeId: node.ID.String()},
		},
	}); err != nil {
		return err
	}

	// Main loop
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch p := msg.GetPayload().(type) {
		case *agentpb.AgentMessage_Heartbeat:
			if err := s.nodes.RecordHeartbeat(ctx, node.ID); err != nil {
				slog.Warn("record heartbeat", "err", err)
			}
			_ = stream.Send(&agentpb.ControlMessage{
				Payload: &agentpb.ControlMessage_HeartbeatAck{HeartbeatAck: &agentpb.HeartbeatAck{}},
			})

		case *agentpb.AgentMessage_EnvReady:
			r := p.EnvReady
			envID, parseErr := uuid.Parse(r.GetEnvId())
			if parseErr != nil {
				slog.Warn("invalid env_id in EnvReady", "v", r.GetEnvId())
				continue
			}
			gpus := make([]int, 0, len(r.GetGpuIndices()))
			for _, g := range r.GetGpuIndices() {
				gpus = append(gpus, int(g))
			}
			if err := s.envs.MarkRunning(ctx, envID, r.GetSidecarContainerId(), r.GetDevContainerId(), gpus); err != nil {
				slog.Error("envs.MarkRunning", "err", err, "env_id", r.GetEnvId())
			}

		case *agentpb.AgentMessage_EnvStopped:
			envID, parseErr := uuid.Parse(p.EnvStopped.GetEnvId())
			if parseErr != nil {
				continue
			}
			if err := s.envs.MarkStopped(ctx, envID); err != nil {
				slog.Error("envs.MarkStopped", "err", err)
			}

		case *agentpb.AgentMessage_EnvDeleted:
			envID, parseErr := uuid.Parse(p.EnvDeleted.GetEnvId())
			if parseErr != nil {
				continue
			}
			if err := s.envs.Delete(ctx, envID); err != nil {
				slog.Error("envs.Delete", "err", err)
			}

		case *agentpb.AgentMessage_EnvError:
			envID, parseErr := uuid.Parse(p.EnvError.GetEnvId())
			if parseErr != nil {
				continue
			}
			if err := s.envs.MarkError(ctx, envID, p.EnvError.GetStage(), p.EnvError.GetDetail()); err != nil {
				slog.Error("envs.MarkError", "err", err)
			}

		case *agentpb.AgentMessage_EnvSnapshot:
			reportedSet := map[string]bool{}
			for _, id := range p.EnvSnapshot.GetRunningEnvIds() {
				reportedSet[id] = true
			}
			dbRunning, err := s.envs.ListRunningOnNode(ctx, node.ID)
			if err != nil {
				slog.Error("list running on node", "err", err)
				continue
			}
			for _, e := range dbRunning {
				if !reportedSet[e.ID.String()] {
					if err := s.envs.MarkError(ctx, e.ID, "resync", "lost during agent reconnect"); err != nil {
						slog.Error("mark error on resync", "err", err)
					}
				}
			}

		default:
			slog.Warn("unexpected message type from agent", "node_id", node.ID.String())
		}
	}
}

func encodeGPUs(gpus []*agentpb.GPU) ([]byte, error) {
	if len(gpus) == 0 {
		return []byte(`[]`), nil
	}
	type gpuJSON struct {
		Index  int32  `json:"index"`
		Model  string `json:"model"`
		VRAMMB int32  `json:"vram_mb"`
	}
	out := make([]gpuJSON, 0, len(gpus))
	for _, g := range gpus {
		out = append(out, gpuJSON{Index: g.GetIndex(), Model: g.GetModel(), VRAMMB: g.GetVramMb()})
	}
	return json.Marshal(out)
}

// EnvsDispatcher implements envs.Dispatcher via the agent gRPC stream.
type EnvsDispatcher struct {
	srv   *Server
	envs  *envs.Service
	users *users.Service
	keys  *sshkeys.Service
	hs    *headscale.Client

	headscaleClientURL string
	sidecarImage       string
}

func NewEnvsDispatcher(srv *Server, headscaleClientURL, sidecarImage string) *EnvsDispatcher {
	return &EnvsDispatcher{
		srv: srv, envs: srv.envs, users: srv.users, keys: srv.keys, hs: srv.hs,
		headscaleClientURL: headscaleClientURL, sidecarImage: sidecarImage,
	}
}

func (d *EnvsDispatcher) Create(ctx context.Context, envID uuid.UUID) error {
	env, err := d.envs.ByID(ctx, envID)
	if err != nil {
		return err
	}
	user, err := d.users.ByID(ctx, env.OwnerUserID)
	if err != nil {
		return err
	}
	keys, err := d.keys.List(ctx, env.OwnerUserID)
	if err != nil {
		return err
	}
	keyText := concatKeys(keys)
	pak, err := d.hs.CreatePreAuthKey(ctx, headscale.PreAuthKeyRequest{
		User: user.Slug, Ephemeral: true, Reusable: false,
		Expiration: 24 * time.Hour, ACLTags: []string{"tag:env-" + user.Slug},
	})
	if err != nil {
		return fmt.Errorf("preauthkey: %w", err)
	}

	tpl, err := d.envs.TemplateByID(ctx, env.TemplateID)
	if err != nil {
		return err
	}

	stream, ok := d.srv.streamFor(env.NodeID)
	if !ok {
		return fmt.Errorf("agent not connected for node %s", env.NodeID)
	}

	return stream.Send(&agentpb.ControlMessage{
		Payload: &agentpb.ControlMessage_CreateEnv{
			CreateEnv: &agentpb.CreateEnv{
				EnvId:           envID.String(),
				ImageRef:        tpl.ImageRef,
				SidecarImageRef: d.sidecarImage,
				Hostname:        env.Hostname,
				HeadscaleUrl:    d.headscaleClientURL,
				PreauthKey:      pak.Key,
				Tags:            []string{"tag:env-" + user.Slug},
				AuthorizedKeys:  keyText,
				GpuRequest:      env.GPURequest,
				DefaultCmd:      tpl.DefaultCmd,
			},
		},
	})
}

func (d *EnvsDispatcher) Stop(ctx context.Context, envID uuid.UUID) error {
	env, err := d.envs.ByID(ctx, envID)
	if err != nil {
		return err
	}
	stream, ok := d.srv.streamFor(env.NodeID)
	if !ok {
		return fmt.Errorf("agent not connected")
	}
	return stream.Send(&agentpb.ControlMessage{
		Payload: &agentpb.ControlMessage_StopEnv{StopEnv: &agentpb.StopEnv{EnvId: envID.String()}},
	})
}

func (d *EnvsDispatcher) Start(ctx context.Context, envID uuid.UUID) error {
	env, err := d.envs.ByID(ctx, envID)
	if err != nil {
		return err
	}
	user, err := d.users.ByID(ctx, env.OwnerUserID)
	if err != nil {
		return err
	}
	keys, err := d.keys.List(ctx, env.OwnerUserID)
	if err != nil {
		return err
	}
	pak, err := d.hs.CreatePreAuthKey(ctx, headscale.PreAuthKeyRequest{
		User: user.Slug, Ephemeral: true, Reusable: false,
		Expiration: 24 * time.Hour, ACLTags: []string{"tag:env-" + user.Slug},
	})
	if err != nil {
		return err
	}
	stream, ok := d.srv.streamFor(env.NodeID)
	if !ok {
		return fmt.Errorf("agent not connected")
	}
	return stream.Send(&agentpb.ControlMessage{
		Payload: &agentpb.ControlMessage_StartEnv{
			StartEnv: &agentpb.StartEnv{
				EnvId:          envID.String(),
				PreauthKey:     pak.Key,
				AuthorizedKeys: concatKeys(keys),
			},
		},
	})
}

func (d *EnvsDispatcher) Delete(ctx context.Context, envID uuid.UUID) error {
	env, err := d.envs.ByID(ctx, envID)
	if err != nil {
		return err
	}
	stream, ok := d.srv.streamFor(env.NodeID)
	if !ok {
		// agent offline → just remove DB row
		return d.envs.Delete(ctx, envID)
	}
	return stream.Send(&agentpb.ControlMessage{
		Payload: &agentpb.ControlMessage_DeleteEnv{DeleteEnv: &agentpb.DeleteEnv{EnvId: envID.String()}},
	})
}

func concatKeys(keys []sshkeys.Key) string {
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k.PublicKey)
		b.WriteString("\n")
	}
	return b.String()
}
