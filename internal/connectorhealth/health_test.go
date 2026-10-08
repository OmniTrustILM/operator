/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connectorhealth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	v2Path = "/v2/health"
	v1Path = "/v1/health"
)

// conditionMessageLimit is metav1.Condition's maxLength for a message.
const conditionMessageLimit = 32768

// answer is one canned response of a fake connector.
type answer struct {
	status int
	body   string
}

// fakeConnector serves canned answers by path and records each path asked.
type fakeConnector struct {
	answers map[string]answer
	delay   time.Duration

	mu    sync.Mutex
	asked []string
}

func (f *fakeConnector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.asked = append(f.asked, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-r.Context().Done():
			return
		}
	}
	a, ok := f.answers[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(a.status)
	_, _ = w.Write([]byte(a.body))
}

func (f *fakeConnector) askedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// check runs one Check against connector over the production client.
func check(t *testing.T, connector http.Handler, paths ...string) Verdict {
	t.Helper()
	server := httptest.NewServer(connector)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return Check(ctx, NewHTTPClient(), server.URL, paths)
}

func TestCheckReadsTheReportedStatus(t *testing.T) {
	tests := []struct {
		name        string
		answer      answer
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantNamed   []string
		wantUnnamed []string
		wantInOrder []string
	}{
		{
			name:       "v2 UP",
			answer:     answer{http.StatusOK, `{"status":"UP","components":{"liveness":{"status":"UP"},"readiness":{"status":"UP"}}}`},
			wantStatus: metav1.ConditionTrue,
			wantReason: ReasonUp,
		},
		{
			name: "v2 DEGRADED names only the components outside UP",
			answer: answer{http.StatusOK, `{"status":"DEGRADED","components":{"liveness":{"status":"UP"},` +
				`"hsm-a":{"status":"DOWN","details":{"error":"Timeout connecting to hsm.internal:1792"}},"hsm-b":{"status":"UP"}}}`},
			wantStatus:  metav1.ConditionFalse,
			wantReason:  ReasonDegraded,
			wantNamed:   []string{"hsm-a", "DOWN"},
			wantUnnamed: []string{"hsm-b", "liveness", "hsm.internal", "Timeout"},
		},
		{
			name:       "v2 DOWN arrives as a 503 with the report",
			answer:     answer{http.StatusServiceUnavailable, `{"status":"DOWN","components":{"keystore":{"status":"DOWN"},"database":{"status":"UP"}}}`},
			wantStatus: metav1.ConditionFalse,
			wantReason: ReasonDown,
			wantNamed:  []string{"keystore"},
		},
		{
			name:       "v2 OUT_OF_SERVICE",
			answer:     answer{http.StatusServiceUnavailable, `{"status":"OUT_OF_SERVICE","components":{"readiness":{"status":"OUT_OF_SERVICE"}}}`},
			wantStatus: metav1.ConditionFalse,
			wantReason: ReasonOutOfService,
			wantNamed:  []string{"readiness", "OUT_OF_SERVICE"},
		},
		{
			name:       "v2 UNKNOWN is a report the connector made",
			answer:     answer{http.StatusOK, `{"status":"UNKNOWN","components":{"database":{"status":"UNKNOWN"}}}`},
			wantStatus: metav1.ConditionFalse,
			wantReason: ReasonUnknown,
			wantNamed:  []string{"database"},
		},
		{
			name: "Spring actuator body with groups and details",
			answer: answer{http.StatusOK, `{"status":"UP","groups":["liveness","readiness"],"components":{"db":{"status":"UP",` +
				`"details":{"database":"PostgreSQL","validationQuery":"isValid()"}},"ssl":{"status":"UP","details":{"validChains":[],"invalidChains":[]}}}}`},
			wantStatus: metav1.ConditionTrue,
			wantReason: ReasonUp,
		},
		{
			name:       "null components",
			answer:     answer{http.StatusOK, `{"status":"UP","components":null}`},
			wantStatus: metav1.ConditionTrue,
			wantReason: ReasonUp,
		},
		{
			name:       "DOWN with null components",
			answer:     answer{http.StatusServiceUnavailable, `{"status":"DOWN","components":null}`},
			wantStatus: metav1.ConditionFalse,
			wantReason: ReasonDown,
			wantNamed:  []string{"DOWN"},
		},
		{
			name:       "null details",
			answer:     answer{http.StatusOK, `{"status":"UP","components":{"liveness":{"status":"UP","details":null},"database":{"status":"UP","details":null}}}`},
			wantStatus: metav1.ConditionTrue,
			wantReason: ReasonUp,
		},
		{
			name: "components are listed in name order",
			answer: answer{http.StatusServiceUnavailable, `{"status":"DOWN","components":{` +
				`"charlie":{"status":"DOWN"},"alpha":{"status":"DOWN"},"bravo":{"status":"DOWN"}}}`},
			wantStatus:  metav1.ConditionFalse,
			wantReason:  ReasonDown,
			wantInOrder: []string{"alpha", "bravo", "charlie"},
		},
		{
			name:       "v1 ok",
			answer:     answer{http.StatusOK, `{"status":"ok","description":null,"parts":null}`},
			wantStatus: metav1.ConditionTrue,
			wantReason: ReasonUp,
		},
		{
			name: "v1 nok names each failing part by name and status alone",
			answer: answer{http.StatusOK, `{"status":"nok","description":"Database down","parts":{` +
				`"database":{"status":"nok","description":"Connection to jdbc:postgresql://db.internal:5432 refused","parts":null},"cache":{"status":"ok"}}}`},
			wantStatus:  metav1.ConditionFalse,
			wantReason:  ReasonDown,
			wantNamed:   []string{"database", "nok"},
			wantUnnamed: []string{"cache", "db.internal", "Database down"},
		},
		{
			name:       "v1 unknown is a report the connector made",
			answer:     answer{http.StatusOK, `{"status":"unknown","description":null,"parts":null}`},
			wantStatus: metav1.ConditionFalse,
			wantReason: ReasonUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			connector := &fakeConnector{answers: map[string]answer{v2Path: tt.answer}}

			got := check(t, connector, v2Path)

			assert.Equal(t, tt.wantStatus, got.Status)
			assert.Equal(t, tt.wantReason, got.Reason)
			for _, s := range tt.wantNamed {
				assert.Contains(t, got.Message, s)
			}
			for _, s := range tt.wantUnnamed {
				assert.NotContains(t, got.Message, s)
			}
			last := -1
			for _, s := range tt.wantInOrder {
				i := strings.Index(got.Message, s)
				assert.Greater(t, i, last, "%q out of order in %q", s, got.Message)
				last = i
			}
			assert.Equal(t, []string{"GET " + v2Path}, connector.askedPaths())
		})
	}
}

