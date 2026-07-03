// Package server implements the Unwedge gRPC service by wiring together the
// serial console, power control, U-Boot orchestration, image store/TFTP server,
// and SSH client.
package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	unwedgev1 "github.com/sonix-network/unwedge/gen/unwedge/v1"
	"github.com/sonix-network/unwedge/internal/power"
	"github.com/sonix-network/unwedge/internal/serialconsole"
	"github.com/sonix-network/unwedge/internal/session"
	"github.com/sonix-network/unwedge/internal/sshexec"
	"github.com/sonix-network/unwedge/internal/tftp"
	"github.com/sonix-network/unwedge/internal/uboot"
)

// Deps are the collaborators the service needs. Any of Power, Orchestrator,
// Store, SSH may be nil if that capability is unconfigured; RPCs needing them
// then return FailedPrecondition.
type Deps struct {
	Version      string
	Console      *serialconsole.Console
	Power        power.Controller
	Orchestrator *uboot.Orchestrator
	Store        *tftp.Store
	SSH          *sshexec.Client
	Sessions     *session.Manager // nil disables session locking
	Logger       *slog.Logger

	// Descriptive fields surfaced by GetStatus.
	SerialDevice string
	SerialBaud   uint32
	TFTPAddress  string
}

// Service implements unwedgev1.UnwedgeServer.
type Service struct {
	unwedgev1.UnimplementedUnwedgeServer
	deps Deps
	log  *slog.Logger
}

// New builds a Service.
func New(deps Deps) *Service {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Service{deps: deps, log: deps.Logger}
}

func dur(ms int64, def time.Duration) time.Duration {
	if ms <= 0 {
		return def
	}
	return time.Duration(ms) * time.Millisecond
}

// ---- Introspection ---------------------------------------------------------

func (s *Service) GetStatus(ctx context.Context, _ *unwedgev1.GetStatusRequest) (*unwedgev1.GetStatusResponse, error) {
	resp := &unwedgev1.GetStatusResponse{
		Version:      s.deps.Version,
		SerialDevice: s.deps.SerialDevice,
		SerialBaud:   s.deps.SerialBaud,
		TftpAddress:  s.deps.TFTPAddress,
	}
	if s.deps.Console != nil {
		resp.SerialConnected = s.deps.Console.Err() == nil
		resp.ConsoleBufferBytes = int64(s.deps.Console.BufferedBytes())
	}
	if s.deps.Store != nil {
		resp.TftpDir = s.deps.Store.Dir()
	}
	resp.PowerState = unwedgev1.PowerState_POWER_STATE_UNKNOWN
	if s.deps.Power != nil {
		if st, err := s.deps.Power.Status(ctx); err == nil {
			resp.PowerState = powerStateToProto(st)
		}
	}
	if s.deps.Sessions != nil {
		si := s.deps.Sessions.Info()
		resp.SessionActive = si.Active
		resp.SessionOwner = si.Owner
		if si.Active {
			resp.SessionStartedAtUnixMs = si.StartedAt.UnixMilli()
			resp.SessionExpiresAtUnixMs = si.ExpiresAt.UnixMilli()
		}
	}
	return resp, nil
}

// ---- Serial console --------------------------------------------------------

func (s *Service) StreamConsole(req *unwedgev1.StreamConsoleRequest, stream unwedgev1.Unwedge_StreamConsoleServer) error {
	if s.deps.Console == nil {
		return status.Error(codes.FailedPrecondition, "serial console not configured")
	}
	sub := s.deps.Console.Subscribe(int(req.GetReplayBytes()))
	defer sub.Close()
	ctx := stream.Context()
	var offset int64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case chunk, ok := <-sub.C():
			if !ok {
				return status.Error(codes.Unavailable, "console closed")
			}
			if err := stream.Send(&unwedgev1.ConsoleChunk{Data: chunk, Offset: offset}); err != nil {
				return err
			}
			offset += int64(len(chunk))
		}
	}
}

