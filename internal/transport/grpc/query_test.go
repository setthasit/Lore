package grpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lorev1 "lore/api/proto/lore/v1"
	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	mock_services "lore/internal/mocks/services"
	"lore/internal/services"
	"lore/internal/transport"
)

const (
	rpcQuestion  = "why postgres over mysql"
	rpcCause     = "dial 10.1.2.3:5432: connection refused"
	traceTestRef = "abc1234"
	rpcTimeout   = 10 * time.Second
	rpcBuffer    = 1024 * 1024
)

var rpcCreatedAt = time.Date(2025, 3, 12, 9, 30, 0, 0, time.UTC)

type rpcFixture struct {
	query   *mock_services.MockQueryService
	trace   *mock_services.MockTraceService
	sync    *mock_services.MockSyncOrchestrator
	status  *mock_services.MockStatusService
	queries lorev1.QueryServiceClient
	syncs   lorev1.SyncServiceClient
	logs    *bytes.Buffer
	stop    context.CancelFunc
}

func newRPCFixture(t *testing.T) rpcFixture {
	t.Helper()

	ctrl := gomock.NewController(t)
	f := rpcFixture{
		query:  mock_services.NewMockQueryService(ctrl),
		trace:  mock_services.NewMockTraceService(ctrl),
		sync:   mock_services.NewMockSyncOrchestrator(ctrl),
		status: mock_services.NewMockStatusService(ctrl),
		logs:   &bytes.Buffer{},
	}

	svc, log := f.services(), slog.New(slog.NewTextHandler(f.logs, nil))
	listener := bufconn.Listen(rpcBuffer)
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, listener, svc, log) }()

	conn, err := grpclib.NewClient("passthrough:///bufnet",
		grpclib.WithTransportCredentials(insecure.NewCredentials()),
		grpclib.WithContextDialer(func(dial context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(dial)
		}))
	if err != nil {
		stop()
		t.Fatalf("dial the server: %v", err)
	}

	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close the client: %v", err)
		}
		stop()
		if err := <-served; err != nil {
			t.Errorf("Serve() = %v, want nil", err)
		}
	})

	f.queries = lorev1.NewQueryServiceClient(conn)
	f.syncs = lorev1.NewSyncServiceClient(conn)
	f.stop = stop

	return f
}

func (f rpcFixture) services() transport.Services {
	return transport.Services{Query: f.query, Trace: f.trace, Sync: f.sync, Status: f.status}
}

func (f rpcFixture) findDecision(t *testing.T, in *lorev1.FindDecisionRequest) (*lorev1.FindDecisionResponse, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()

	return f.queries.FindDecision(ctx, in)
}

func (f rpcFixture) traceRef(t *testing.T, direction lorev1.Direction) (*lorev1.TraceResponse, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()

	return f.queries.Trace(ctx, &lorev1.TraceRequest{Ref: traceTestRef, Direction: direction, Depth: 2})
}

func rpcStatus(t *testing.T, err error) *status.Status {
	t.Helper()

	if err == nil {
		t.Fatal("call succeeded, want an error")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error %v is not a gRPC status", err)
	}

	return st
}

func assertSameProto(t *testing.T, got, want proto.Message) {
	t.Helper()

	if !proto.Equal(got, want) {
		t.Errorf("message =\n%v\nwant\n%v", got, want)
	}
}

