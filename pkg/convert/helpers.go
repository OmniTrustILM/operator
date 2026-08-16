/*
Copyright (c) ILM.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package convert

import (
	"math"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// mapOf returns v as a map[string]interface{} when it is one, otherwise an empty map. It
// lets the converter walk the loosely-typed values tree without nil-panic guards at every
// step.
func mapOf(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

// slice returns v as a []interface{} when it is one, otherwise nil.
func slice(v interface{}) []interface{} {
	if s, ok := v.([]interface{}); ok {
		return s
	}
	return nil
}

// str returns v as a string when it is one, otherwise "". Non-string scalars (numbers,
// bools) are intentionally NOT coerced here — callers that want a number use intOf/boolOf.
func str(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// intOf coerces v to int32 from the numeric types a YAML decoder produces (int, int64,
// float64). It reports ok=false for non-numeric OR out-of-int32-range values, so callers
// leave a CR int32 field at its default rather than wrapping a too-large number (the CR
// fields this feeds — ports, replicas, instances — are all small int32s).
func intOf(v interface{}) (int32, bool) {
	var i64 int64
	switch n := v.(type) {
	case int:
		i64 = int64(n)
	case int32:
		return n, true
	case int64:
		i64 = n
	case float64:
		if n < math.MinInt32 || n > math.MaxInt32 {
			return 0, false
		}
		i64 = int64(n)
	default:
		return 0, false
	}
	if i64 < math.MinInt32 || i64 > math.MaxInt32 {
		return 0, false
	}
	return int32(i64), true
}

// boolOf coerces v to bool, reporting ok=false for non-bool values.
func boolOf(v interface{}) (bool, bool) {
	if b, ok := v.(bool); ok {
		return b, true
	}
	return false, false
}

// stringSlice returns v as a []string, coercing each element via str and dropping empties.
func stringSlice(v interface{}) []string {
	var out []string
	for _, e := range slice(v) {
		if s := str(e); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// stringMap returns v as a map[string]string, coercing each value via str.
func stringMap(v interface{}) map[string]string {
	in := mapOf(v)
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, val := range in {
		out[k] = str(val)
	}
	return out
}

// isEmpty reports whether v is nil, an empty map, or an empty slice — used to skip blocks
// that are present but carry no content (so they are neither mapped nor flagged).
func isEmpty(v interface{}) bool {
	switch t := v.(type) {
	case nil:
		return true
	case map[string]interface{}:
		return len(t) == 0
	case []interface{}:
		return len(t) == 0
	case string:
		return t == ""
	default:
		return false
	}
}

// ptrTo returns a pointer to v, for setting optional pointer CR fields.
func ptrTo[T any](v T) *T { return &v }

// roundTrip re-encodes a loosely-typed values fragment through YAML into a typed target.
// It is used for blocks (resources) the CR carries as a typed Kubernetes struct, so the
// chart's verbatim shape is preserved without hand-mapping every nested field.
func roundTrip(in interface{}, out interface{}) error {
	data, err := yaml.Marshal(in)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, out)
}

// resourceRequirements is a minimal mirror of corev1.ResourceRequirements with string-
// valued quantities, so a chart resources block (cpu/memory as strings) round-trips
// cleanly before being converted to the quantity-typed corev1 form.
type resourceRequirements struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

// toCore converts the string-valued resource map into a corev1.ResourceRequirements,
// parsing each value as a Kubernetes quantity. Unparseable values are skipped (best-effort).
func (rr resourceRequirements) toCore() *corev1.ResourceRequirements {
	out := &corev1.ResourceRequirements{}
	if list := toResourceList(rr.Requests); len(list) > 0 {
		out.Requests = list
	}
	if list := toResourceList(rr.Limits); len(list) > 0 {
		out.Limits = list
	}
	return out
}

// toResourceList parses a name->quantity-string map into a corev1.ResourceList, skipping
// values that are not valid Kubernetes quantities.
func toResourceList(m map[string]string) corev1.ResourceList {
	if len(m) == 0 {
		return nil
	}
	out := corev1.ResourceList{}
	for name, val := range m {
		q, err := resource.ParseQuantity(val)
		if err != nil {
			continue
		}
		out[corev1.ResourceName(name)] = q
	}
	return out
}

// resourcesFrom converts a chart "resources" sub-block (requests/limits) into typed
// ResourceRequirements via a YAML round-trip, or nil when it is absent, empty or unparseable.
// It is the ONE conversion both component resources and the time-quality-monitor sidecar's
// resources go through, so the two can never diverge.
func resourcesFrom(block vals) *corev1.ResourceRequirements {
	res := mapOf(block["resources"])
	if len(res) == 0 {
		return nil
	}
	var rr resourceRequirements
	if roundTrip(res, &rr) != nil || (len(rr.Requests) == 0 && len(rr.Limits) == 0) {
		return nil
	}
	return rr.toCore()
}

// componentSpecPath maps an umbrella values key to the Platform spec path segment for that
// component, used in customization hints. For the top-level "image" block (Core's image)
// it returns "core".
func componentSpecPath(valuesKey string) string {
	if valuesKey == "image" {
		return "core"
	}
	return valuesKey
}

// normalizeGlobalKey maps a chart global passthrough key to the spec.common field name it
// corresponds to (e.g. sidecarContainers -> sidecars, additionalVolumes -> volumes).
func normalizeGlobalKey(chartKey string) string {
	switch chartKey {
	case "sidecarContainers":
		return "sidecars"
	case "additionalVolumes":
		return "volumes"
	case "additionalVolumeMounts":
		return "volumeMounts"
	default:
		return chartKey
	}
}