func (s *Service) ReadConsoleLog(_ context.Context, req *unwedgev1.ReadConsoleLogRequest) (*unwedgev1.ReadConsoleLogResponse, error) {
	if s.deps.Console == nil {
		return nil, status.Error(codes.FailedPrecondition, "serial console not configured")
	}
	data, truncated := s.deps.Console.Snapshot(int(req.GetMaxBytes()))
	return &unwedgev1.ReadConsoleLogResponse{Data: data, Truncated: truncated}, nil
}

func (s *Service) WriteConsole(_ context.Context, req *unwedgev1.WriteConsoleRequest) (*unwedgev1.WriteConsoleResponse, error) {
	if s.deps.Console == nil {
		return nil, status.Error(codes.FailedPrecondition, "serial console not configured")
	}
	var out []byte
	out = append(out, req.GetData()...)
	if keys := req.GetKeys(); len(keys) > 0 {
		kb, err := serialconsole.KeysBytes(keys)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		out = append(out, kb...)
	}
	n, err := s.deps.Console.Write(out)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "console write: %v", err)
	}
	return &unwedgev1.WriteConsoleResponse{BytesWritten: int64(n)}, nil
}

func (s *Service) WaitForPattern(ctx context.Context, req *unwedgev1.WaitForPatternRequest) (*unwedgev1.WaitForPatternResponse, error) {
	if s.deps.Console == nil {
		return nil, status.Error(codes.FailedPrecondition, "serial console not configured")
	}
	re, err := regexp.Compile(req.GetPattern())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "bad pattern: %v", err)
	}
	timeout := dur(req.GetTimeoutMs(), 30*time.Second)
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	match, contextOut, err := s.deps.Console.WaitForPattern(wctx, re, int(req.GetIncludeScrollbackBytes()))
	elapsed := time.Since(start).Milliseconds()
	resp := &unwedgev1.WaitForPatternResponse{Context: contextOut, ElapsedMs: elapsed}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			resp.Matched = false
			return resp, nil
		}
		return resp, status.Errorf(codes.Unavailable, "wait: %v", err)
	}
	resp.Matched = true
	resp.Match = match
	return resp, nil
}

// ---- Power -----------------------------------------------------------------

func (s *Service) PowerControl(ctx context.Context, req *unwedgev1.PowerControlRequest) (*unwedgev1.PowerControlResponse, error) {
	if s.deps.Power == nil {
		return nil, status.Error(codes.FailedPrecondition, "power control not configured")
	}
	var err error
	switch req.GetAction() {
	case unwedgev1.PowerAction_POWER_ACTION_ON:
		err = s.deps.Power.On(ctx)
	case unwedgev1.PowerAction_POWER_ACTION_OFF:
		err = s.deps.Power.Off(ctx)
	case unwedgev1.PowerAction_POWER_ACTION_CYCLE:
		err = s.deps.Power.Cycle(ctx, dur(req.GetOffDurationMs(), 0))
	case unwedgev1.PowerAction_POWER_ACTION_STATUS:
		// no-op; fall through to status query
	default:
		return nil, status.Error(codes.InvalidArgument, "unspecified power action")
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "power: %v", err)
	}
	st, stErr := s.deps.Power.Status(ctx)
	resp := &unwedgev1.PowerControlResponse{State: powerStateToProto(st)}
	if stErr != nil {
		resp.Detail = "state query failed: " + stErr.Error()
	}
	return resp, nil
}

// ---- Boot orchestration ----------------------------------------------------

func (s *Service) InterruptBoot(req *unwedgev1.InterruptBootRequest, stream unwedgev1.Unwedge_InterruptBootServer) error {
	if s.deps.Orchestrator == nil {
		return status.Error(codes.FailedPrecondition, "uboot orchestrator not configured")
	}
	ctx, cancel := context.WithTimeout(stream.Context(), dur(req.GetTimeoutMs(), 3*time.Minute))
	defer cancel()
	emit := streamEmitter(stream)
	err := s.deps.Orchestrator.InterruptBoot(ctx, req.GetPowerCycle(), emit)
	return finishBoot(stream, err)
}