func rpcBundle() *entities.EvidenceBundle {
	return &entities.EvidenceBundle{
		Question: rpcQuestion,
		Anchor: entities.Anchor{
			Kind:  entities.AnchorQuery | entities.AnchorCodeSpan | entities.AnchorDocument | entities.AnchorTimeWindow,
			Query: rpcQuestion,
			Code: &entities.CodeAnchor{
				Repo:       "acme/lore",
				File:       "internal/repositories/sqlite/store.go",
				LineStart:  12,
				LineEnd:    48,
				BlamedSHAs: []string{"abc1234", "def5678"},
			},
			Doc: &entities.DocRef{
				ID:        "github:pr:42",
				Title:     "Switch the store to postgres",
				URL:       "https://github.com/acme/lore/pull/42",
				CreatedAt: rpcCreatedAt,
			},
			Window: &entities.TimeWindow{
				From:       rpcCreatedAt.AddDate(0, 0, -30),
				To:         rpcCreatedAt,
				Derivation: "date 2025-03-12 +/- 30d",
				AnchoredBy: "jira:ticket:PROJ-1",
			},
		},
		Nodes: []entities.EvidenceNode{
			{
				Doc: entities.DocumentMeta{
					ID:        "github:pr:42",
					Source:    "github",
					Type:      entities.DocTypePR,
					Title:     "Switch the store to postgres",
					Author:    "ada",
					URL:       "https://github.com/acme/lore/pull/42",
					CreatedAt: rpcCreatedAt,
					UpdatedAt: rpcCreatedAt,
				},
				Excerpt: "we need transactional DDL",
				Role:    entities.RoleSeed,
				Score:   0.5,
				Via: []entities.Edge{{
					Src:        "github:pr:42",
					Dst:        "jira:ticket:PROJ-1",
					Kind:       entities.EdgeKindPRClosesIssue,
					Confidence: 1,
				}},
			},
			{
				Doc: entities.DocumentMeta{
					ID:        "jira:ticket:PROJ-1",
					Source:    "jira",
					Type:      entities.DocTypeTicket,
					Title:     "Pick the primary store",
					Author:    "grace",
					URL:       "https://jira.test/browse/PROJ-1",
					CreatedAt: rpcCreatedAt.AddDate(0, 0, -10),
				},
				Excerpt: "mysql cannot do transactional DDL",
				Role:    entities.RoleLinkedTicket,
				Score:   0.25,
			},
		},
		Chains: [][]entities.DocID{{"jira:ticket:PROJ-1", "github:pr:42"}},
		Gaps:   []string{"trail ends at PROJ-1; no linked follow-up"},
	}
}

func rpcBundleProto() *lorev1.EvidenceBundle {
	return &lorev1.EvidenceBundle{
		Question: rpcQuestion,
		Anchor: &lorev1.Anchor{
			Kinds: []lorev1.AnchorKind{
				lorev1.AnchorKind_ANCHOR_KIND_QUERY,
				lorev1.AnchorKind_ANCHOR_KIND_CODE_SPAN,
				lorev1.AnchorKind_ANCHOR_KIND_DOCUMENT,
				lorev1.AnchorKind_ANCHOR_KIND_TIME_WINDOW,
			},
			Query: rpcQuestion,
			Code: &lorev1.CodeAnchor{
				Repo:       "acme/lore",
				File:       "internal/repositories/sqlite/store.go",
				LineStart:  12,
				LineEnd:    48,
				BlamedShas: []string{"abc1234", "def5678"},
			},
			Doc: &lorev1.DocRef{
				Id:        "github:pr:42",
				Title:     "Switch the store to postgres",
				Url:       "https://github.com/acme/lore/pull/42",
				CreatedAt: timestamppb.New(rpcCreatedAt),
			},
			Window: &lorev1.TimeWindow{
				From:       timestamppb.New(rpcCreatedAt.AddDate(0, 0, -30)),
				To:         timestamppb.New(rpcCreatedAt),
				Derivation: "date 2025-03-12 +/- 30d",
				AnchoredBy: "jira:ticket:PROJ-1",
			},
		},
		Nodes: []*lorev1.EvidenceNode{
			{
				Doc: &lorev1.DocumentMeta{
					Id:        "github:pr:42",
					Source:    "github",
					Type:      string(entities.DocTypePR),
					Title:     "Switch the store to postgres",
					Author:    "ada",
					Url:       "https://github.com/acme/lore/pull/42",
					CreatedAt: timestamppb.New(rpcCreatedAt),
					UpdatedAt: timestamppb.New(rpcCreatedAt),
				},
				Excerpt: "we need transactional DDL",
				Role:    lorev1.NodeRole_NODE_ROLE_SEED,
				Score:   0.5,
				Via: []*lorev1.Edge{{
					Src:        "github:pr:42",
					Dst:        "jira:ticket:PROJ-1",
					Kind:       lorev1.EdgeKind_EDGE_KIND_PR_CLOSES_ISSUE,
					Confidence: 1,
				}},
			},
			{
				Doc: &lorev1.DocumentMeta{
					Id:        "jira:ticket:PROJ-1",
					Source:    "jira",
					Type:      string(entities.DocTypeTicket),
					Title:     "Pick the primary store",
					Author:    "grace",
					Url:       "https://jira.test/browse/PROJ-1",
					CreatedAt: timestamppb.New(rpcCreatedAt.AddDate(0, 0, -10)),
				},
				Excerpt: "mysql cannot do transactional DDL",
				Role:    lorev1.NodeRole_NODE_ROLE_LINKED_TICKET,
				Score:   0.25,
			},
		},
		Chains: []*lorev1.Chain{{DocIds: []string{"jira:ticket:PROJ-1", "github:pr:42"}}},
		Gaps:   []string{"trail ends at PROJ-1; no linked follow-up"},
	}
}

