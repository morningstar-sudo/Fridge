// Package alert posts failure notices to a webhook.
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// Webhook returns a notifier; empty url means log only. Errors are only logged.
func Webhook(url string) func(ctx context.Context, text string) {
	return func(ctx context.Context, text string) {
		log.Printf("alert: %s", text)
		if url == "" {
			return
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		body, _ := json.Marshal(map[string]string{"text": text})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			log.Printf("alert webhook: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Printf("alert webhook: %v", err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("alert webhook: %s", resp.Status)
		}
	}
}
