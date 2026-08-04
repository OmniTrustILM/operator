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

package platform

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	"github.com/OmniTrustILM/operator/pkg/bom"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// managedMQPlatform returns a Platform with a managed broker (replicas/version/storage)
// and any extra mutation applied, in a namespace so the rendered objects carry it.
func managedMQPlatform(mutate func(*otilmv1alpha1.Platform)) *otilmv1alpha1.Platform {
	scClass := "fast-ssd"
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{
			Messaging: otilmv1alpha1.MessagingSpec{
				Mode:       "managed",
				BrokerType: "rabbitmq",
				Managed: &otilmv1alpha1.ManagedMessagingSpec{
					Replicas: 3,
					Version:  "4.0",
					Storage:  otilmv1alpha1.StorageSpec{Size: "20Gi", StorageClass: &scClass},
				},
			},
		},
	}
	if mutate != nil {
		mutate(p)
	}
	return p
}

// findManagedObjs returns all rendered objects of the given kind.
func findManagedObjs(objs []client.Object, kind string) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	for _, o := range objs {
		u, ok := o.(*unstructured.Unstructured)
		if ok && u.GetKind() == kind {
			out = append(out, u)
		}
	}
	return out
}

// renderedNames returns the sorted metadata.names of a rendered object set — the object
// IDENTITIES the operator applies to the cluster.
func renderedNames(t *testing.T, objs []client.Object) []string {
	t.Helper()
	names := make([]string, 0, len(objs))
	for _, o := range objs {
		names = append(names, o.GetName())
	}
	sort.Strings(names)
	return names
}

// TestLegacyTopologyNamesAreFrozen pins the topology CR names for the legacy vhost
// EXACTLY as earlier operator builds applied them. Live 2.17.0/2.18.0 platforms carry
// these names in their clusters; rendering anything else would orphan every applied CR
// (rabbitmq.com kinds are deliberately excluded from pruning, so the originals would
// linger forever holding finalizers over live queues). Never relax this test: the legacy
// vhost must map to an EMPTY scope, unconditionally.
func TestLegacyTopologyNamesAreFrozen(t *testing.T) {
	t.Run("default bundle (2.18.0)", func(t *testing.T) {
		p := managedMQPlatform(nil) // vhost unset -> the 2.18.0 bundle default
		assert.Equal(t, frozenUnscopedTopologyNames, renderedNames(t, ResolveManagedMessaging(p)))
	})

	t.Run("2.17.0 bundle", func(t *testing.T) {
		p := managedMQPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Version = testVersion217 })
		assert.Equal(t, []string{
			"ilm-messaging",
			"ilm-messaging-binding-czertainly-core-actions",
			"ilm-messaging-binding-czertainly-core-audit-logs",
			"ilm-messaging-binding-czertainly-core-events",
			"ilm-messaging-binding-czertainly-core-notifications",
			"ilm-messaging-binding-czertainly-core-scheduler",
			"ilm-messaging-binding-czertainly-core-validation",
			testMsgUserCore,
			testMsgCorePerm,
			"ilm-messaging-exchange-czertainly",
			"ilm-messaging-queue-core",
			"ilm-messaging-queue-core-actions",
			testMsgQueueAudit,
			"ilm-messaging-queue-core-events",
			"ilm-messaging-queue-core-notifications",
			"ilm-messaging-queue-core-scheduler",
			"ilm-messaging-queue-core-validation",
			testMsgVhost,
		}, renderedNames(t, ResolveManagedMessaging(p)))
	})
}

// TestUserPinnedVhostTopologyNamesAreFrozen proves a user-pinned spec.messaging.virtualHost
// renders the SAME unscoped topology CR names as an operator predating vhost-scoped naming
// (commit 7ef6d9e) — exactly the legacy-vhost freeze above, one vhost over. A pinned vhost
// resolves to the same value under every bundle (a user override always wins over the bundle
// default), so that platform never experiences the vhost RENAME a version migration causes:
// it never migrates, and so it never needs a disjoint name. Never relax this test: a
// user-pinned vhost must map to an EMPTY scope, unconditionally.
func TestUserPinnedVhostTopologyNamesAreFrozen(t *testing.T) {
	t.Run("custom vhost on the 2.18.0 bundle", func(t *testing.T) {
		p := managedMQPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Messaging.VirtualHost = "myvhost" })
		assert.Equal(t, frozenUnscopedTopologyNames, renderedNames(t, ResolveManagedMessaging(p)))
	})

	t.Run(`vhost pinned to "/" on the 2.18.0 bundle`, func(t *testing.T) {
		p := managedMQPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Messaging.VirtualHost = "/" })
		assert.Equal(t, frozenUnscopedTopologyNames, renderedNames(t, ResolveManagedMessaging(p)))
	})
}

