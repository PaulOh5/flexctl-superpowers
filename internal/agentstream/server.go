package agentstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/paul/flexctl/internal/agentpb"
	"github.com/paul/flexctl/internal/nodes"
)

const tokenMetadataKey = "node-token"

type Server struct {
	agentpb.UnimplementedAgentServer
	svc *nodes.Service
}

func NewServer(svc *nodes.Service) *Server { return &Server{svc: svc} }

func (s *Server) Stream(stream agentpb.Agent_StreamServer) error {
	ctx := stream.Context()

	// Authenticate
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}
	tokens := md.Get(tokenMetadataKey)
	if len(tokens) == 0 {
		return status.Error(codes.Unauthenticated, "missing node-token")
	}
	node, err := s.svc.AuthenticateNodeToken(ctx, tokens[0])
	if errors.Is(err, nodes.ErrNodeTokenInvalid) {
		return status.Error(codes.Unauthenticated, "invalid node-token")
	}
	if err != nil {
		slog.Error("authenticate node token", "err", err)
		return status.Error(codes.Internal, "auth failure")
	}

	slog.Info("agent connected", "node_id", node.ID.String(), "name", node.Name)

	// Mark offline on disconnect (best-effort)
	defer func() {
		bgCtx := context.Background()
		if err := s.svc.MarkOffline(bgCtx, node.ID); err != nil {
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
	if err := s.svc.UpdateRegister(ctx, node.ID, reg.GetAgentVersion(), gpuJSON); err != nil {
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

	// Subsequent messages: Heartbeat (others ignored for now)
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch msg.GetPayload().(type) {
		case *agentpb.AgentMessage_Heartbeat:
			if err := s.svc.RecordHeartbeat(ctx, node.ID); err != nil {
				slog.Warn("record heartbeat", "err", err)
			}
			if err := stream.Send(&agentpb.ControlMessage{
				Payload: &agentpb.ControlMessage_HeartbeatAck{HeartbeatAck: &agentpb.HeartbeatAck{}},
			}); err != nil {
				return err
			}
		default:
			slog.Warn("unexpected message type from agent", "node_id", node.ID.String())
			// ignore — future commands handled here
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
