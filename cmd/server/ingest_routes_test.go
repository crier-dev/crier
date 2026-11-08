package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

func TestWebhookIngestRoutesAreAbsentWhenGateOff(t *testing.T) {
	baseline := mux.NewRouter()
	gated := mux.NewRouter()
	registerWebhookIngestRoutes(gated, nil)
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
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("disabled route census differs: before=%v after=%v", before, after)
	}
	recorder := httptest.NewRecorder()
	gated.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/ingress/webhooks/x", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("disabled webhook route status=%d", recorder.Code)
	}
}