// frozenUnscopedTopologyNames is the EXACT set of topology CR names an operator predating
// vhost-scoped naming applied on the 2.18.0 bundle, spelled out in full so a rename is a diff
// rather than a derivation. Both freezes above assert against this one list because they are
// the same freeze reached two ways — the legacy vhost, and a user-pinned one — and a list that
// existed twice could be relaxed on one side alone.
var frozenUnscopedTopologyNames = []string{
	"ilm-messaging",
	"ilm-messaging-administrator",
	"ilm-messaging-administrator-permission",
	"ilm-messaging-binding-czertainly-core-actions",
	"ilm-messaging-binding-czertainly-core-audit-logs",
	"ilm-messaging-binding-czertainly-core-events",
	"ilm-messaging-binding-czertainly-core-notifications",
	"ilm-messaging-binding-czertainly-core-scheduler",
	"ilm-messaging-binding-czertainly-core-validation",
	"ilm-messaging-binding-czertainly-time-quality-config",
	"ilm-messaging-binding-czertainly-time-quality-config-request",
	"ilm-messaging-binding-czertainly-time-quality-results",
	testMsgUserCore,
	testMsgCorePerm,
	"ilm-messaging-exchange-czertainly",
	"ilm-messaging-exchange-czertainly-proxy",
	testMsgUserMonitor,
	"ilm-messaging-monitor-permission",
	testMsgUserProv,
	"ilm-messaging-provisioner-permission",
	testMsgUserProxy,
	"ilm-messaging-proxy-permission",
	"ilm-messaging-queue-core",
	"ilm-messaging-queue-core-actions",
	testMsgQueueAudit,
	"ilm-messaging-queue-core-events",
	"ilm-messaging-queue-core-notifications",
	"ilm-messaging-queue-core-scheduler",
	"ilm-messaging-queue-core-validation",
	"ilm-messaging-queue-time-quality-config",
	"ilm-messaging-queue-time-quality-config-request",
	"ilm-messaging-queue-time-quality-results",
	testMsgVhost,
}

// namesOfKinds returns the sorted metadata.names of the rendered objects whose Kind is one
// of kinds.
func namesOfKinds(objs []client.Object, kinds ...string) []string {
	want := map[string]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	var names []string
	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok && want[u.GetKind()] {
			names = append(names, u.GetName())
		}
	}
	sort.Strings(names)
	return names
}

// vhostScopedNames returns the names of the rendered objects whose identity is vhost-scoped
// (everything the Messaging Topology Operator binds to a single vhost). The RabbitmqCluster
// and the Users are broker-global and deliberately excluded.
func vhostScopedNames(objs []client.Object) []string {
	return namesOfKinds(objs, rmqKindVhost, rmqKindPermission, rmqKindExchange, rmqKindQueue, rmqKindBinding)
}

// TestSourceAndTargetTopologyNamesAreDisjoint proves a 2.18.0 render and a 2.19.0 render
// share no vhost-scoped topology CR name, so both topologies can be live at once during a
// migration (the Topology Operator's immutable spec.vhost/spec.name would otherwise reject
// re-pointing the applied CRs).
//
// The RabbitmqCluster and the User CRs are EXCLUDED on purpose: they are broker-global,
// their specs are identical across the two bundles, and each User's generated
// "<user>-user-credentials" Secret is wired into every component's secretKeyRef — so they
// must stay IDENTICAL across the cutover, which is asserted here instead of disjointness.
func TestSourceAndTargetTopologyNamesAreDisjoint(t *testing.T) {
	src := managedMQPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Version = testVersion218 })
	tgt := managedMQPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Version = testVersion219 })
	srcObjs := ResolveManagedMessaging(src)
	tgtObjs := ResolveManagedMessaging(tgt)

	s := vhostScopedNames(srcObjs)
	d := vhostScopedNames(tgtObjs)
	require.NotEmpty(t, s)
	require.NotEmpty(t, d)
	for _, n := range s {
		assert.NotContains(t, d, n, "source name %q must not collide with the target set", n)
	}

	assert.Equal(t, namesOfKinds(srcObjs, rmqKindUser), namesOfKinds(tgtObjs, rmqKindUser),
		"the broker Users (and thus their credentials Secrets) must be IDENTICAL across the cutover")
	assert.Equal(t, namesOfKinds(srcObjs, rmqKindCluster), namesOfKinds(tgtObjs, rmqKindCluster),
		"both topologies live on the SAME managed broker")
}

