#!/usr/bin/env bash
# The Flatcar release the e2e lanes and the canary use when nothing sets one: the one
# in-repo default. Every test/e2e-*/defaults.sh sources this file.
#
# CI does not use these values. The workflows export E2E_FLATCAR_CHANNEL and
# E2E_FLATCAR_VERSION from the repository variables FLATCAR_CHANNEL and FLATCAR_VERSION,
# set by the operator of the CI account, who also provides the images and templates of
# exactly that release in the e2e clouds. An empty variable falls back to the default
# below (":=" treats an empty value as unset). To bump the release: the operator updates
# the variables, and a change updates the pins in the repository (this file, the chart
# values, the samples, the examples and the how-to guides) so that
# hack/check-flatcar-pins.sh passes in CI.
#
# Per vendor, the release must exist as: an AWS AMI named Flatcar-<channel>-<version>-hvm,
# a GCP image flatcar-<channel>-<version with dashes>, an Azure marketplace version
# (read by the preflight and the canary), an OCI image flatcar-<channel>-<version>-<arch>
# imported into the compartment (docs/how-to-guides/set-up-oci.md), and so on.
# shellcheck disable=SC2034  # read by the scripts that source defaults.sh

: "${E2E_FLATCAR_CHANNEL:=stable}"
: "${E2E_FLATCAR_VERSION:=4757.2.3}"
