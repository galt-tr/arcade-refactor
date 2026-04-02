package merkleservice

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegister(t *testing.T) {
	var gotBody map[string]string
	var gotAuth string
	var gotPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(server.URL, "mytoken", 0)
	err := client.Register(context.Background(), "abc123", "http://callback/url")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/watch" {
		t.Errorf("expected /watch, got %s", gotPath)
	}
	if gotAuth != "Bearer mytoken" {
		t.Errorf("expected Bearer mytoken, got %s", gotAuth)
	}
	if gotBody["txid"] != "abc123" {
		t.Errorf("expected txid abc123, got %s", gotBody["txid"])
	}
	if gotBody["callbackUrl"] != "http://callback/url" {
		t.Errorf("expected callbackUrl, got %s", gotBody["callbackUrl"])
	}
}

func TestRegister_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewClient(server.URL, "", 0)
	err := client.Register(context.Background(), "abc123", "http://callback")
	if err == nil {
		t.Error("expected error for 500 response")
	}
}
