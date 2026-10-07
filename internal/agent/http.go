package agent

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/rajeev-chaurasia/benchgrid/internal/wire"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/runs", func(w http.ResponseWriter, r *http.Request) {
		var d wire.Dispatch
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&d); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := d.Spec.Validate(); err != nil || d.Attempt < 1 || d.Fence < 1 {
			http.Error(w, "invalid dispatch", http.StatusBadRequest)
			return
		}
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		reply := a.AcceptContext(ctx, d)
		code := http.StatusAccepted
		switch {
		case reply.Duplicate:
			code = http.StatusOK
		case !reply.Accepted:
			code = http.StatusConflict
		}
		writeJSON(w, code, reply)
	})
	mux.HandleFunc("GET /v1/runs/{experiment}/{attempt}", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(r.PathValue("attempt"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		st, ok := a.Status(r.PathValue("experiment"), n)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.Snapshot())
	})
	mux.HandleFunc("POST /v1/unquarantine", func(w http.ResponseWriter, r *http.Request) {
		a.Unquarantine()
		writeJSON(w, http.StatusOK, a.Snapshot())
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