// TestManagedUserNamesAreVhostIndependent proves the broker Users — and the credentials
// Secrets the Topology Operator generates from them, which every component consumes by
// secretKeyRef — are NOT vhost-scoped. Scoping them would re-key every component's
// credentials at cutover: a second migration inside the first.
func TestManagedUserNamesAreVhostIndependent(t *testing.T) {
	for _, vh := range []string{"", "/", "myvhost", bom.LegacyUnscopedVirtualHost} {
		p := managedMQPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Messaging.VirtualHost = vh })
		assert.Equal(t, testMsgUserCore, managedUserName(p, bom.MessagingUserCore),
			"the User CR name must not depend on the vhost (%q)", vh)
		assert.Equal(t, "ilm-messaging-core-user-credentials", managedUserCredentialsSecretName(p, bom.MessagingUserCore),
			"the generated credentials Secret name must not depend on the vhost (%q)", vh)
		assert.Contains(t, namesOfKinds(ResolveManagedMessaging(p), rmqKindUser), testMsgUserCore,
			"the rendered User CR must keep its vhost-independent name (%q)", vh)
	}
}

func TestResolveManagedMessagingExternalRendersNothing(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{
			Messaging: otilmv1alpha1.MessagingSpec{
				Mode: "external", Host: "mq", Port: 5672, VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "mq-secret"},
			},
		},
	}
	assert.Nil(t, ResolveManagedMessaging(p), "external mode renders no RabbitMQ objects")
	assert.False(t, MessagingManaged(p))
}

// TestResolveManagedMessagingObjectCounts asserts the full topology cardinality the
// operator renders: 1 cluster + 1 vhost + 5 users + 5 permissions + 2 exchanges + 10
// queues + 9 bindings = 33 objects.
func TestResolveManagedMessagingObjectCounts(t *testing.T) {
	p := managedMQPlatform(nil)
	objs := ResolveManagedMessaging(p)

	assert.Len(t, findManagedObjs(objs, rmqKindCluster), 1, "exactly one RabbitmqCluster")
	assert.Len(t, findManagedObjs(objs, rmqKindVhost), 1, "exactly one Vhost")
	assert.Len(t, findManagedObjs(objs, rmqKindUser), 5, "five users (administrator/provisioner/proxy/core/monitor)")
	assert.Len(t, findManagedObjs(objs, rmqKindPermission), 5, "five permission sets")
	assert.Len(t, findManagedObjs(objs, rmqKindExchange), 2, "two exchanges (czertainly + czertainly-proxy)")
	assert.Len(t, findManagedObjs(objs, rmqKindQueue), 10, "ten queues (core.* + time-quality.*)")
	assert.Len(t, findManagedObjs(objs, rmqKindBinding), 9, "nine bindings (core.* + time-quality.*)")
	assert.Len(t, objs, 33, "the full topology renders 33 objects")
}

func TestResolveManagedMessagingRendersCluster(t *testing.T) {
	p := managedMQPlatform(nil)
	cluster := findManagedObj(ResolveManagedMessaging(p), rmqKindCluster)
	require.NotNil(t, cluster, "the RabbitmqCluster must be rendered")

	// GVK + identity.
	assert.Equal(t, rabbitmqAPIVersion, cluster.GetAPIVersion())
	assert.Equal(t, rmqKindCluster, cluster.GetKind())
	assert.Equal(t, testMessagingName, cluster.GetName(), "Cluster name is <platform>-messaging")
	assert.Equal(t, "ns", cluster.GetNamespace())

	// Recommended labels (managed-by/instance drive the prune-exclusion + Secret watch).
	labels := cluster.GetLabels()
	assert.Equal(t, common.ManagedByValue, labels[common.ManagedByLabel])
	assert.Equal(t, "ilm", labels[common.InstanceLabel])
	assert.Equal(t, managedMessagingRole, labels[common.ComponentLabel])

	// replicas.
	replicas, found, err := unstructured.NestedInt64(cluster.Object, "spec", "replicas")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, int64(3), replicas)

	// persistence.{storage,storageClassName}.
	size, _, _ := unstructured.NestedString(cluster.Object, "spec", "persistence", "storage")
	assert.Equal(t, "20Gi", size)
	sc, _, _ := unstructured.NestedString(cluster.Object, "spec", "persistence", "storageClassName")
	assert.Equal(t, "fast-ssd", sc)

	// version → image.
	image, _, _ := unstructured.NestedString(cluster.Object, "spec", "image")
	assert.Contains(t, image, "4.0", "the version selects the RabbitMQ image tag")
}

func TestResolveManagedMessagingResources(t *testing.T) {
	p := managedMQPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Messaging.Managed.Resources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
		}
	})
	cluster := findManagedObj(ResolveManagedMessaging(p), rmqKindCluster)
	require.NotNil(t, cluster)
	reqCPU, _, _ := unstructured.NestedString(cluster.Object, "spec", "resources", "requests", "cpu")
	assert.Equal(t, "1", reqCPU)
	limMem, _, _ := unstructured.NestedString(cluster.Object, "spec", "resources", "limits", "memory")
	assert.Equal(t, "2Gi", limMem)
}

