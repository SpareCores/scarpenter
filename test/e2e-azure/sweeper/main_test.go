/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestParseTakesTheEnvironmentAndTheFlags(t *testing.T) {
	var buf bytes.Buffer
	s, err := parse([]string{"-dry-run"}, env(map[string]string{
		"AZURE_SUBSCRIPTION_ID": "00000000-0000-0000-0000-000000000000", "AZURE_RESOURCE_GROUP": "rg-example",
	}), &buf)
	require.NoError(t, err)
	require.Equal(t, "00000000-0000-0000-0000-000000000000", s.subscription)
	require.Equal(t, "rg-example", s.opts.ResourceGroup)
	require.Equal(t, "scarpenter-ci-", s.opts.ClusterPrefix)
	require.Equal(t, 2*time.Hour, s.opts.MaxAge)
	require.True(t, s.opts.DryRun)

	s, err = parse([]string{"-subscription", "x", "-resource-group", "g", "-cluster", "scarpenter-ci-7", "-max-age", "0"}, env(nil), &buf)
	require.NoError(t, err)
	require.Equal(t, "scarpenter-ci-7", s.opts.Cluster)
	require.Empty(t, s.opts.ClusterPrefix, "one cluster by name, not by prefix")
	require.Zero(t, s.opts.MaxAge)
}

func TestParseRefusesWhatIsMissing(t *testing.T) {
	var buf bytes.Buffer
	_, err := parse(nil, env(nil), &buf)
	require.ErrorContains(t, err, "AZURE_SUBSCRIPTION_ID")
	require.ErrorContains(t, err, "AZURE_RESOURCE_GROUP")
	_, err = parse([]string{"-subscription", "x", "-resource-group", "g", "-max-age", "-1h"}, env(nil), &buf)
	require.Error(t, err)
}

func TestHelpAndUsageErrorsNeedNoCredentials(t *testing.T) {
	var out, errb bytes.Buffer
	require.Equal(t, 0, run(context.Background(), []string{"--help"}, env(nil), &out, &errb))
	require.Contains(t, errb.String(), "-resource-group")
	errb.Reset()
	require.Equal(t, 2, run(context.Background(), nil, env(nil), &out, &errb))
	require.Contains(t, errb.String(), "missing")
	require.Equal(t, 2, run(context.Background(), []string{"-nope"}, env(nil), &out, &errb))
}
