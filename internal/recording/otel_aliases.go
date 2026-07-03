package recording

import (
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// Aliases for the OTLP common types so reader.go can flatten span attributes
// without repeating the deep package name everywhere. Keeping them in their
// own file makes the reader's intent clearer.
type (
	otelCommonAny       = commonpb.AnyValue
	otelCommonAnyString = commonpb.AnyValue_StringValue
	otelCommonAnyBool   = commonpb.AnyValue_BoolValue
	otelCommonAnyInt    = commonpb.AnyValue_IntValue
	otelCommonAnyDouble = commonpb.AnyValue_DoubleValue
)