func TestResolveManagedMessagingNoVersionOmitsImage(t *testing.T) {
	p := managedMQPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Messaging.Managed.Version = "" })
	cluster := findManagedObj(ResolveManagedMessaging(p), rmqKindCluster)
	require.NotNil(t, cluster)
	_, found, _ := unstructured.NestedString(cluster.Object, "spec", "image")
	assert.False(t, found, "with no version the Cluster Operator default image is used (image omitted)")
}

// TestResolveManagedMessagingManagementPathPrefix locks the management-UI path-prefix wiring:
// when messaging.management.expose is on, the RabbitmqCluster carries management.path_prefix=/mq
// in spec.rabbitmq.additionalConfig so RabbitMQ serves the management UI under /mq (matching the
// gateway's unstripped /mq route) and the UI loads with OR without the trailing slash. When expose
// is off the broker is left unchanged (no rabbitmq.additionalConfig). The prefix MUST equal the
// gateway route path, so both read the same gatewayMqPath constant.
func TestResolveManagedMessagingManagementPathPrefix(t *testing.T) {
	t.Run("expose on -> path_prefix set", func(t *testing.T) {
		p := managedMQPlatform(func(p *otilmv1alpha1.Platform) {
			p.Spec.Messaging.Management.Expose = true
		})
		cluster := findManagedObj(ResolveManagedMessaging(p), rmqKindCluster)
		require.NotNil(t, cluster)
		cfg, found, err := unstructured.NestedString(cluster.Object, "spec", "rabbitmq", "additionalConfig")
		require.NoError(t, err)
		require.True(t, found, "expose on must set spec.rabbitmq.additionalConfig")
		assert.Equal(t, "management.path_prefix = "+gatewayMqPath+"\n", cfg)
		assert.Contains(t, cfg, "management.path_prefix = /mq", "prefix must match the gateway /mq route")
	})
	t.Run("expose off -> broker unchanged", func(t *testing.T) {
		p := managedMQPlatform(nil) // expose defaults off
		cluster := findManagedObj(ResolveManagedMessaging(p), rmqKindCluster)
		require.NotNil(t, cluster)
		_, found, _ := unstructured.NestedString(cluster.Object, "spec", "rabbitmq", "additionalConfig")
		assert.False(t, found, "expose off must NOT set a management path prefix (broker unchanged)")
	})
}

func TestResolveManagedMessagingReplicasDefault(t *testing.T) {
	p := managedMQPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Messaging.Managed.Replicas = 0 })
	cluster := findManagedObj(ResolveManagedMessaging(p), rmqKindCluster)
	require.NotNil(t, cluster)
	replicas, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "replicas")
	assert.Equal(t, int64(1), replicas)
}

// findByName returns the first unstructured object with the given metadata.name.
func findByName(objs []*unstructured.Unstructured, name string) *unstructured.Unstructured {
	for _, o := range objs {
		if o.GetName() == name {
			return o
		}
	}
	return nil
}

func TestResolveManagedMessagingVhost(t *testing.T) {
	p := managedMQPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Messaging.VirtualHost = "myvhost" })
	vhost := findManagedObj(ResolveManagedMessaging(p), rmqKindVhost)
	require.NotNil(t, vhost)
	name, _, _ := unstructured.NestedString(vhost.Object, "spec", "name")
	assert.Equal(t, "myvhost", name, "the Vhost uses the configured virtualHost")
	ref, _, _ := unstructured.NestedString(vhost.Object, "spec", "rabbitmqClusterReference", "name")
	assert.Equal(t, testMessagingName, ref, "every topology CR references the managed cluster")

	// A user-pinned vhost keeps every vhost-bound object name UNSCOPED: it resolves to the
	// same value on every bundle, so this platform never migrates and never needs a
	// disjoint name (see TestUserPinnedVhostTopologyNamesAreFrozen and topologyScope).
	assert.Equal(t, testMsgVhost, vhost.GetName())
	assert.Contains(t, namesOfKinds(ResolveManagedMessaging(p), rmqKindPermission), testMsgCorePerm)
	assert.Contains(t, namesOfKinds(ResolveManagedMessaging(p), rmqKindQueue), testMsgQueueAudit)
}

func TestResolveManagedMessagingVhostDefault(t *testing.T) {
	// No virtualHost → the default "czertainly".
	p := managedMQPlatform(nil)
	vhost := findManagedObj(ResolveManagedMessaging(p), rmqKindVhost)
	require.NotNil(t, vhost)
	name, _, _ := unstructured.NestedString(vhost.Object, "spec", "name")
	assert.Equal(t, "czertainly", name)
}

