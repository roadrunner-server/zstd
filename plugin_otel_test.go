package zstd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	rrcontext "github.com/roadrunner-server/context"
	"github.com/stretchr/testify/require"
	jprop "go.opentelemetry.io/contrib/propagators/jaeger"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestMiddlewareTracing(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  any
	}{
		{"enabled", "test-tracer"},
		{"missing_key", nil},
		{"invalid_key", 42},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
			ctx, parent := tp.Tracer("test").Start(t.Context(), "parent")
			defer parent.End()
			if tc.key != nil {
				ctx = context.WithValue(ctx, rrcontext.OtelTracerNameKey, tc.key)
			}
			member, err := baggage.NewMember("test", "value")
			require.NoError(t, err)
			bag, err := baggage.New(member)
			require.NoError(t, err)
			ctx = baggage.ContextWithBaggage(ctx, bag)

			called := false
			h := newPlugin(t).Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				spans := exporter.GetSpans()
				sc := trace.SpanContextFromContext(r.Context())
				if tc.name == "enabled" {
					require.Len(t, spans, 1)
					require.Equal(t, "zstd", spans[0].Name)
					require.Equal(t, trace.SpanKindInternal, spans[0].SpanKind)
					require.Equal(t, parent.SpanContext(), spans[0].Parent)
					require.Equal(t, spans[0].SpanContext, sc)
					require.False(t, spans[0].EndTime.IsZero())
					for _, prop := range []propagation.TextMapPropagator{propagation.TraceContext{}, jprop.Jaeger{}} {
						extracted := prop.Extract(context.Background(), propagation.HeaderCarrier(r.Header))
						got := trace.SpanContextFromContext(extracted)
						require.Equal(t, sc.TraceID(), got.TraceID())
						require.Equal(t, sc.SpanID(), got.SpanID())
					}
					require.Equal(t, "test=value", r.Header.Get("Baggage"))
				} else {
					require.Empty(t, spans)
					require.Equal(t, parent.SpanContext(), sc)
					require.Empty(t, r.Header.Get("Traceparent"))
					require.Empty(t, r.Header.Get("Uber-Trace-Id"))
					require.Empty(t, r.Header.Get("Baggage"))
				}
				require.Equal(t, "value", baggage.FromContext(r.Context()).Member("test").Value())
				w.WriteHeader(http.StatusCreated)
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil))
			require.True(t, called)
			require.Equal(t, http.StatusCreated, rec.Code)
		})
	}
}
