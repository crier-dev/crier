package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/middleware"
	"github.com/gorilla/mux"
)

func TestWebhookIngestRoutesAreAbsentWhenGateOff(t *testing.T) {
	baseline := mux.NewRouter()
	gated := mux.NewRouter()
	registerWebhookIngestRoutes(gated, nil, "access-token")
	var before, after []string
	if err := baseline.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		path, _ := route.GetPathTemplate()
		methods, _ := route.GetMethods()
		before = append(before, path+":"+strings.Join(methods, ","))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := gated.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		path, _ := route.GetPathTemplate()
		methods, _ := route.GetMethods()
		after = append(after, path+":"+strings.Join(methods, ","))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(before, after) {
		t.Fatal("disabled routes must retain a named-gate response route")
	}
	recorder := httptest.NewRecorder()
	gated.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/ingress/webhooks/x", nil))
	if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "CR_INGEST_WEBHOOK_ENABLED") {
		t.Fatalf("disabled webhook route status=%d body=%s; want 404 naming CR_INGEST_WEBHOOK_ENABLED", recorder.Code, recorder.Body.String())
	}
	source := httptest.NewRecorder()
	secured := middleware.AuthTokensWithWebhookBypass("access-token", "")(gated)
	secured.ServeHTTP(source, httptest.NewRequest(http.MethodGet, "/ingress/sources", nil))
	if source.Code != http.StatusNotFound || !strings.Contains(source.Body.String(), "CR_INGEST_WEBHOOK_ENABLED") {
		t.Fatalf("disabled source route status=%d body=%s; want 404 naming CR_INGEST_WEBHOOK_ENABLED", source.Code, source.Body.String())
	}
}

func TestWebhookIngestRouteAuthPostureWhenEnabled(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ingress/webhooks/build-bot" && r.Header.Get("X-Crier-Source-ID") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"source authentication required"}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	router := mux.NewRouter()
	registerWebhookIngestRoutes(router, handler, "access-token")

	source := httptest.NewRecorder()
	middleware.AuthTokens("access-token", "")(router).ServeHTTP(source, httptest.NewRequest(http.MethodGet, "/ingress/sources", nil))
	if source.Code != http.StatusUnauthorized {
		t.Fatalf("enabled source route status=%d body=%s; want bearer auth refusal", source.Code, source.Body.String())
	}

	webhook := httptest.NewRecorder()
	webhookReq := httptest.NewRequest(http.MethodPost, "/ingress/webhooks/build-bot", nil)
	middleware.AuthTokensWithWebhookBypass("access-token", "")(router).ServeHTTP(webhook, webhookReq)
	if webhook.Code != http.StatusUnauthorized || !strings.Contains(webhook.Body.String(), "source authentication required") {
		t.Fatalf("enabled webhook route status=%d body=%s; want source auth refusal", webhook.Code, webhook.Body.String())
	}

	accepted := httptest.NewRecorder()
	acceptedReq := httptest.NewRequest(http.MethodPost, "/ingress/webhooks/build-bot", nil)
	acceptedReq.Header.Set("X-Crier-Source-ID", "build-bot")
	middleware.AuthTokensWithWebhookBypass("access-token", "")(router).ServeHTTP(accepted, acceptedReq)
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("enabled webhook route with source auth status=%d body=%s; want 202", accepted.Code, accepted.Body.String())
	}
}
