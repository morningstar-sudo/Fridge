package alert

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWebhookPostsText(t *testing.T) {
	var body, ct string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body, ct = string(b), r.Header.Get("Content-Type")
	}))
	defer srv.Close()
	Webhook(srv.URL)(context.Background(), "hi")
	if body != `{"text":"hi"}` || ct != "application/json" {
		t.Fatalf("body=%q ct=%q", body, ct)
	}
}