// TestResolveManagedMessagingUsers asserts the five users + their tags, and that NO user
// CR carries an inline password (the Topology Operator generates the Secret).
func TestResolveManagedMessagingUsers(t *testing.T) {
	p := managedMQPlatform(nil)
	users := findManagedObjs(ResolveManagedMessaging(p), rmqKindUser)
	require.Len(t, users, 5)

	type want struct {
		name string
		tags []string
	}
	wants := []want{
		{testMessagingAdminUser, []string{"administrator"}},
		{testMsgUserProv, []string{"administrator"}},
		{testMsgUserProxy, nil},
		{testMsgUserCore, nil},
		{testMsgUserMonitor, nil}, // time-quality monitor user (new in 2.18.0)
	}
	for _, w := range wants {
		u := findByName(users, w.name)
		require.NotNil(t, u, "user %s must be rendered", w.name)
		ref, _, _ := unstructured.NestedString(u.Object, "spec", "rabbitmqClusterReference", "name")
		assert.Equal(t, testMessagingName, ref)
		tags, found, _ := unstructured.NestedStringSlice(u.Object, "spec", "tags")
		if len(w.tags) == 0 {
			assert.False(t, found, "user %s has no tags", w.name)
		} else {
			assert.Equal(t, w.tags, tags, "user %s tags", w.name)
		}
		// No inline password anywhere on the User CR.
		body, _ := json.Marshal(u.Object)
		assert.NotContains(t, string(body), "password", "User %s must carry no inline credential", w.name)
	}
}

// TestResolveManagedMessagingPermissions asserts the EXACT permission regexes from the
// chart for each user (the load-bearing detail of the topology).
func TestResolveManagedMessagingPermissions(t *testing.T) {
	p := managedMQPlatform(nil)
	perms := findManagedObjs(ResolveManagedMessaging(p), rmqKindPermission)
	require.Len(t, perms, 5)

	type want struct {
		permName               string
		userRef                string
		configure, write, read string
	}
	wants := []want{
		{testMessagingAdminPerm, testMessagingAdminUser, ".*", ".*", ".*"},
		{"ilm-messaging-provisioner-permission", testMsgUserProv, ".*", ".*", ".*"},
		{"ilm-messaging-proxy-permission", testMsgUserProxy, "", "^czertainly-proxy$", `^proxy\..*$`},
		// Core's read MUST include the time-quality monitor's request/result queues (2.18.0) —
		// without it the broker denies read and Core crash-loops.
		{testMsgCorePerm, testMsgUserCore, "", "^czertainly(-proxy)?$", `^core(\..+|-.+)?$|^time-quality\.(config-request|results)$`},
		{"ilm-messaging-monitor-permission", testMsgUserMonitor, "", "^czertainly$", `^time-quality\.config$`},
	}
	for _, w := range wants {
		perm := findByName(perms, w.permName)
		require.NotNil(t, perm, "permission %s must be rendered", w.permName)
		vhost, _, _ := unstructured.NestedString(perm.Object, "spec", "vhost")
		assert.Equal(t, "czertainly", vhost)
		userRef, _, _ := unstructured.NestedString(perm.Object, "spec", "userReference", "name")
		assert.Equal(t, w.userRef, userRef, "permission %s userReference", w.permName)
		configure, _, _ := unstructured.NestedString(perm.Object, "spec", "permissions", "configure")
		write, _, _ := unstructured.NestedString(perm.Object, "spec", "permissions", "write")
		read, _, _ := unstructured.NestedString(perm.Object, "spec", "permissions", "read")
		assert.Equal(t, w.configure, configure, "permission %s configure regex", w.permName)
		assert.Equal(t, w.write, write, "permission %s write regex", w.permName)
		assert.Equal(t, w.read, read, "permission %s read regex", w.permName)
	}
}

// TestResolveManagedMessagingExchanges asserts the two exchanges + their types/durability.
func TestResolveManagedMessagingExchanges(t *testing.T) {
	p := managedMQPlatform(nil)
	exchanges := findManagedObjs(ResolveManagedMessaging(p), rmqKindExchange)
	require.Len(t, exchanges, 2)

	byExchangeName := func(specName string) *unstructured.Unstructured {
		for _, e := range exchanges {
			n, _, _ := unstructured.NestedString(e.Object, "spec", "name")
			if n == specName {
				return e
			}
		}
		return nil
	}

	direct := byExchangeName("czertainly")
	require.NotNil(t, direct)
	typ, _, _ := unstructured.NestedString(direct.Object, "spec", "type")
	assert.Equal(t, "direct", typ)
	durable, _, _ := unstructured.NestedBool(direct.Object, "spec", "durable")
	assert.True(t, durable)
	vhost, _, _ := unstructured.NestedString(direct.Object, "spec", "vhost")
	assert.Equal(t, "czertainly", vhost)

	topic := byExchangeName("czertainly-proxy")
	require.NotNil(t, topic)
	typ2, _, _ := unstructured.NestedString(topic.Object, "spec", "type")
	assert.Equal(t, "topic", typ2)
}

