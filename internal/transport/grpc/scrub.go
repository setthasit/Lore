package grpc

import (
	"context"

	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protopath"
	"google.golang.org/protobuf/reflect/protorange"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/setthasit/Lore/internal/secrets"
)

func scrubbingInterceptors(sink *secrets.Sink) []grpclib.ServerOption {
	return []grpclib.ServerOption{
		grpclib.UnaryInterceptor(func(ctx context.Context, req any, _ *grpclib.UnaryServerInfo, handler grpclib.UnaryHandler) (any, error) {
			resp, err := handler(ctx, req)
			if err != nil {
				return nil, scrubStatus(sink, err)
			}
			scrubResult(sink, resp)
			return resp, nil
		}),
		grpclib.StreamInterceptor(func(srv any, stream grpclib.ServerStream, _ *grpclib.StreamServerInfo, handler grpclib.StreamHandler) error {
			return scrubStatus(sink, handler(srv, scrubbingStream{ServerStream: stream, sink: sink}))
		}),
	}
}

type scrubbingStream struct {
	grpclib.ServerStream
	sink *secrets.Sink
}

func (s scrubbingStream) SendMsg(m any) error {
	scrubResult(s.sink, m)
	return s.ServerStream.SendMsg(m)
}

func scrubStatus(sink *secrets.Sink, err error) error {
	if err == nil {
		return nil
	}
	st, _ := status.FromError(err)
	scrubbed := sink.Scrub(st.Message())
	if scrubbed == st.Message() {
		return err
	}
	carried := st.Proto()
	carried.Message = scrubbed
	return status.ErrorProto(carried)
}

func scrubResult(sink *secrets.Sink, result any) {
	message, ok := result.(proto.Message)
	if !ok {
		return
	}
	_ = protorange.Range(message.ProtoReflect(), func(path protopath.Values) error {
		last := path.Index(-1)
		text, isString := last.Value.Interface().(string)
		if !isString {
			return nil
		}
		scrubbed := sink.Scrub(text)
		if scrubbed == text {
			return nil
		}
		parent, value := path.Index(-2).Value, protoreflect.ValueOfString(scrubbed)
		switch last.Step.Kind() {
		case protopath.FieldAccessStep:
			parent.Message().Set(last.Step.FieldDescriptor(), value)
		case protopath.ListIndexStep:
			parent.List().Set(last.Step.ListIndex(), value)
		case protopath.MapIndexStep:
			parent.Map().Set(last.Step.MapIndex(), value)
		}
		return nil
	})
}
