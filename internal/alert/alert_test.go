package alert

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

func TestWebhookLogsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	Webhook(srv.URL)(context.Background(), "hi")
	if !strings.Contains(buf.String(), "alert webhook: 500") {
		t.Fatalf("log = %q", buf.String())
	}
}