// TestResolveManagedMessagingQueues asserts all ten queues are present by their exact
// app-level names (carried verbatim in spec.name), including the three time-quality monitor
// queues added in 2.18.0 and their x-max-length/x-overflow arguments.
func TestResolveManagedMessagingQueues(t *testing.T) {
	p := managedMQPlatform(nil)
	queues := findManagedObjs(ResolveManagedMessaging(p), rmqKindQueue)
	require.Len(t, queues, 10)

	byQueueName := map[string]*unstructured.Unstructured{}
	got := map[string]bool{}
	for _, q := range queues {
		n, _, _ := unstructured.NestedString(q.Object, "spec", "name")
		got[n] = true
		byQueueName[n] = q
		durable, _, _ := unstructured.NestedBool(q.Object, "spec", "durable")
		assert.True(t, durable, "queue %s is durable", n)
		vhost, _, _ := unstructured.NestedString(q.Object, "spec", "vhost")
		assert.Equal(t, "czertainly", vhost)
	}
	for _, name := range []string{
		"core", testQueueAuditLogs, "core.notifications", "core.scheduler", "core.actions",
		"core.validation", "core.events",
		testQueueTQConfig, testQueueTQConfigReq, testQueueTQResults,
	} {
		assert.True(t, got[name], "queue %q must be present", name)
	}
	// The two time-quality config queues keep only the latest message (x-max-length 1, drop-head);
	// time-quality.results is a plain queue with no arguments.
	for _, name := range []string{testQueueTQConfig, testQueueTQConfigReq} {
		args, found, _ := unstructured.NestedMap(byQueueName[name].Object, "spec", "arguments")
		require.True(t, found, "queue %q carries x-arguments", name)
		assert.Equal(t, "drop-head", args["x-overflow"], "queue %q x-overflow", name)
		assert.Contains(t, args, "x-max-length", "queue %q has x-max-length", name)
	}
	_, hasArgs, _ := unstructured.NestedMap(byQueueName[testQueueTQResults].Object, "spec", "arguments")
	assert.False(t, hasArgs, "time-quality.results is a plain queue (no arguments)")
}

// TestResolveManagedMessagingBindings asserts the nine czertainly-exchange→queue bindings
// with their exact routing keys, and destinationType=queue (incl. the three time-quality
// bindings added in 2.18.0, whose routing key equals the queue name).
func TestResolveManagedMessagingBindings(t *testing.T) {
	p := managedMQPlatform(nil)
	bindings := findManagedObjs(ResolveManagedMessaging(p), rmqKindBinding)
	require.Len(t, bindings, 9)

	// destination → routingKey map the operator wires for the platform queues.
	want := map[string]string{
		testQueueAuditLogs:   "audit-logs",
		"core.notifications": "notification",
		"core.actions":       "action",
		"core.scheduler":     "scheduler",
		"core.validation":    "validation",
		"core.events":        "event",
		testQueueTQConfig:    testQueueTQConfig,
		testQueueTQConfigReq: testQueueTQConfigReq,
		testQueueTQResults:   testQueueTQResults,
	}
	got := map[string]string{}
	for _, b := range bindings {
		src, _, _ := unstructured.NestedString(b.Object, "spec", "source")
		assert.Equal(t, "czertainly", src, "every binding sources the czertainly direct exchange")
		dt, _, _ := unstructured.NestedString(b.Object, "spec", "destinationType")
		assert.Equal(t, "queue", dt)
		dest, _, _ := unstructured.NestedString(b.Object, "spec", "destination")
		rk, _, _ := unstructured.NestedString(b.Object, "spec", "routingKey")
		got[dest] = rk
	}
	assert.Equal(t, want, got, "the bindings must match the destination→routingKey map exactly")
}

func TestResolveManagedMessagingTopologyNames(t *testing.T) {
	// metadata.name is sanitized (".", "_" → "-") while spec.name carries the original.
	p := managedMQPlatform(nil)
	queues := findManagedObjs(ResolveManagedMessaging(p), rmqKindQueue)
	auditLogs := func() *unstructured.Unstructured {
		for _, q := range queues {
			if n, _, _ := unstructured.NestedString(q.Object, "spec", "name"); n == testQueueAuditLogs {
				return q
			}
		}
		return nil
	}()
	require.NotNil(t, auditLogs)
	assert.Equal(t, testMsgQueueAudit, auditLogs.GetName(),
		"metadata.name is DNS-sanitized while spec.name keeps the dotted app name")
}

// --- Overrides (RFC 7396 JSON-merge patch) -----------------------------------

