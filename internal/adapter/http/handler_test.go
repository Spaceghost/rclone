package httpadapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Spaceghost/rclone-projection-vfs/internal/model"
)

type stubResolver struct{}

func (stubResolver) Resolve(_ context.Context, projectionPath string) (model.Resolution, error) {
	return model.Resolution{Path: projectionPath, RouteName: "test"}, nil
}

func TestResolveEndpoint(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/resolve?path=/hello.txt", nil)
	NewHandler(stubResolver{}).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
}
