package rustfs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// WebhookEvent is a decoded, simplified view of one RustFS S3 event
// notification record (docs.rustfs.com/en/operations/event-notifications).
type WebhookEvent struct {
	EventName string // e.g. "s3:ObjectCreated:Put", "s3:ObjectRemoved:Delete"
	Bucket    string
	Key       string // URL-decoded object key
}

// IsCreate reports whether this event is an ObjectCreated variant (Put,
// Copy, CompleteMultipartUpload, ...).
func (e WebhookEvent) IsCreate() bool { return strings.HasPrefix(e.EventName, "s3:ObjectCreated:") }

// IsRemove reports whether this event is an ObjectRemoved variant
// (Delete, DeleteMarkerCreated, ...).
func (e WebhookEvent) IsRemove() bool { return strings.HasPrefix(e.EventName, "s3:ObjectRemoved:") }

// webhookPayload mirrors RustFS's documented webhook JSON body. RustFS
// may batch several events into one delivery (multiple Records entries).
type webhookPayload struct {
	Records []struct {
		EventName string `json:"eventName"`
		S3        struct {
			Bucket struct {
				Name string `json:"name"`
			} `json:"bucket"`
			Object struct {
				Key string `json:"key"`
			} `json:"object"`
		} `json:"s3"`
	} `json:"Records"`
}

// ParseWebhookPayload decodes a raw RustFS webhook POST body into zero or
// more WebhookEvents. Object keys are URL-decoded per RustFS's documented
// payload format (keys are delivered percent-encoded, e.g.
// "uploads%2Fhello.dat" for "uploads/hello.dat"). Kept separate from the
// HTTP handling in ServeWebhook so it can be unit tested with canned
// payload bytes instead of a real HTTP round-trip.
func ParseWebhookPayload(body []byte) ([]WebhookEvent, error) {
	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("decode webhook payload: %w", err)
	}
	events := make([]WebhookEvent, 0, len(p.Records))
	for _, r := range p.Records {
		key, err := url.QueryUnescape(r.S3.Object.Key)
		if err != nil {
			key = r.S3.Object.Key // malformed escape -- use the raw key rather than dropping the event
		}
		events = append(events, WebhookEvent{
			EventName: r.EventName,
			Bucket:    r.S3.Bucket.Name,
			Key:       key,
		})
	}
	return events, nil
}

// ServeWebhook starts an HTTP server on addr that accepts RustFS bucket-
// notification webhook POSTs (sync-2.1-design.info section 3.1/11) and
// calls onEvent once per decoded record. Blocks until ctx is cancelled or
// the listener errors -- run it in its own goroutine.
//
// Idempotency: RustFS's own docs state "a receiver can observe the same
// event more than once" (queued store-and-forward delivery) -- onEvent
// implementations should tolerate duplicate calls for the same event.
// package rustfs's own CopyTo/CopyFrom/Delete are naturally idempotent
// (an rclone copyto/deletefile of something already in the target state
// is a safe no-op), so wiring onEvent straight to those is safe as-is.
//
// A plain GET/HEAD to any path returns 200 with no processing -- this is
// what satisfies RustFS's own delivery health check (a probe to the
// configured endpoint's origin root before it starts sending real
// events), without needing a body or JSON parsing for that request.
//
// bearerToken, if non-empty, requires a matching "Authorization: Bearer
// <token>" header on POSTs (RustFS's webhook config supports sending
// one, RUSTFS_NOTIFY_WEBHOOK_AUTH_TOKEN_* per the docs) -- requests
// without it are rejected with 401.
//
// allowIP (v3.0, --allow_ip), if non-empty, restricts POSTs to that
// single source IP (checked against r.RemoteAddr's host part, same
// single-IP-allowlist model as remote.Receiver.Serve's --allow-ip) --
// requests from any other address are rejected with 403 before the body
// is even read. Empty = unrestricted (GET/HEAD health-check probes are
// never IP-filtered either way, matching remote.Receiver's existing
// convention of only gating the actual transfer, not liveness checks).
func ServeWebhook(ctx context.Context, addr string, bearerToken string, allowIP string, onEvent func(WebhookEvent)) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if allowIP != "" {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			if host != allowIP {
				http.Error(w, "forbidden: source IP not allowed", http.StatusForbidden)
				return
			}
		}
		if bearerToken != "" && r.Header.Get("Authorization") != "Bearer "+bearerToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10MB cap, defensive
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		events, err := ParseWebhookPayload(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, e := range events {
			onEvent(e)
		}
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