func TestManagedMessagingOverridesMergeApplies(t *testing.T) {
	// An override that adds an unprotected scalar and a nested field merges in. (Values are
	// strings/maps so they round-trip through the JSON patch unambiguously.)
	p := managedMQPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Messaging.Managed.Overrides = rawExt(t, map[string]interface{}{
			"spec": map[string]interface{}{
				"image": "my-mirror/rabbitmq:4.0-management",
				"rabbitmq": map[string]interface{}{
					"envConfig": "RABBITMQ_LOG=info",
				},
			},
		})
	})
	cluster := findManagedObj(ResolveManagedMessaging(p), rmqKindCluster)
	require.NotNil(t, cluster)
	require.NoError(t, ManagedMessagingRenderError(cluster), "a non-protected override must apply cleanly")

	image, _, _ := unstructured.NestedString(cluster.Object, "spec", "image")
	assert.Equal(t, "my-mirror/rabbitmq:4.0-management", image, "an unprotected override replaces the rendered value")
	envConfig, _, _ := unstructured.NestedString(cluster.Object, "spec", "rabbitmq", "envConfig")
	assert.Equal(t, "RABBITMQ_LOG=info", envConfig, "a nested unprotected override merges in")
	// The operator-rendered fields survive the merge (replicas untouched).
	replicas, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "replicas")
	assert.Equal(t, int64(3), replicas)
}

func TestManagedMessagingOverridesRejectProtectedAdditionalConfig(t *testing.T) {
	// default_user/default_pass live under spec.rabbitmq.additionalConfig — protected.
	p := managedMQPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Messaging.Managed.Overrides = rawExt(t, map[string]interface{}{
			"spec": map[string]interface{}{
				"rabbitmq": map[string]interface{}{
					"additionalConfig": "default_user = evil\ndefault_pass = hijack\n",
				},
			},
		})
	})
	cluster := findManagedObj(ResolveManagedMessaging(p), rmqKindCluster)
	require.NotNil(t, cluster)
	err := ManagedMessagingRenderError(cluster)
	require.Error(t, err, "overriding the default-user wiring (a topology-contract field) must be rejected")
	assert.Contains(t, err.Error(), "spec.rabbitmq.additionalConfig")
	// The protected field must NOT have been merged in.
	_, found, _ := unstructured.NestedString(cluster.Object, "spec", "rabbitmq", "additionalConfig")
	assert.False(t, found, "the rejected override must not seed the protected field")
}

func TestManagedMessagingOverridesRejectProtectedMetadataName(t *testing.T) {
	p := managedMQPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Messaging.Managed.Overrides = rawExt(t, map[string]interface{}{
			"metadata": map[string]interface{}{"name": "hijack"},
		})
	})
	cluster := findManagedObj(ResolveManagedMessaging(p), rmqKindCluster)
	require.NotNil(t, cluster)
	err := ManagedMessagingRenderError(cluster)
	require.Error(t, err, "overriding metadata.name must be rejected")
	assert.Contains(t, err.Error(), "metadata.name")
	assert.Equal(t, testMessagingName, cluster.GetName(), "the rejected override must not rename the Cluster")
}

func TestManagedMessagingOverridesMalformedRejected(t *testing.T) {
	p := managedMQPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Messaging.Managed.Overrides = &runtime.RawExtension{Raw: []byte(`"not an object"`)}
	})
	cluster := findManagedObj(ResolveManagedMessaging(p), rmqKindCluster)
	require.NotNil(t, cluster)
	require.Error(t, ManagedMessagingRenderError(cluster), "a non-object override must be rejected")
}

func TestManagedMessagingDependencies(t *testing.T) {
	// External: no deps.
	ext := &otilmv1alpha1.Platform{Spec: otilmv1alpha1.PlatformSpec{Messaging: otilmv1alpha1.MessagingSpec{Mode: "external"}}}
	assert.Nil(t, MessagingDependencies(ext))

	// Managed: the Cluster Operator AND the Topology Operator CRDs.
	p := managedMQPlatform(nil)
	deps := MessagingDependencies(p)
	require.Len(t, deps, 2)
	assert.Equal(t, rabbitmqGroup, deps[0].GroupKind.Group)
	assert.Equal(t, rmqKindCluster, deps[0].GroupKind.Kind)
	assert.Equal(t, ReasonRabbitMQNotInstalled, deps[0].Reason)
	assert.Contains(t, deps[0].Message, "spec.messaging.mode=external", "the message names the escape hatch")

	assert.Equal(t, rmqKindVhost, deps[1].GroupKind.Kind, "the second dep probes the Topology Operator")
	assert.Equal(t, ReasonTopologyOperatorNotInstalled, deps[1].Reason)
}

