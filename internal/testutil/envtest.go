/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package testutil holds helpers shared by test packages.
package testutil

import (
	"fmt"
	"path/filepath"
	"runtime"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	karpenterapis "sigs.k8s.io/karpenter/pkg/apis"
)

// RepoRoot returns the absolute path of the repository root.
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// KarpenterCRDs returns Karpenter core's CRDs as embedded in the
// sigs.k8s.io/karpenter module (the version in go.mod), copied so callers may
// modify them.
func KarpenterCRDs() []*apiextensionsv1.CustomResourceDefinition {
	out := make([]*apiextensionsv1.CustomResourceDefinition, 0, len(karpenterapis.CRDs))
	for _, crd := range karpenterapis.CRDs {
		out = append(out, crd.DeepCopy())
	}
	return out
}

// EnvOptions add CRDs to the envtest control plane.
type EnvOptions struct {
	// CRDs are additional CRD objects, for example KarpenterCRDs().
	CRDs []*apiextensionsv1.CustomResourceDefinition
}

// StartEnv starts an envtest control plane with the generated SCarpenter CRDs
// installed. It needs KUBEBUILDER_ASSETS (set in the dev container). The
// returned stop function must be called when the tests are done.
func StartEnv() (*rest.Config, func() error, error) {
	return StartEnvWith(EnvOptions{})
}

// StartEnvWith is StartEnv with additional CRDs.
func StartEnvWith(opts EnvOptions) (*rest.Config, func() error, error) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(RepoRoot(), "config", "crd", "bases")},
		CRDs:                  opts.CRDs,
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		return nil, nil, fmt.Errorf("starting envtest (is KUBEBUILDER_ASSETS set?): %w", err)
	}
	return cfg, env.Stop, nil
}
