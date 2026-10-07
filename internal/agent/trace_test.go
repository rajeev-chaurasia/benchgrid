package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rajeev-chaurasia/benchgrid/internal/probe"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// A dispatch that carries a trace context must produce a session in that
// same trace, with one child span per phase, or the trace stops at the edge
// of the scheduler and says nothing about where an attempt spent its time.
func TestDispatchJoinsTheSchedulersTrace(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(prevTP); otel.SetTextMapPropagator(prevProp) })

	a, _ := newAgent(t, true, &probe.Profile{})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	ctx, parent := tp.Tracer("test").Start(context.Background(), "dispatch")
	body, _ := json.Marshal(dispatch("exp_tr", 1, 1, testSpec("-rounds", "10")))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/runs", bytes.NewReader(body))
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("%v %v", resp, err)
	}
	parent.End()
	waitDone(t, a, "exp_tr", 1)

	want := parent.SpanContext().TraceID()
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range rec.Ended() {
		byName[s.Name()] = s
	}
	session, ok := byName["session"]
	if !ok || session.SpanContext().TraceID() != want || session.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Fatalf("session span not in the dispatch's trace: %v", byName)
	}
	for _, phase := range []string{"preflight", "running", "cleanup", "collecting"} {
		s, ok := byName[phase]
		if !ok || s.Parent().SpanID() != session.SpanContext().SpanID() {
			t.Errorf("phase %s missing or not under the session", phase)
		}
	}
}
