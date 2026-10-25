/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package utils

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKeycloakInstallationOwnership(t *testing.T) {
	for _, tc := range []struct {
		name     string
		output   string
		failure  bool
		present  bool
		complete bool
	}{
		{name: "empty cluster"},
		{name: "unrelated CRDs", output: "clusters.postgresql.cnpg.io"},
		{name: "previous operator", output: "keycloaks.k8s.keycloak.org\nkeycloakrealmimports.k8s.keycloak.org", present: true},
		{name: "partial client installation", output: "keycloakoidcclients.k8s.keycloak.org", present: true},
		{name: "complete installation", output: "keycloaks.k8s.keycloak.org\nkeycloakrealmimports.k8s.keycloak.org\nkeycloakoidcclients.k8s.keycloak.org\nkeycloaksamlclients.k8s.keycloak.org", present: true, complete: true},
		{name: "unreachable cluster", failure: true, present: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			stub := "#!/bin/sh\nprintf 'NAME\\n%s\\n' \"$KEYCLOAK_TEST_CRDS\"\n"
			if tc.failure {
				stub = "#!/bin/sh\nexit 1\n"
			}
			if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(stub), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("KEYCLOAK_TEST_CRDS", tc.output)
			if got := HasKeycloakOperatorCRDs(); got != tc.present {
				t.Errorf("pre-existing installation = %t, want %t; must not adopt existing CRDs for teardown", got, tc.present)
			}
			if got := IsKeycloakOperatorCRDsInstalled(); got != tc.complete {
				t.Errorf("complete installation = %t, want %t", got, tc.complete)
			}
		})
	}
}