func (s *Service) Netboot(req *unwedgev1.NetbootRequest, stream unwedgev1.Unwedge_NetbootServer) error {
	if s.deps.Orchestrator == nil {
		return status.Error(codes.FailedPrecondition, "uboot orchestrator not configured")
	}
	params := uboot.NetbootParams{
		Image:       req.GetImage(),
		PowerCycle:  req.GetPowerCycle(),
		KernelArgs:  req.GetKernelArgs(),
		VerifyCRC32: req.GetVerifyCrc32(),
		Timeout:     dur(req.GetTimeoutMs(), 5*time.Minute),
	}
	// If CRC verification is requested, fill in the length/CRC from the store.
	if params.VerifyCRC32 {
		if s.deps.Store == nil {
			return status.Error(codes.FailedPrecondition, "verify_crc32 requested but image store not configured")
		}
		list, err := s.deps.Store.List()
		if err != nil {
			return status.Errorf(codes.Internal, "list images: %v", err)
		}
		found := false
		for _, im := range list {
			if im.Name == req.GetImage() {
				params.ImageLen = int(im.Size)
				params.ImageCRC32 = im.CRC32
				found = true
				break
			}
		}
		if !found {
			return status.Errorf(codes.NotFound, "image %q not found for CRC verification", req.GetImage())
		}
	}
	// U-Boot fetches over the shared TFTP server by on-disk basename, which
	// carries this instance's namespace prefix; the client only ever sees the
	// clean name. Translate here so a multi-instance controller's DUTs don't
	// collide on identically-named uploads.
	if s.deps.Store != nil {
		onDisk, err := s.deps.Store.OnDiskName(req.GetImage())
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "invalid image name %q: %v", req.GetImage(), err)
		}
		params.Image = onDisk
	}
	emit := streamEmitter(stream)
	err := s.deps.Orchestrator.Netboot(stream.Context(), params, emit)
	return finishBoot(stream, err)
}

func (s *Service) RunUbootCommand(ctx context.Context, req *unwedgev1.RunUbootCommandRequest) (*unwedgev1.RunUbootCommandResponse, error) {
	if s.deps.Orchestrator == nil {
		return nil, status.Error(codes.FailedPrecondition, "uboot orchestrator not configured")
	}
	cctx, cancel := context.WithTimeout(ctx, dur(req.GetTimeoutMs(), 30*time.Second))
	defer cancel()
	out, err := s.deps.Orchestrator.RunCommand(cctx, req.GetCommand())
	resp := &unwedgev1.RunUbootCommandResponse{Output: out, PromptReturned: err == nil}
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		// Return output with the error surfaced in the message for context.
		return resp, nil
	}
	return resp, nil
}

// ---- Images ----------------------------------------------------------------

func (s *Service) UploadImage(stream unwedgev1.Unwedge_UploadImageServer) error {
	if s.deps.Store == nil {
		return status.Error(codes.FailedPrecondition, "image store not configured")
	}
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "expected metadata message: %v", err)
	}
	meta := first.GetMetadata()
	if meta == nil {
		return status.Error(codes.InvalidArgument, "first message must be metadata")
	}
	pr, pw := io.Pipe()
	done := make(chan struct{})
	var info tftp.Info
	var saveErr error
	go func() {
		defer close(done)
		info, saveErr = s.deps.Store.Save(meta.GetName(), pr, meta.GetOverwrite())
		// On a Save error, close the read end so the writer unblocks with it.
		if saveErr != nil {
			pr.CloseWithError(saveErr)
		}
	}()

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			pw.CloseWithError(err)
			<-done
			return status.Errorf(codes.Aborted, "receiving image: %v", err)
		}
		if chunk := msg.GetChunk(); len(chunk) > 0 {
			if _, werr := pw.Write(chunk); werr != nil {
				<-done
				return status.Errorf(codes.Internal, "store write: %v", saveErr)
			}
		}
	}
	pw.Close()
	<-done
	if saveErr != nil {
		return status.Errorf(codes.Internal, "save image: %v", saveErr)
	}
	return stream.SendAndClose(&unwedgev1.UploadImageResponse{
		Name:   info.Name,
		Size:   info.Size,
		Sha256: info.SHA256,
		Crc32:  info.CRC32,
	})
}

