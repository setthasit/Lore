package grpc

import (
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"lore/internal/errors/internalerror"
	"lore/internal/transport"
)

var rpcCodes = map[internalerror.Kind]codes.Code{
	internalerror.KindBadRequest:   codes.InvalidArgument,
	internalerror.KindNotFound:     codes.NotFound,
	internalerror.KindPrecondition: codes.FailedPrecondition,
}

func rpcError(log *slog.Logger, method string, err error) error {
	kind, message := transport.Classify(err)
	if code, mapped := rpcCodes[kind]; mapped {
		return status.Error(code, message)
	}

	log.Error(method+" failed", "error", err)

	return status.Error(codes.Internal, message)
}
