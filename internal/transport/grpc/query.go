package grpc

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	lorev1 "github.com/setthasit/Lore/api/proto/lore/v1"
	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/internal/transport"
)

type queryServer struct {
	lorev1.UnimplementedQueryServiceServer
	query     services.QueryService
	why       services.WhyService
	trace     services.TraceService
	impact    services.ImpactService
	history   services.HistoryService
	synthesis services.SynthesisService
	log       *slog.Logger
}

var _ lorev1.QueryServiceServer = (*queryServer)(nil)

func newQueryServer(svc transport.Services, synthesis services.SynthesisService, log *slog.Logger) *queryServer {
	return &queryServer{
		query:     svc.Query,
		why:       svc.Why,
		trace:     svc.Trace,
		impact:    svc.Impact,
		history:   svc.History,
		synthesis: synthesis,
		log:       log,
	}
}

// Unset means synthesize: gRPC is a non-AI surface.
func (s *queryServer) synthesize(
	ctx context.Context,
	method string,
	asked *bool,
	bundle *entities.EvidenceBundle,
) (string, error) {
	if asked != nil && !*asked {
		return "", nil
	}

	answer, err := s.synthesis.Synthesize(ctx, bundle.Question, bundle)
	if err != nil {
		return "", rpcError(s.log, method, err)
	}

	return answer, nil
}

func (s *queryServer) FindDecision(ctx context.Context, in *lorev1.FindDecisionRequest) (*lorev1.FindDecisionResponse, error) {
	bundle, err := s.query.FindDecision(ctx, services.FindDecisionRequest{
		Question: in.GetQuestion(),
		Around:   in.GetAround(),
		Source:   in.GetSource(),
		Repo:     in.GetRepo(),
		DocType:  in.GetDocType(),
		Since:    timeOf(in.GetSince()),
		Until:    timeOf(in.GetUntil()),
	})
	if err != nil {
		return nil, rpcError(s.log, "FindDecision", err)
	}

	answer, err := s.synthesize(ctx, "FindDecision", in.Synthesize, bundle)
	if err != nil {
		return nil, err
	}

	return &lorev1.FindDecisionResponse{Bundle: newEvidenceBundle(bundle), Synthesis: answer}, nil
}

func (s *queryServer) Why(ctx context.Context, in *lorev1.WhyRequest) (*lorev1.WhyResponse, error) {
	bundle, err := s.why.Why(ctx, services.WhyRequest{
		Repo:      in.GetRepo(),
		File:      in.GetFile(),
		LineStart: int(in.GetLineStart()),
		LineEnd:   int(in.GetLineEnd()),
		Question:  in.GetQuestion(),
	})
	if err != nil {
		return nil, rpcError(s.log, "Why", err)
	}

	answer, err := s.synthesize(ctx, "Why", in.Synthesize, bundle)
	if err != nil {
		return nil, err
	}

	return &lorev1.WhyResponse{Bundle: newEvidenceBundle(bundle), Synthesis: answer}, nil
}

func (s *queryServer) Trace(ctx context.Context, in *lorev1.TraceRequest) (*lorev1.TraceResponse, error) {
	bundle, err := s.trace.Trace(ctx, services.TraceRequest{
		Ref:       in.GetRef(),
		Direction: traceDirection(in.GetDirection()),
		Depth:     int(in.GetDepth()),
	})
	if err != nil {
		return nil, rpcError(s.log, "Trace", err)
	}

	answer, err := s.synthesize(ctx, "Trace", in.Synthesize, bundle)
	if err != nil {
		return nil, err
	}

	return &lorev1.TraceResponse{Bundle: newEvidenceBundle(bundle), Synthesis: answer}, nil
}

func (s *queryServer) ImpactOf(ctx context.Context, in *lorev1.ImpactOfRequest) (*lorev1.ImpactOfResponse, error) {
	bundle, err := s.impact.ImpactOf(ctx, services.ImpactRequest{
		Ref:      in.GetRefOrQuery(),
		Question: in.GetQuestion(),
	})
	if err != nil {
		return nil, rpcError(s.log, "ImpactOf", err)
	}

	answer, err := s.synthesize(ctx, "ImpactOf", in.Synthesize, bundle)
	if err != nil {
		return nil, err
	}

	return &lorev1.ImpactOfResponse{Bundle: newEvidenceBundle(bundle), Synthesis: answer}, nil
}

func (s *queryServer) HistoryOf(ctx context.Context, in *lorev1.HistoryOfRequest) (*lorev1.HistoryOfResponse, error) {
	bundle, err := s.history.HistoryOf(ctx, services.HistoryRequest{
		Repo:   in.GetRepo(),
		File:   in.GetPath(),
		Limit:  int(in.GetLimit()),
		Before: in.GetBefore(),
	})
	if err != nil {
		return nil, rpcError(s.log, "HistoryOf", err)
	}

	answer, err := s.synthesize(ctx, "HistoryOf", in.Synthesize, bundle)
	if err != nil {
		return nil, err
	}

	return &lorev1.HistoryOfResponse{Bundle: newEvidenceBundle(bundle), Synthesis: answer}, nil
}

var traceDirections = map[lorev1.Direction]string{
	lorev1.Direction_DIRECTION_OUT:  "out",
	lorev1.Direction_DIRECTION_IN:   "in",
	lorev1.Direction_DIRECTION_BOTH: "both",
}

// Proto3 enums are open, so an unknown direction must reach the service to be
// rejected; only an absent one may fall back to the service's own default.
func traceDirection(direction lorev1.Direction) string {
	if direction == lorev1.Direction_DIRECTION_UNSPECIFIED {
		return ""
	}
	if name, known := traceDirections[direction]; known {
		return name
	}

	return direction.String()
}

func timeOf(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}

	return ts.AsTime()
}