func (s *Service) ListImages(_ context.Context, _ *unwedgev1.ListImagesRequest) (*unwedgev1.ListImagesResponse, error) {
	if s.deps.Store == nil {
		return nil, status.Error(codes.FailedPrecondition, "image store not configured")
	}
	list, err := s.deps.Store.List()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list images: %v", err)
	}
	resp := &unwedgev1.ListImagesResponse{}
	for _, im := range list {
		resp.Images = append(resp.Images, &unwedgev1.ImageInfo{
			Name:        im.Name,
			Size:        im.Size,
			ModTimeUnix: im.ModTime.Unix(),
			Crc32:       im.CRC32,
		})
	}
	return resp, nil
}

func (s *Service) DeleteImage(_ context.Context, req *unwedgev1.DeleteImageRequest) (*unwedgev1.DeleteImageResponse, error) {
	if s.deps.Store == nil {
		return nil, status.Error(codes.FailedPrecondition, "image store not configured")
	}
	if err := s.deps.Store.Delete(req.GetName()); err != nil {
		return nil, status.Errorf(codes.Internal, "delete image: %v", err)
	}
	return &unwedgev1.DeleteImageResponse{}, nil
}

// ---- SSH -------------------------------------------------------------------

func (s *Service) SSHExec(ctx context.Context, req *unwedgev1.SSHExecRequest) (*unwedgev1.SSHExecResponse, error) {
	if s.deps.SSH == nil {
		return nil, status.Error(codes.FailedPrecondition, "ssh not configured")
	}
	res, err := s.deps.SSH.Exec(ctx, req.GetHostOverride(), req.GetCommand(), dur(req.GetTimeoutMs(), 30*time.Second))
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "ssh: %v", err)
	}
	return &unwedgev1.SSHExecResponse{
		ExitCode: int32(res.ExitCode),
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		TimedOut: res.TimedOut,
	}, nil
}

// ---- helpers ---------------------------------------------------------------

func powerStateToProto(st power.State) unwedgev1.PowerState {
	switch st {
	case power.StateOn:
		return unwedgev1.PowerState_POWER_STATE_ON
	case power.StateOff:
		return unwedgev1.PowerState_POWER_STATE_OFF
	default:
		return unwedgev1.PowerState_POWER_STATE_UNKNOWN
	}
}

// streamEmitter adapts a boot-event stream to uboot.Emit. Send errors are
// ignored here; a broken stream is detected when the RPC returns.
func streamEmitter(stream unwedgev1.Unwedge_NetbootServer) uboot.Emit {
	return func(ev uboot.Event) {
		_ = stream.Send(bootEventToProto(ev))
	}
}

func bootEventToProto(ev uboot.Event) *unwedgev1.BootEvent {
	pe := &unwedgev1.BootEvent{
		Stage:   ev.Stage,
		Message: ev.Message,
		Console: ev.Console,
	}
	switch ev.Kind {
	case uboot.EventInfo:
		pe.Kind = unwedgev1.BootEvent_KIND_INFO
	case uboot.EventConsole:
		pe.Kind = unwedgev1.BootEvent_KIND_CONSOLE
	case uboot.EventStage:
		pe.Kind = unwedgev1.BootEvent_KIND_STAGE
	case uboot.EventSuccess:
		pe.Kind = unwedgev1.BootEvent_KIND_SUCCESS
	case uboot.EventError:
		pe.Kind = unwedgev1.BootEvent_KIND_ERROR
	default:
		pe.Kind = unwedgev1.BootEvent_KIND_UNSPECIFIED
	}
	return pe
}

// finishBoot sends a terminal SUCCESS/ERROR event and maps the error to a
// gRPC status.
func finishBoot(stream unwedgev1.Unwedge_NetbootServer, err error) error {
	if err != nil {
		_ = stream.Send(&unwedgev1.BootEvent{
			Kind:    unwedgev1.BootEvent_KIND_ERROR,
			Message: err.Error(),
		})
		if errors.Is(err, context.DeadlineExceeded) {
			return status.Error(codes.DeadlineExceeded, err.Error())
		}
		return status.Error(codes.Internal, err.Error())
	}
	_ = stream.Send(&unwedgev1.BootEvent{
		Kind:    unwedgev1.BootEvent_KIND_SUCCESS,
		Message: "operation completed",
	})
	return nil
}