// TestManagedMessagingNoLeak asserts the rendered objects carry no credential material —
// only the non-secret sizing/topology data RabbitMQ needs. (The Topology Operator
// generates the per-user Secrets; the operator never inlines credentials.)
func TestManagedMessagingNoLeak(t *testing.T) {
	p := managedMQPlatform(nil)
	objs := ResolveManagedMessaging(p)
	require.NotEmpty(t, objs)
	for _, o := range objs {
		u := o.(*unstructured.Unstructured)
		b, err := json.Marshal(u.Object)
		require.NoError(t, err)
		body := string(b)
		for _, forbidden := range []string{"password", "Password", "secretKey", "stringData", "importCredentialsSecret"} {
			assert.NotContains(t, body, forbidden, "the %s object must carry no credential material", u.GetKind())
		}
	}
}

// TestManagedMessagingTopologyIsBOMData proves the builder loops over the versioned BOM
// data (not scattered literals): the rendered users/exchanges/queues/bindings counts equal
// the BOM topology's.
func TestManagedMessagingTopologyIsBOMData(t *testing.T) {
	topo := bom.Messaging()
	p := managedMQPlatform(nil)
	objs := ResolveManagedMessaging(p)
	assert.Len(t, findManagedObjs(objs, rmqKindUser), len(topo.Users))
	assert.Len(t, findManagedObjs(objs, rmqKindPermission), len(topo.Users))
	assert.Len(t, findManagedObjs(objs, rmqKindExchange), len(topo.Exchanges))
	assert.Len(t, findManagedObjs(objs, rmqKindQueue), len(topo.Queues))
	assert.Len(t, findManagedObjs(objs, rmqKindBinding), len(topo.Bindings))
}

// TestResolveManagedMessaging2190 proves a spec.version 2.19.0 platform renders the
// renamed topology: vhost "/", 35 objects (1 cluster + 1 vhost + 5 users + 5
// permissions + 2 exchanges + 11 queues + 10 bindings), and the status-poll queue —
// with every vhost-bound object name carrying the "/" vhost's "-default" scope.
func TestResolveManagedMessaging2190(t *testing.T) {
	p := managedMQPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Version = testVersion219 })
	objs := ResolveManagedMessaging(p)
	assert.Len(t, objs, 35)

	vhost := findManagedObj(objs, rmqKindVhost)
	require.NotNil(t, vhost)
	name, _, _ := unstructured.NestedString(vhost.Object, "spec", "name")
	assert.Equal(t, "/", name)
	assert.Equal(t, "ilm-messaging-default-vhost", vhost.GetName(),
		"the 2.19.0 vhost CR is scoped by its \"/\" vhost")
	assert.Equal(t, "ilm-messaging-default-vhost", ManagedMessagingVhostName(p),
		"the controller's Vhost GET must address the SAME object the builder renders")
	for _, n := range vhostScopedNames(objs) {
		assert.True(t, strings.HasPrefix(n, "ilm-messaging-default-"),
			"vhost-bound object %q must carry the \"/\" vhost scope", n)
	}
	assert.Equal(t, []string{
		"ilm-messaging-default-exchange-ilm",
		"ilm-messaging-default-exchange-ilm-proxy",
	}, namesOfKinds(objs, rmqKindExchange))
	assert.Contains(t, namesOfKinds(objs, rmqKindQueue), "ilm-messaging-default-queue-provider-status-poll")

	queues := findManagedObjs(objs, rmqKindQueue)
	require.Len(t, queues, 11)
	var queueNames []string
	for _, q := range queues {
		n, _, _ := unstructured.NestedString(q.Object, "spec", "name")
		queueNames = append(queueNames, n)
	}
	assert.Contains(t, queueNames, "provider.status-poll")
}

// TestManagedMessagingReclaimKinds pins the one list in the builder that authorises DELETION:
// the classes a messaging migration reclaims from the virtual host it moved away from, in the
// order the Topology Operator's finalizers require, with the two Kinds that must never be
// deleted absent from it.
func TestManagedMessagingReclaimKinds(t *testing.T) {
	kinds := ManagedMessagingReclaimKinds()

	assert.Equal(t, []string{rmqKindBinding, rmqKindQueue, rmqKindExchange, rmqKindPermission, rmqKindVhost}, kinds,
		"a dependent deleted after what it depends on is stranded behind a finalizer")
	assert.NotContains(t, kinds, rmqKindCluster, "the broker itself goes on serving the target topology")
	assert.NotContains(t, kinds, rmqKindUser,
		"broker users are global, shared with the target topology, and own the credentials Secrets every component reads")

	// Every reclaimable Kind is one the renderer actually produces, so the list can never name a
	// class that would silently reclaim nothing.
	p := managedMQPlatform(nil)
	rendered := map[string]bool{}
	for _, obj := range ResolveManagedMessaging(p) {
		rendered[obj.GetObjectKind().GroupVersionKind().Kind] = true
	}
	for _, kind := range kinds {
		assert.True(t, rendered[kind], "the render must produce a %s for the reclaim to address", kind)
	}
}
