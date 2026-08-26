package httpadapter

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Spaceghost/rclone-projection-vfs/internal/resolver"
)

func NewHandler(r resolver.Resolver) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /v1/resolve", func(w http.ResponseWriter, request *http.Request) {
		projectionPath := request.URL.Query().Get("path")
		if projectionPath == "" {
			writeError(w, http.StatusBadRequest, "query parameter path is required")
			return
		}
		resolution, err := r.Resolve(request.Context(), projectionPath)
		if errors.Is(err, resolver.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "resolver failed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resolution)
	})
	return mux
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
