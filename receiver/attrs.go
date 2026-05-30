package receiver

import (
	"strconv"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// attrs wraps an OTLP attribute list for tolerant, multi-key lookup. The OTel
// HTTP semantic conventions were renamed as they stabilized (http.method →
// http.request.method, http.status_code → http.response.status_code,
// net.peer.ip → client.address, …), so which keys you receive depends on the
// emitting SDK's semconv version. Every lookup takes a fallback list and returns
// the first key present — this is the semconv-drift shim.
type attrs map[string]*commonpb.AnyValue

func newAttrs(kvs []*commonpb.KeyValue) attrs {
	m := make(attrs, len(kvs))
	for _, kv := range kvs {
		if kv != nil && kv.Value != nil {
			m[kv.Key] = kv.Value
		}
	}
	return m
}

// str returns the first present key's non-empty string value, or "" if none resolve.
func (a attrs) str(keys ...string) string {
	for _, k := range keys {
		if v, ok := a[k]; ok {
			if s := anyStr(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// intOr returns the first present key's int value, or def if none resolve.
func (a attrs) intOr(def int64, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := a[k]; ok {
			if n, ok := anyInt(v); ok {
				return n
			}
		}
	}
	return def
}

func anyStr(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'g', -1, 64)
	default:
		return ""
	}
}

func anyInt(v *commonpb.AnyValue) (int64, bool) {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_IntValue:
		return x.IntValue, true
	case *commonpb.AnyValue_DoubleValue:
		return int64(x.DoubleValue), true
	case *commonpb.AnyValue_StringValue:
		if n, err := strconv.ParseInt(x.StringValue, 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

// serviceName pulls the conventional service.name from resource attributes.
func serviceName(kvs []*commonpb.KeyValue) string {
	return newAttrs(kvs).str("service.name")
}
