/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

// Package connectorhealth turns a connector's health endpoint into a Connector's Healthy
// condition. Under the Health Interface, a 200 or a 503 carries a v2 or v1 health report.
package connectorhealth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These reasons name a health report's status. Up comes with ConditionTrue and the rest with
// ConditionFalse.
const (
	ReasonUp           = "Up"
	ReasonDegraded     = "Degraded"
	ReasonDown         = "Down"
	ReasonOutOfService = "OutOfService"
	ReasonUnknown      = "Unknown"
)

// These reasons mark a check without a health report. Both come with ConditionUnknown.
const (
	ReasonNoAnswer       = "NoAnswer"
	ReasonNoHealthReport = "NoHealthReport"
)

// Verdict is one check's Healthy condition.
type Verdict struct {
	Status  metav1.ConditionStatus
	Reason  string
	Message string
}

// maxReportBytes caps a health report's body. A larger body yields no report.
const maxReportBytes = 1 << 20

// These limits bound a message's component list. They keep the message far below the
// condition's limit.
const (
	maxListedComponents    = 10
	maxComponentEntryBytes = 128
)

type statusMapping struct {
	conditionStatus metav1.ConditionStatus
	reason          string
}

// statusMappings maps the v2 and v1 statuses. The mapping follows the platform's.
var statusMappings = map[string]statusMapping{
	"UP":             {metav1.ConditionTrue, ReasonUp},
	"DEGRADED":       {metav1.ConditionFalse, ReasonDegraded},
	"DOWN":           {metav1.ConditionFalse, ReasonDown},
	"OUT_OF_SERVICE": {metav1.ConditionFalse, ReasonOutOfService},
	"UNKNOWN":        {metav1.ConditionFalse, ReasonUnknown},
	"ok":             {metav1.ConditionTrue, ReasonUp},
	"nok":            {metav1.ConditionFalse, ReasonDown},
	"unknown":        {metav1.ConditionFalse, ReasonUnknown},
}

func isUp(status string) bool {
	mapping, known := statusMappings[status]
	return known && mapping.conditionStatus == metav1.ConditionTrue
}

// healthReport is the verdict's view of a v2 HealthInfo or a v1 HealthDto.
type healthReport struct {
	Status     string                     `json:"status"`
	Components map[string]componentReport `json:"components"`
	Parts      map[string]componentReport `json:"parts"`
}

// componentReport is one entry of a report's v2 components or v1 parts.
type componentReport struct {
	Status string `json:"status"`
}

// NewHTTPClient returns a client that bypasses the proxy, takes a redirect as the answer and
// dials anew per check. The check thus stays on the cluster and reaches any pod the Service picks.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:             nil,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Check asks endpoint at each path in turn, stopping at a health report or a missing answer.
// The caller's context bounds the whole check.
func Check(ctx context.Context, httpClient *http.Client, endpoint string, paths []string) Verdict {
	answers := make([]string, 0, len(paths))
	for _, path := range paths {
		verdict, code, err := ask(ctx, httpClient, endpoint+path)
		if err != nil {
			answers = append(answers, unanswered(path, err))
			return Verdict{Status: metav1.ConditionUnknown, Reason: ReasonNoAnswer, Message: strings.Join(answers, ", ")}
		}
		if verdict != nil {
			return *verdict
		}
		answers = append(answers, fmt.Sprintf("%s answered HTTP %d", path, code))
	}
	return Verdict{
		Status:  metav1.ConditionUnknown,
		Reason:  ReasonNoHealthReport,
		Message: "no health report: " + strings.Join(answers, ", "),
	}
}

// ask GETs one health endpoint and returns the HTTP code, plus the verdict of a health report.
// A non-nil err means no complete answer arrived.
func ask(ctx context.Context, httpClient *http.Client, url string) (*Verdict, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return nil, resp.StatusCode, nil
	}
	body, fits, err := readCapped(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", errBrokenOff, err)
	}
	if !fits {
		return nil, resp.StatusCode, nil
	}
	verdict, ok := verdictOf(body)
	if !ok {
		return nil, resp.StatusCode, nil
	}
	return &verdict, resp.StatusCode, nil
}

// errBrokenOff marks an answer with a truncated body.
var errBrokenOff = errors.New("answer broke off")

// readCapped reads at most one byte past maxReportBytes. That byte tells whether body fits.
func readCapped(body io.Reader) (data []byte, fits bool, err error) {
	data, err = io.ReadAll(io.LimitReader(body, maxReportBytes+1))
	return data, len(data) <= maxReportBytes, err
}

func verdictOf(body []byte) (Verdict, bool) {
	var report healthReport
	if err := json.Unmarshal(body, &report); err != nil {
		return Verdict{}, false
	}
	return report.verdict()
}

// unanswered names the path and the kind of failure only. The transport error quotes the
// connector's address.
func unanswered(path string, err error) string {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return path + " gave no answer within the timeout"
	}
	if errors.Is(err, errBrokenOff) {
		return path + " broke off its answer"
	}
	return path + " could not be reached"
}

// verdict maps the reported status, with ok false for a status outside the interface.
// Component details can name hosts and stay out of the message.
func (r *healthReport) verdict() (verdict Verdict, ok bool) {
	mapping, known := statusMappings[r.Status]
	if !known {
		return Verdict{}, false
	}
	message := "connector reports " + r.Status
	if unhealthy := r.unhealthyComponents(); len(unhealthy) > 0 {
		message += "; unhealthy components: " + listComponents(unhealthy)
	}
	return Verdict{Status: mapping.conditionStatus, Reason: mapping.reason, Message: message}, true
}

// unhealthyComponents lists each component outside Up as a clipped "name (status)", sorted.
func (r *healthReport) unhealthyComponents() []string {
	var unhealthy []string
	for _, components := range []map[string]componentReport{r.Components, r.Parts} {
		for name, component := range components {
			if !isUp(component.Status) {
				unhealthy = append(unhealthy, clip(fmt.Sprintf("%s (%s)", name, component.Status), maxComponentEntryBytes))
			}
		}
	}
	slices.Sort(unhealthy)
	return unhealthy
}

func listComponents(entries []string) string {
	if len(entries) <= maxListedComponents {
		return strings.Join(entries, ", ")
	}
	listed := strings.Join(entries[:maxListedComponents], ", ")
	return fmt.Sprintf("%s, and %d more", listed, len(entries)-maxListedComponents)
}

const ellipsis = "…"

// clip cuts s to at most n bytes, ellipsis included, at a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}