func TestFindDecisionRoundTripsEveryBundleField(t *testing.T) {
	f := newRPCFixture(t)
	f.query.EXPECT().
		FindDecision(gomock.Any(), services.FindDecisionRequest{
			Question: rpcQuestion,
			Around:   "the outage",
			Source:   "github",
			Repo:     "acme/lore",
			DocType:  "pr",
			Since:    rpcCreatedAt.AddDate(0, 0, -30),
			Until:    rpcCreatedAt,
		}).
		Return(rpcBundle(), nil)

	res, err := f.findDecision(t, &lorev1.FindDecisionRequest{
		Question: rpcQuestion,
		Around:   "the outage",
		Source:   "github",
		Repo:     "acme/lore",
		DocType:  "pr",
		Since:    timestamppb.New(rpcCreatedAt.AddDate(0, 0, -30)),
		Until:    timestamppb.New(rpcCreatedAt),
	})
	if err != nil {
		t.Fatalf("FindDecision() = %v, want a bundle", err)
	}

	assertSameProto(t, res.GetBundle(), rpcBundleProto())
	if updated := res.GetBundle().GetNodes()[1].GetDoc().GetUpdatedAt(); updated != nil {
		t.Errorf("nodes[1].doc.updated_at = %v, want it absent for a never-updated document", updated)
	}
}

func TestFindDecisionOmitsAnUnsetTimeWindow(t *testing.T) {
	f := newRPCFixture(t)
	f.query.EXPECT().
		FindDecision(gomock.Any(), services.FindDecisionRequest{Question: rpcQuestion}).
		Return(&entities.EvidenceBundle{Question: rpcQuestion}, nil)

	res, err := f.findDecision(t, &lorev1.FindDecisionRequest{Question: rpcQuestion})
	if err != nil {
		t.Fatalf("FindDecision() = %v, want a bundle", err)
	}

	assertSameProto(t, res.GetBundle(), &lorev1.EvidenceBundle{Question: rpcQuestion, Anchor: &lorev1.Anchor{}})
}

func TestFindDecisionMapsServiceErrorsToStatusCodes(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantCode    codes.Code
		wantMessage string
	}{
		{
			name:        "bad request",
			err:         internalerror.NewBadRequestError("question must not be empty", nil),
			wantCode:    codes.InvalidArgument,
			wantMessage: "question must not be empty",
		},
		{
			name:        "not found",
			err:         internalerror.NewNotFoundError("no document matches ref PROJ-4521", nil),
			wantCode:    codes.NotFound,
			wantMessage: "no document matches ref PROJ-4521",
		},
		{
			name:        "precondition keeps its remediation verbatim",
			err:         internalerror.NewPreconditionError("embedder identity mismatch - run `lore sync --reembed`", nil),
			wantCode:    codes.FailedPrecondition,
			wantMessage: "embedder identity mismatch - run `lore sync --reembed`",
		},
		{
			name:        "internal hides the cause",
			err:         internalerror.NewInternalError("vector search failed", errors.New(rpcCause)),
			wantCode:    codes.Internal,
			wantMessage: transport.InternalErrorMessage,
		},
		{
			name:        "unclassified hides the cause",
			err:         errors.New(rpcCause),
			wantCode:    codes.Internal,
			wantMessage: transport.InternalErrorMessage,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRPCFixture(t)
			f.query.EXPECT().FindDecision(gomock.Any(), gomock.Any()).Return(nil, tt.err)

			_, err := f.findDecision(t, &lorev1.FindDecisionRequest{Question: rpcQuestion})

			st := rpcStatus(t, err)
			if st.Code() != tt.wantCode {
				t.Errorf("code = %v, want %v", st.Code(), tt.wantCode)
			}
			if st.Message() != tt.wantMessage {
				t.Errorf("message = %q, want %q", st.Message(), tt.wantMessage)
			}
		})
	}
}

