/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

// Names of the seeded world of NewSeeded.
const (
	SeedRegion   = "europe-west3"
	SeedZoneA    = "europe-west3-a"
	SeedZoneB    = "europe-west3-b"
	SeedOtherReg = "us-central1"
	SeedOtherZ   = "us-central1-a"
	// SeedImageProject and SeedImage are Flatcar's public project and a pinned
	// amd64 image there; SeedImageARM is an arm64 image in a second project.
	SeedImageProject    = "kinvolk-public"
	SeedImage           = "flatcar-stable-4757-2-3"
	SeedARMImageProject = "spare-cores-images"
	SeedImageARM        = "flatcar-stable-4757-2-3-arm64"
	SeedSubnet          = "nodes"
	SeedSubnetCIDR      = "10.240.0.0/20"
)

// NewSeeded returns a fake with two regions (europe-west3 with zones a and b,
// ids 2000 and 2001; us-central1 with zone a, id 2002), the machine types
// e2-small, n2-standard-2 and c3-standard-4 (amd64) and t2a-standard-1 (arm64) in
// every zone, the Flatcar images, quota limits (CPUS 24, N2_CPUS 8,
// PREEMPTIBLE_CPUS 0 per region; CPUS_ALL_REGIONS 32 for the project), and a
// custom network "vpc" with the subnet "nodes" in europe-west3. It has no
// default network.
func NewSeeded(project string) *Compute {
	c := New(project)
	limits := map[string]float64{"CPUS": 24, "N2_CPUS": 8, "C3_CPUS": 8, "PREEMPTIBLE_CPUS": 0, "INSTANCES": 240}
	c.AddRegion(SeedRegion, limits)
	c.AddRegion(SeedOtherReg, limits)
	c.SetLimit("", "CPUS_ALL_REGIONS", 32)
	c.AddZone(SeedRegion, SeedZoneA, 2000)
	c.AddZone(SeedRegion, SeedZoneB, 2001)
	c.AddZone(SeedOtherReg, SeedOtherZ, 2002)
	for _, z := range []string{SeedZoneA, SeedZoneB, SeedOtherZ} {
		c.AddMachineType(z, "e2-small", 2, 2048, "X86_64")
		c.AddMachineType(z, "n2-standard-2", 2, 8192, "X86_64")
		c.AddMachineType(z, "c3-standard-4", 4, 16384, "X86_64")
		c.AddMachineType(z, "t2a-standard-1", 1, 4096, "ARM64")
	}
	c.AddImage(SeedImageProject, SeedImage, "flatcar-stable", "X86_64",
		"GVNIC", "UEFI_COMPATIBLE", "VIRTIO_SCSI_MULTIQUEUE")
	c.AddImage(SeedARMImageProject, SeedImageARM, "flatcar-stable-arm64", "ARM64", "GVNIC", "UEFI_COMPATIBLE")
	c.seedNetwork()
	return c
}
