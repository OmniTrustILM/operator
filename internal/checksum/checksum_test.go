package checksum_test

import (
	"testing"

	"github.com/OmniTrustILM/operator/internal/checksum"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestComputeSecretChecksum(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-secret", Namespace: "default"},
		Data: map[string][]byte{
			"username": []byte("admin"),
			"password": []byte("secret"),
		},
	}

	sum1 := checksum.ComputeSecretChecksum(secret)
	assert.NotEmpty(t, sum1)

	sum2 := checksum.ComputeSecretChecksum(secret)
	assert.Equal(t, sum1, sum2)

	secret.Data["password"] = []byte("changed")
	sum3 := checksum.ComputeSecretChecksum(secret)
	assert.NotEqual(t, sum1, sum3)
}

func TestComputeConfigMapChecksum(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cm", Namespace: "default"},
		Data: map[string]string{
			"config.yaml": "key: value",
		},
	}

	sum1 := checksum.ComputeConfigMapChecksum(cm)
	assert.NotEmpty(t, sum1)

	sum2 := checksum.ComputeConfigMapChecksum(cm)
	assert.Equal(t, sum1, sum2)

	cm.Data["config.yaml"] = "key: changed"
	sum3 := checksum.ComputeConfigMapChecksum(cm)
	assert.NotEqual(t, sum1, sum3)
}

func TestComputeCombinedChecksum(t *testing.T) {
	checksums := map[string]string{
		"secret/default/db-creds":  "abc123",
		"configmap/default/config": "def456",
	}

	combined1 := checksum.CombineChecksums(checksums)
	assert.NotEmpty(t, combined1)

	combined2 := checksum.CombineChecksums(checksums)
	assert.Equal(t, combined1, combined2)

	// Order-independent
	checksums2 := map[string]string{
		"configmap/default/config": "def456",
		"secret/default/db-creds":  "abc123",
	}
	combined3 := checksum.CombineChecksums(checksums2)
	assert.Equal(t, combined1, combined3)
}

func TestEmptyDataChecksum(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: "default"},
		Data:       map[string][]byte{},
	}
	sum := checksum.ComputeSecretChecksum(secret)
	require.NotEmpty(t, sum)
}

func TestNilChecksums(t *testing.T) {
	combined := checksum.CombineChecksums(nil)
	assert.NotEmpty(t, combined)

	combined2 := checksum.CombineChecksums(map[string]string{})
	assert.Equal(t, combined, combined2)
}