func TestFindDecisionLogsAnUnclassifiedCauseInsteadOfSendingIt(t *testing.T) {
	f := newRPCFixture(t)
	f.query.EXPECT().FindDecision(gomock.Any(), gomock.Any()).Return(nil, errors.New(rpcCause))

	_, err := f.findDecision(t, &lorev1.FindDecisionRequest{Question: rpcQuestion})

	st := rpcStatus(t, err)
	if st.Code() != codes.Internal {
		t.Errorf("code = %v, want %v", st.Code(), codes.Internal)
	}
	if strings.Contains(st.Message(), rpcCause) {
		t.Errorf("message %q leaks the cause", st.Message())
	}
	if details := st.Details(); len(details) != 0 {
		t.Errorf("status carries %d detail(s), want none", len(details))
	}
	logged := f.logs.String()
	if !strings.Contains(logged, rpcCause) {
		t.Errorf("log %q does not record the cause", logged)
	}
	if !strings.Contains(logged, "FindDecision failed") {
		t.Errorf("log %q does not attribute the failure to FindDecision", logged)
	}
}

func TestTraceSendsTheDirectionTheServiceUnderstands(t *testing.T) {
	tests := []struct {
		name      string
		direction lorev1.Direction
		want      string
	}{
		{name: "unspecified leaves the service its own default", direction: lorev1.Direction_DIRECTION_UNSPECIFIED, want: ""},
		{name: "out", direction: lorev1.Direction_DIRECTION_OUT, want: "out"},
		{name: "in", direction: lorev1.Direction_DIRECTION_IN, want: "in"},
		{name: "both", direction: lorev1.Direction_DIRECTION_BOTH, want: "both"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRPCFixture(t)
			f.trace.EXPECT().
				Trace(gomock.Any(), services.TraceRequest{Ref: traceTestRef, Direction: tt.want, Depth: 2}).
				Return(&entities.EvidenceBundle{}, nil)

			if _, err := f.traceRef(t, tt.direction); err != nil {
				t.Fatalf("Trace() = %v, want a bundle", err)
			}
		})
	}
}

func TestTraceLetsTheServiceRejectADirectionOutsideTheEnum(t *testing.T) {
	const unknown = lorev1.Direction(42)

	f := newRPCFixture(t)
	f.trace.EXPECT().
		Trace(gomock.Any(), services.TraceRequest{Ref: traceTestRef, Direction: unknown.String(), Depth: 2}).
		Return(nil, internalerror.NewBadRequestError(
			`direction "42" must be one of "in", "out", "both"`, nil))

	_, err := f.traceRef(t, unknown)

	st := rpcStatus(t, err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want %v", st.Code(), codes.InvalidArgument)
	}
	if !strings.Contains(st.Message(), `direction "42"`) {
		t.Errorf("message = %q, want it to name the rejected direction", st.Message())
	}
}

func TestTraceKeepsTheCandidatesOfAnAmbiguousRef(t *testing.T) {
	candidates := []entities.DocumentMeta{
		{
			ID:    "github:commit:acme/lore/commit/abc1234a",
			Title: "Widen the lease TTL",
			URL:   "https://github.com/acme/lore/commit/abc1234a",
		},
		{
			ID:    "github:commit:acme/lore/commit/abc1234b",
			Title: "Revert the lease TTL",
			URL:   "https://github.com/acme/lore/commit/abc1234b",
		},
	}
	listed := make([]string, len(candidates))
	for i, candidate := range candidates {
		listed[i] = fmt.Sprintf("%s (%s) %s", candidate.ID, candidate.Title, candidate.URL)
	}

	f := newRPCFixture(t)
	f.trace.EXPECT().Trace(gomock.Any(), gomock.Any()).Return(nil,
		internalerror.NewBadRequestError(fmt.Sprintf("ref %q matches %d documents — retry with one of: %s",
			traceTestRef, len(candidates), strings.Join(listed, "; ")), nil))

	_, err := f.traceRef(t, lorev1.Direction_DIRECTION_BOTH)

	st := rpcStatus(t, err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want %v", st.Code(), codes.InvalidArgument)
	}
	for _, candidate := range candidates {
		for _, part := range []string{string(candidate.ID), candidate.Title, candidate.URL} {
			if !strings.Contains(st.Message(), part) {
				t.Errorf("message = %q, want candidate %s identified by %q", st.Message(), candidate.ID, part)
			}
		}
	}
}
