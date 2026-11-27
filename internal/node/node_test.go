package node

import (
	"net/http/httptest"
	"testing"

	"github.com/srawlin/offload/internal/proto"
)

func TestHealthReportsVersion(t *testing.T) {
	s, err := New(t.TempDir(), "token")
	if err != nil {
		t.Fatal(err)
	}
	s.Version = "v1.2.3"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/health", nil))
	if rec.Code != 200 || rec.Header().Get(proto.HeaderVersion) != "v1.2.3" || rec.Body.String() != "ok\n" {
		t.Fatalf("got %d %q %q", rec.Code, rec.Header().Get(proto.HeaderVersion), rec.Body.String())
	}
}