func TestCheckWithoutAHealthReport(t *testing.T) {
	tests := []struct {
		name     string
		answer   answer
		wantCode string
	}{
		{"a 500 from a connector without the endpoint", answer{http.StatusInternalServerError, `{"message":"Internal server error."}`}, "500"},
		{"a 404", answer{http.StatusNotFound, `{"type":"about:blank","status":404}`}, "404"},
		{"a 503 carrying a problem body", answer{http.StatusServiceUnavailable, `{"type":"about:blank","status":503}`}, "503"},
		{"a body that is not JSON", answer{http.StatusOK, `<html>ok</html>`}, "200"},
		{"a status outside the contract", answer{http.StatusOK, `{"status":"GREEN"}`}, "200"},
		{"an empty object", answer{http.StatusOK, `{}`}, "200"},
		{"an oversized body", answer{http.StatusOK, `{"status":"UP","padding":"` + strings.Repeat("x", 2<<20) + `"}`}, "200"},
		{"a report followed by bytes past the size cap", answer{http.StatusOK, `{"status":"UP"}` + strings.Repeat(" ", 2<<20)}, "200"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := check(t, &fakeConnector{answers: map[string]answer{v2Path: tt.answer}}, v2Path)

			assert.Equal(t, metav1.ConditionUnknown, got.Status)
			assert.Equal(t, ReasonNoHealthReport, got.Reason)
			assert.Contains(t, got.Message, tt.wantCode)
			assert.NotContains(t, got.Message, "Internal server error")
		})
	}
}

func TestCheckTakesARedirectAsTheAnswer(t *testing.T) {
	elsewhere := &fakeConnector{answers: map[string]answer{v2Path: {http.StatusOK, `{"status":"UP"}`}}}
	target := httptest.NewServer(elsewhere)
	t.Cleanup(target.Close)
	redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+v2Path, http.StatusFound)
	})

	got := check(t, redirect, v2Path)

	assert.Equal(t, ReasonNoHealthReport, got.Reason)
	assert.Contains(t, got.Message, "302")
	assert.Empty(t, elsewhere.askedPaths())
}

func TestCheckFallsBackThroughThePaths(t *testing.T) {
	tests := []struct {
		name       string
		answers    map[string]answer
		wantReason string
		wantAsked  []string
		wantNamed  []string
	}{
		{
			name: "a v1 report after v2 gave none",
			answers: map[string]answer{
				v2Path: {http.StatusInternalServerError, `{"message":"Internal server error."}`},
				v1Path: {http.StatusOK, `{"status":"ok","parts":{"database":{"status":"ok"}}}`},
			},
			wantReason: ReasonUp,
			wantAsked:  []string{"GET " + v2Path, "GET " + v1Path},
		},
		{
			name: "a v2 report ends the check",
			answers: map[string]answer{
				v2Path: {http.StatusServiceUnavailable, `{"status":"DOWN","components":{"keystore":{"status":"DOWN"}}}`},
				v1Path: {http.StatusOK, `{"status":"ok"}`},
			},
			wantReason: ReasonDown,
			wantAsked:  []string{"GET " + v2Path},
		},
		{
			name:       "no report on either path names both answers",
			answers:    map[string]answer{v2Path: {http.StatusInternalServerError, `{}`}},
			wantReason: ReasonNoHealthReport,
			wantAsked:  []string{"GET " + v2Path, "GET " + v1Path},
			wantNamed:  []string{v2Path, "500", v1Path, "404"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			connector := &fakeConnector{answers: tt.answers}

			got := check(t, connector, v2Path, v1Path)

			assert.Equal(t, tt.wantReason, got.Reason)
			assert.Equal(t, tt.wantAsked, connector.askedPaths())
			for _, s := range tt.wantNamed {
				assert.Contains(t, got.Message, s)
			}
		})
	}
}

