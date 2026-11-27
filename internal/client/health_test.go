package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srawlin/offload/internal/proto"
)

func TestHealthReadsNodeVersion(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		status       int
		want         string
		wantErr      bool
	}{
		{"current node", "v0.2.0", 200, "v0.2.0", false},
		{"older node without the header", "", 200, "", false},
		{"proxy in front answering for a dead node", "", 502, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.header != "" {
					w.Header().Set(proto.HeaderVersion, tc.header)
				}
				w.WriteHeader(tc.status)
				fmt.Fprintln(w, "ok")
			}))
			defer srv.Close()
			_, v, err := Health(context.Background(), Config{Node: srv.URL})
			if (err != nil) != tc.wantErr || v != tc.want {
				t.Fatalf("Health() = %q, %v", v, err)
			}
		})
	}
}
