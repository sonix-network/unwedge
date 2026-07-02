package server

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	unwedgev1 "github.com/sonix-network/unwedge/gen/unwedge/v1"
	"github.com/sonix-network/unwedge/internal/session"
)

// SessionMetadataKey is the gRPC metadata header carrying the session ID on
// every operational call. Clients set it automatically.
const SessionMetadataKey = "unwedge-session-id"

// sessionExemptMethods are not gated by the hardware lock: status (so the lock
// is always observable), the read-only console observers (so anyone can watch
// what the lock holder is doing), and the session-management RPCs themselves.
// Writing to the console, driving U-Boot, power, images, and SSH all require the
// lock.
var sessionExemptMethods = map[string]bool{
	unwedgev1.Unwedge_GetStatus_FullMethodName:      true,
	unwedgev1.Unwedge_StreamConsole_FullMethodName:  true,
	unwedgev1.Unwedge_ReadConsoleLog_FullMethodName: true,
	unwedgev1.Unwedge_StartSession_FullMethodName:   true,
	unwedgev1.Unwedge_FinishSession_FullMethodName:  true,
	unwedgev1.Unwedge_Ping_FullMethodName:           true,
}

func sessionIDFromContext(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	if v := md.Get(SessionMetadataKey); len(v) > 0 {
		return v[0]
	}
	return ""
}

// ---- Session RPCs ----------------------------------------------------------

func (s *Service) StartSession(ctx context.Context, req *unwedgev1.StartSessionRequest) (*unwedgev1.StartSessionResponse, error) {
	if s.deps.Sessions == nil {
		return nil, status.Error(codes.Unimplemented, "session locking is disabled on this daemon")
	}
	// wait_timeout_ms: 0 => block until the client's context/deadline; <0 =>
	// fail fast if held; >0 => bounded wait.
	wait := time.Duration(req.GetWaitTimeoutMs()) * time.Millisecond
	info, err := s.deps.Sessions.Acquire(ctx, req.GetOwner(), wait)
	if err != nil {
		var busy *session.BusyError
		switch {
		case errors.As(err, &busy):
			return nil, status.Error(codes.FailedPrecondition, busy.Error())
		case errors.Is(err, context.DeadlineExceeded):
			return nil, status.Error(codes.DeadlineExceeded, "timed out waiting for the hardware lock")
		case errors.Is(err, context.Canceled):
			return nil, status.Error(codes.Canceled, "cancelled while waiting for the hardware lock")
		default:
			return nil, status.Errorf(codes.Internal, "acquire session: %v", err)
		}
	}
	return &unwedgev1.StartSessionResponse{
		SessionId:       info.ID,
		ExpiresAtUnixMs: info.ExpiresAt.UnixMilli(),
		TtlMs:           s.deps.Sessions.TTL().Milliseconds(),
	}, nil
}

func (s *Service) FinishSession(_ context.Context, req *unwedgev1.FinishSessionRequest) (*unwedgev1.FinishSessionResponse, error) {
	if s.deps.Sessions == nil {
		return &unwedgev1.FinishSessionResponse{}, nil
	}
	if err := s.deps.Sessions.Finish(req.GetSessionId()); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &unwedgev1.FinishSessionResponse{}, nil
}

func (s *Service) Ping(_ context.Context, req *unwedgev1.PingRequest) (*unwedgev1.PingResponse, error) {
	if s.deps.Sessions == nil {
		return &unwedgev1.PingResponse{}, nil
	}
	if err := s.deps.Sessions.Refresh(req.GetSessionId()); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &unwedgev1.PingResponse{ExpiresAtUnixMs: s.deps.Sessions.Info().ExpiresAt.UnixMilli()}, nil
}

// ---- Interceptors ----------------------------------------------------------

// UnaryInterceptor enforces the hardware lock on operational unary RPCs and
// refreshes the holder's TTL.
func (s *Service) UnaryInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	if s.deps.Sessions == nil || sessionExemptMethods[info.FullMethod] {
		return handler(ctx, req)
	}
	id := sessionIDFromContext(ctx)
	if err := s.deps.Sessions.CallStart(id); err != nil {
		return nil, sessionRequiredErr(id)
	}
	defer s.deps.Sessions.CallEnd(id)
	return handler(ctx, req)
}

// StreamInterceptor enforces the lock on operational streaming RPCs and
// refreshes the TTL on every message so long-lived streams do not time out.
func (s *Service) StreamInterceptor(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if s.deps.Sessions == nil || sessionExemptMethods[info.FullMethod] {
		return handler(srv, ss)
	}
	id := sessionIDFromContext(ss.Context())
	if err := s.deps.Sessions.CallStart(id); err != nil {
		return sessionRequiredErr(id)
	}
	defer s.deps.Sessions.CallEnd(id)
	return handler(srv, &refreshingStream{ServerStream: ss, mgr: s.deps.Sessions, id: id})
}

func sessionRequiredErr(id string) error {
	if id == "" {
		return status.Error(codes.FailedPrecondition, "hardware is locked to sessions: acquire one with StartSession (no session id supplied)")
	}
	return status.Errorf(codes.FailedPrecondition, "session %q is not the current hardware lock holder (expired or superseded); call StartSession", id)
}

type refreshingStream struct {
	grpc.ServerStream
	mgr *session.Manager
	id  string
}

func (r *refreshingStream) SendMsg(m interface{}) error {
	_ = r.mgr.Refresh(r.id)
	return r.ServerStream.SendMsg(m)
}

func (r *refreshingStream) RecvMsg(m interface{}) error {
	_ = r.mgr.Refresh(r.id)
	return r.ServerStream.RecvMsg(m)
}