func TestCheckWithoutAnAnswer(t *testing.T) {
	t.Run("the deadline passes first", func(t *testing.T) {
		connector := &fakeConnector{answers: map[string]answer{v2Path: {http.StatusOK, `{"status":"UP"}`}}, delay: 2 * time.Second}
		server := httptest.NewServer(connector)
		t.Cleanup(server.Close)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		got := Check(ctx, NewHTTPClient(), server.URL, []string{v2Path, v1Path})

		assert.Equal(t, metav1.ConditionUnknown, got.Status)
		assert.Equal(t, ReasonNoAnswer, got.Reason)
		assert.Equal(t, []string{"GET " + v2Path}, connector.askedPaths(), "no answer ends the check")
	})

	t.Run("the connection is refused", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		endpoint := server.URL
		server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		got := Check(ctx, NewHTTPClient(), endpoint, []string{v2Path})

		assert.Equal(t, metav1.ConditionUnknown, got.Status)
		assert.Equal(t, ReasonNoAnswer, got.Reason)
		assert.NotContains(t, got.Message, strings.TrimPrefix(endpoint, "http://"), "the connector's address stays out of the condition")
	})

	t.Run("the deadline passes while the body is read", func(t *testing.T) {
		stall := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":`))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		})
		server := httptest.NewServer(stall)
		t.Cleanup(server.Close)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		got := Check(ctx, NewHTTPClient(), server.URL, []string{v2Path})

		assert.Equal(t, ReasonNoAnswer, got.Reason)
	})

	t.Run("the connection breaks off mid-answer", func(t *testing.T) {
		breakOff := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"status\":")
			_ = buf.Flush()
			_ = conn.Close()
		})

		got := check(t, breakOff, v2Path)

		assert.Equal(t, metav1.ConditionUnknown, got.Status)
		assert.Equal(t, ReasonNoAnswer, got.Reason)
		assert.Equal(t, v2Path+" broke off its answer", got.Message)
	})

	t.Run("the answers before the silent path stay in the message", func(t *testing.T) {
		v2FailsThenV1Stalls := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == v2Path {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			<-r.Context().Done()
		})
		server := httptest.NewServer(v2FailsThenV1Stalls)
		t.Cleanup(server.Close)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		got := Check(ctx, NewHTTPClient(), server.URL, []string{v2Path, v1Path})

		assert.Equal(t, ReasonNoAnswer, got.Reason)
		assert.Contains(t, got.Message, v2Path+" answered HTTP 500")
		assert.Contains(t, got.Message, v1Path)
	})
}

func TestCheckKeepsTheMessageWithinTheConditionLimit(t *testing.T) {
	var components []string
	for i := range 2000 {
		components = append(components, fmt.Sprintf(`"profile-with-a-long-name-%04d":{"status":"DOWN"}`, i))
	}
	body := `{"status":"DOWN","components":{` + strings.Join(components, ",") + `}}`

	got := check(t, &fakeConnector{answers: map[string]answer{v2Path: {http.StatusServiceUnavailable, body}}}, v2Path)

	require.Equal(t, ReasonDown, got.Reason)
	assert.LessOrEqual(t, len(got.Message), conditionMessageLimit)
	assert.Contains(t, got.Message, "profile-with-a-long-name-0000")
}

func TestCheckListsAComponentWhoseNameIsTooLong(t *testing.T) {
	name := strings.Repeat("n", 2*conditionMessageLimit)
	body := `{"status":"DOWN","components":{"` + name + `":{"status":"DOWN"}}}`

	got := check(t, &fakeConnector{answers: map[string]answer{v2Path: {http.StatusServiceUnavailable, body}}}, v2Path)

	require.Equal(t, ReasonDown, got.Reason)
	assert.LessOrEqual(t, len(got.Message), conditionMessageLimit)
	assert.Contains(t, got.Message, name[:64])
}

func TestClipCutsWithinTheLimitAtARuneBoundary(t *testing.T) {
	got := clip(strings.Repeat("é", 100), maxComponentEntryBytes)

	assert.LessOrEqual(t, len(got), maxComponentEntryBytes)
	assert.True(t, utf8.ValidString(got), "%q is cut inside a rune", got)
	assert.True(t, strings.HasSuffix(got, "…"))
}
