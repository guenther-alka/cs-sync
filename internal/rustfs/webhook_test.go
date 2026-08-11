package rustfs

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseWebhookPayload_SingleRecord(t *testing.T) {
	// exact shape from docs.rustfs.com/en/operations/event-notifications
	body := []byte(`{
		"EventName": "s3:ObjectCreated:Put",
		"Records": [
			{
				"eventName": "s3:ObjectCreated:Put",
				"s3": {
					"bucket": { "name": "my-bucket" },
					"object": { "key": "uploads%2Fhello.dat" }
				}
			}
		]
	}`)
	events, err := ParseWebhookPayload(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := events[0]
	if e.EventName != "s3:ObjectCreated:Put" {
		t.Errorf("EventName = %q", e.EventName)
	}
	if e.Bucket != "my-bucket" {
		t.Errorf("Bucket = %q", e.Bucket)
	}
	if e.Key != "uploads/hello.dat" {
		t.Errorf("Key = %q, want url-decoded \"uploads/hello.dat\"", e.Key)
	}
	if !e.IsCreate() {
		t.Error("expected IsCreate() == true")
	}
	if e.IsRemove() {
		t.Error("expected IsRemove() == false")
	}
}

func TestParseWebhookPayload_DeleteEvent(t *testing.T) {
	body := []byte(`{"Records":[{"eventName":"s3:ObjectRemoved:Delete","s3":{"bucket":{"name":"b"},"object":{"key":"rufus-3.20p.exe"}}}]}`)
	events, err := ParseWebhookPayload(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !events[0].IsRemove() || events[0].IsCreate() {
		t.Errorf("got %+v, expected exactly one ObjectRemoved event", events)
	}
}

func TestParseWebhookPayload_MultipleRecords(t *testing.T) {
	// RustFS may batch several events into one delivery.
	body := []byte(`{"Records":[
		{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"b"},"object":{"key":"a.txt"}}},
		{"eventName":"s3:ObjectRemoved:Delete","s3":{"bucket":{"name":"b"},"object":{"key":"b.txt"}}}
	]}`)
	events, err := ParseWebhookPayload(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
}

func TestParseWebhookPayload_Empty(t *testing.T) {
	events, err := ParseWebhookPayload([]byte(`{"Records":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("expected zero events, got %+v", events)
	}
}

func TestParseWebhookPayload_InvalidJSON(t *testing.T) {
	_, err := ParseWebhookPayload([]byte(`not json`))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseWebhookPayload_MalformedEscapeFallsBackToRawKey(t *testing.T) {
	// "%zz" is not a valid percent-escape -- QueryUnescape errors, and we
	// must fall back to the raw key rather than dropping the event.
	body := []byte(`{"Records":[{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"b"},"object":{"key":"bad%zzkey"}}}]}`)
	events, err := ParseWebhookPayload(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Key != "bad%zzkey" {
		t.Errorf("got %+v, want raw key fallback", events)
	}
}

func TestServeWebhook_EndToEnd(t *testing.T) {
	received := make(chan WebhookEvent, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := "127.0.0.1:18089" // fixed test port, unlikely to collide
	go func() {
		_ = ServeWebhook(ctx, addr, "", "", func(e WebhookEvent) { received <- e })
	}()
	time.Sleep(100 * time.Millisecond) // let the listener come up

	// health-check GET, per RustFS's own delivery health check pattern
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET health check: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET health check: got status %d, want 200", resp.StatusCode)
	}

	body := `{"Records":[{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"s3share"},"object":{"key":"rufus-3.20p.exe"}}}]}`
	resp2, err := http.Post("http://"+addr+"/", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("POST: got status %d, want 200", resp2.StatusCode)
	}

	select {
	case e := <-received:
		if e.Bucket != "s3share" || e.Key != "rufus-3.20p.exe" || !e.IsCreate() {
			t.Errorf("unexpected event: %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook event to be delivered")
	}
}

func TestServeWebhook_RejectsMissingBearerToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := "127.0.0.1:18090"
	go func() { _ = ServeWebhook(ctx, addr, "secret-token", "", func(WebhookEvent) {}) }()
	time.Sleep(100 * time.Millisecond)

	body := `{"Records":[]}`
	resp, err := http.Post("http://"+addr+"/", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("got status %d, want 401 (no Authorization header sent)", resp.StatusCode)
	}
}

func TestServeWebhook_AcceptsCorrectBearerToken(t *testing.T) {
	received := make(chan WebhookEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := "127.0.0.1:18091"
	go func() {
		_ = ServeWebhook(ctx, addr, "secret-token", "", func(e WebhookEvent) { received <- e })
	}()
	time.Sleep(100 * time.Millisecond)

	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/", strings.NewReader(`{"Records":[{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"b"},"object":{"key":"x"}}}]}`))
	req.Header.Set("Authorization", "Bearer secret-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("got status %d, want 200 (correct token sent)", resp.StatusCode)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestServeWebhook_AllowIPRejectsOtherSource(t *testing.T) {
	// A real client connecting from 127.0.0.1 but allowIP set to some
	// other address must be rejected with 403 -- this is the v3.0
	// --allow_ip filter (Gea 2026.08.11: "soll pull/receiver auf diese
	// push/send begrenzen, egal ob cs-sync oder RustFS Gegenstelle").
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := "127.0.0.1:18092"
	go func() { _ = ServeWebhook(ctx, addr, "", "192.0.2.1", func(WebhookEvent) {}) }()
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Post("http://"+addr+"/", "application/json", strings.NewReader(`{"Records":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("got status %d, want 403 (source 127.0.0.1 != allowIP 192.0.2.1)", resp.StatusCode)
	}
}

func TestServeWebhook_AllowIPAcceptsMatchingSource(t *testing.T) {
	received := make(chan WebhookEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := "127.0.0.1:18093"
	go func() {
		_ = ServeWebhook(ctx, addr, "", "127.0.0.1", func(e WebhookEvent) { received <- e })
	}()
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Post("http://"+addr+"/", "application/json",
		strings.NewReader(`{"Records":[{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"b"},"object":{"key":"x"}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("got status %d, want 200 (source 127.0.0.1 == allowIP)", resp.StatusCode)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}
