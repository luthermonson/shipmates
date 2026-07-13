package fleetsteer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type localControlRoundTrip func(*http.Request) (*http.Response, error)

func (f localControlRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func targetQueryResponse(status int, scope, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"X-Shipmates-Project": []string{scope}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestLocalControlTargetsPreservesBoundedFailureReasons(t *testing.T) {
	tests := []struct {
		name   string
		ctx    context.Context
		base   string
		result *http.Response
		err    error
		want   LocalTargetQueryReason
	}{
		{name: "request construction", ctx: nil, base: "http://local", want: TargetQueryRequestConstruction},
		{name: "connection", ctx: context.Background(), base: "http://local", err: errors.New("private transport detail"), want: TargetQueryConnection},
		{name: "auth", ctx: context.Background(), base: "http://local", result: targetQueryResponse(http.StatusUnauthorized, "", `{}`), want: TargetQueryAuth},
		{name: "http status", ctx: context.Background(), base: "http://local", result: targetQueryResponse(http.StatusNotFound, "", `{}`), want: TargetQueryHTTPStatus},
		{name: "json decode", ctx: context.Background(), base: "http://local", result: targetQueryResponse(http.StatusOK, "scope", `{`), want: TargetQueryJSONDecode},
		{name: "response header scope", ctx: context.Background(), base: "http://local", result: targetQueryResponse(http.StatusOK, "other", `{}`), want: TargetQueryScope},
		{name: "response body scope", ctx: context.Background(), base: "http://local", result: targetQueryResponse(http.StatusOK, "scope", `{"schema_version":1,"project_scope":"other","targets":[]}`), want: TargetQueryScope},
		{name: "scope status", ctx: context.Background(), base: "http://local", result: targetQueryResponse(http.StatusConflict, "", `{}`), want: TargetQueryScope},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			control := &LocalControl{baseURL: tt.base, token: "secret", scope: "scope", client: &http.Client{Transport: localControlRoundTrip(func(r *http.Request) (*http.Response, error) {
				return tt.result, tt.err
			})}}
			_, err := control.Targets(tt.ctx)
			var queryErr *LocalTargetQueryError
			if !errors.As(err, &queryErr) || queryErr.Reason != tt.want {
				t.Fatalf("err=%v reason=%v, want %v", err, queryErr, tt.want)
			}
			if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), tt.base) {
				t.Fatalf("unbounded error detail: %q", err)
			}
		})
	}
}

func TestLocalControlTargetsSendsAuthAndScopeAndAcceptsSnapshot(t *testing.T) {
	control := &LocalControl{baseURL: "http://local", token: "secret", scope: "scope", client: &http.Client{Transport: localControlRoundTrip(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("authorization=%q", got)
		}
		if got := r.Header.Get("X-Shipmates-Project"); got != "scope" {
			t.Fatalf("scope=%q", got)
		}
		return targetQueryResponse(http.StatusOK, "scope", `{"schema_version":1,"project_scope":"scope","targets":[]}`), nil
	})}}
	targets, err := control.Targets(context.Background())
	if err != nil || len(targets) != 0 {
		t.Fatalf("targets=%v err=%v", targets, err)
	}
}
