package grpc

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/setthasit/Lore/internal/secrets"
)

func TestScrubResultRedactsAMapValueHoldingASecret(t *testing.T) {
	const token = "fake-map-token-8Wd4"
	sink := &secrets.Sink{}
	sink.Record("LORE_FORGE_TOKEN (sources[forge].with.token_env)", token)

	stringField := func(name string, number int32) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name:   proto.String(name),
			Number: proto.Int32(number),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		}
	}
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("scrub_test.proto"),
		Package: proto.String("scrubtest"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Labelled"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:     proto.String("labels"),
				Number:   proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String(".scrubtest.Labelled.LabelsEntry"),
			}},
			NestedType: []*descriptorpb.DescriptorProto{{
				Name:    proto.String("LabelsEntry"),
				Field:   []*descriptorpb.FieldDescriptorProto{stringField("key", 1), stringField("value", 2)},
				Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
			}},
		}},
	}, nil)
	if err != nil {
		t.Fatalf("build the map-bearing descriptor: %v", err)
	}
	labelled := dynamicpb.NewMessage(file.Messages().Get(0))
	labels := labelled.Mutable(labelled.Descriptor().Fields().ByName("labels")).Map()
	upstream := protoreflect.ValueOfString("upstream").MapKey()
	labels.Set(upstream, protoreflect.ValueOfString("x "+token+" x"))

	scrubResult(sink, labelled)

	if got, want := labels.Get(upstream).String(), "x "+secrets.Placeholder+" x"; got != want {
		t.Errorf("labels[upstream] = %q, want %q", got, want)
	}
}
