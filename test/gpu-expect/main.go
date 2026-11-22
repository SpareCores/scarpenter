/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Command gpu-expect prints the NVIDIA driver the table of pkg/nvidia gives a GPU
// type on a Flatcar release, for the manual `gpu` scenario of the L5 lanes
// (test/lib.sh, gpu_check_node):
//
//	gpu-expect <flatcar version> <accelerator-model label> <GPU count>
//
// It prints "<branch> <version> <flavour>" and exits 1 when the table does not
// support the type.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/SpareCores/scarpenter/pkg/nvidia"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gpu-expect:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: gpu-expect <flatcar version> <accelerator-model label> <GPU count>")
	}
	count, err := strconv.Atoi(args[2])
	if err != nil {
		return fmt.Errorf("GPU count: %w", err)
	}
	gpu := nvidia.GPU{
		Manufacturer: nvidia.Manufacturer,
		// The label value has "-" where the model has a space (RTX-PRO-6000).
		Model: strings.ReplaceAll(args[1], "-", " "),
		Count: float64(count),
		AMD64: true,
	}
	sel, reason := nvidia.Select(gpu, args[0])
	if reason != nvidia.Supported {
		return fmt.Errorf("%s x%d on Flatcar %s is not supported: %s", args[1], count, args[0], reason)
	}
	fmt.Println(sel.Branch, sel.Version, sel.Flavour)
	return nil
}
