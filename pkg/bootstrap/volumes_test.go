/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package bootstrap_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/bootstrap"
)

func volumeParams() bootstrap.Params {
	p := baseParams("amd64", v1alpha1.NetworkTemplateDirect)
	p.Volumes = []bootstrap.VolumeHint{
		{Name: "pgdata", Filesystem: "xfs", MountPath: "/var/lib/pgdata", ByID: "/dev/disk/by-id/scsi-0HC_Volume_123"},
		{Name: "scratch", Filesystem: "ext4", MountPath: "/mnt/scratch-disk", SerialPattern: "vol*", SizeBytes: 100 << 30},
	}
	p.Local = &bootstrap.LocalHint{Policy: "raid0", MountPath: "/mnt/local", DevicePattern: "nvme-Amazon_EC2_NVMe_Instance_Storage_*", Count: 2}
	return p
}

func TestGoldenVolumeUnit(t *testing.T) {
	r, err := bootstrap.Render(volumeParams())
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(r.Parts))
	for _, p := range r.Parts {
		names = append(names, p.Name)
	}
	if got := strings.Join(names, ","); got != "base,volumes,network" {
		t.Fatalf("parts %s, want base,volumes,network", got)
	}
	testutil.Golden(t, "render-volumes.golden", goldenText(t, r))
	t.Logf("%d bytes of user-data with two volumes and local storage", len(r.UserData))
}

func TestNoHintsNoVolumePart(t *testing.T) {
	r, err := bootstrap.Render(baseParams("amd64", v1alpha1.NetworkTemplateDirect))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range r.Parts {
		if p.Name == "volumes" {
			t.Error("a node without volumes got the volume unit")
		}
	}
}

func TestVolumeUnitFitsTheBudgetWithEveryVolume(t *testing.T) {
	p := baseParams("amd64", v1alpha1.NetworkTemplateTailscale)
	for i := range 8 {
		p.Volumes = append(p.Volumes, bootstrap.VolumeHint{
			Name: fmt.Sprintf("data-%d", i), Filesystem: "xfs", MountPath: fmt.Sprintf("/mnt/data-%d", i),
			SerialPattern: "vol*", SizeBytes: int64(i+1) << 30,
		})
	}
	p.Local = &bootstrap.LocalHint{Policy: "mount", MountPath: "/mnt/local", DevicePattern: "nvme-*", Count: 4}
	p.KubeReserved = map[string]string{"cpu": "70m", "memory": "1Gi", "ephemeral-storage": "1Gi"}
	r, err := bootstrap.Render(p)
	if err != nil {
		t.Fatalf("eight volumes and local storage do not render: %v", err)
	}
	if len(r.UserData) > bootstrap.MaxUserDataSize {
		t.Errorf("%d bytes", len(r.UserData))
	}
	t.Logf("tailscale template, eight volumes and four local disks: %d of %d bytes", len(r.UserData), bootstrap.MaxUserDataSize)
}

func TestVolumeHintValidation(t *testing.T) {
	good := bootstrap.VolumeHint{Name: "data", Filesystem: "ext4", MountPath: "/data", ByID: "/dev/disk/by-id/x"}
	mut := func(f func(*bootstrap.VolumeHint)) bootstrap.VolumeHint { v := good; f(&v); return v }
	tests := map[string]bootstrap.VolumeHint{
		"upper-case name":    mut(func(v *bootstrap.VolumeHint) { v.Name = "Data" }),
		"unknown filesystem": mut(func(v *bootstrap.VolumeHint) { v.Filesystem = "ntfs" }),
		"no device":          mut(func(v *bootstrap.VolumeHint) { v.ByID = "" }),
		"both devices":       mut(func(v *bootstrap.VolumeHint) { v.SerialPattern = "x" }),
		"relative path":      mut(func(v *bootstrap.VolumeHint) { v.MountPath = "data" }),
		"root":               mut(func(v *bootstrap.VolumeHint) { v.MountPath = "/" }),
		"traversal":          mut(func(v *bootstrap.VolumeHint) { v.MountPath = "/a/../b" }),
		"shell characters":   mut(func(v *bootstrap.VolumeHint) { v.MountPath = "/a b;rm" }),
		"by-id injection":    mut(func(v *bootstrap.VolumeHint) { v.ByID = "/dev/disk/by-id/x --fs xfs" }),
		"serial injection": mut(func(v *bootstrap.VolumeHint) {
			v.ByID, v.SerialPattern = "", "x; reboot"
		}),
	}
	for name, v := range tests {
		t.Run(name, func(t *testing.T) {
			p := baseParams("amd64", v1alpha1.NetworkTemplateDirect)
			p.Volumes = []bootstrap.VolumeHint{v}
			if _, err := bootstrap.Render(p); err == nil {
				t.Errorf("%+v rendered", v)
			}
		})
	}
	p := baseParams("amd64", v1alpha1.NetworkTemplateDirect)
	p.Volumes = []bootstrap.VolumeHint{good, mut(func(v *bootstrap.VolumeHint) { v.Name = "other" })}
	if _, err := bootstrap.Render(p); err == nil {
		t.Error("two volumes at one mount path rendered")
	}
	p.Volumes = []bootstrap.VolumeHint{good, mut(func(v *bootstrap.VolumeHint) { v.MountPath = "/other" })}
	if _, err := bootstrap.Render(p); err == nil {
		t.Error("two volumes with one name rendered")
	}
	p.Volumes = nil
	for name, l := range map[string]*bootstrap.LocalHint{
		"policy":  {Policy: "ignore", MountPath: "/mnt/l", DevicePattern: "x*"},
		"pattern": {Policy: "mount", MountPath: "/mnt/l", DevicePattern: "x; y"},
		"path":    {Policy: "mount", MountPath: "mnt", DevicePattern: "x*"},
		"count":   {Policy: "mount", MountPath: "/mnt/l", DevicePattern: "x*", Count: 1000},
	} {
		p.Local = l
		if _, err := bootstrap.Render(p); err == nil {
			t.Errorf("a local storage hint with a bad %s rendered", name)
		}
	}
}

func TestKubeReservedOverridesTheKubeletConfiguration(t *testing.T) {
	p := baseParams("amd64", v1alpha1.NetworkTemplateDirect)
	p.KubeReserved = map[string]string{"cpu": "80m", "memory": "1566Mi", "ephemeral-storage": "1Gi"}
	r, err := bootstrap.Render(p)
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(r.Parts[0].Ignition)
	var parsed struct {
		Storage struct {
			Files []struct {
				Path     string
				Contents struct{ Source string }
			}
		}
	}
	if err := json.Unmarshal(r.Parts[0].Ignition, &parsed); err != nil {
		t.Fatal(err)
	}
	text := readable(t, r.Parts[0].Ignition)
	for _, want := range []string{"kubeReserved", "cpu: 80m", "memory: 1566Mi", "ephemeral-storage: 1Gi"} {
		if !strings.Contains(text, want) {
			t.Errorf("the kubelet configuration lacks %q:\n%s", want, text)
		}
	}
	_ = cfg
	// Without it the NodeClass's own value is kept.
	p = baseParams("amd64", v1alpha1.NetworkTemplateDirect)
	p.Kubelet.KubeReserved = map[string]string{"cpu": "123m"}
	r, err = bootstrap.Render(p)
	if err != nil {
		t.Fatal(err)
	}
	if text := readable(t, r.Parts[0].Ignition); !strings.Contains(text, "cpu: 123m") {
		t.Errorf("the NodeClass's kubeReserved is gone:\n%s", text)
	}
}

func TestZoneAndInstanceTypeAreRendered(t *testing.T) {
	p := baseParams("amd64", v1alpha1.NetworkTemplateCustom)
	p.Zone, p.InstanceType = "us-east-1b", "m7i.large"
	p.Network.CustomSource = "test"
	p.Network.Custom = `variant: flatcar
version: 1.1.0
storage:
  files:
    - path: /etc/scarpenter/where
      mode: 0644
      contents:
        inline: "{{ .Zone }} {{ .InstanceType }}\n"
`
	r, err := bootstrap.Render(p)
	if err != nil {
		t.Fatal(err)
	}
	if text := readable(t, r.Parts[1].Ignition); !strings.Contains(text, "us-east-1b m7i.large") {
		t.Errorf("zone and instance type are not in the rendering:\n%s", text)
	}
}

// The first-boot scripts run against stubs of the block device tools.
type stubs struct {
	dir string
}

func newStubs(t *testing.T) *stubs {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/bash\n"+body), 0o755); err != nil { //nolint:gosec // test stubs must be executable
			t.Fatal(err)
		}
	}
	// lsblk -dnbp ... lists $STUB/disks (path size type serial); lsblk -nr
	// lists the device and its partitions from $STUB/children-<base>.
	write("lsblk", `
case "$*" in
  *-dnbp*) cat "$STUB/disks" ;;
  *-nr*) b="$(basename "${@: -1}")"; if [ -f "$STUB/children-$b" ]; then cat "$STUB/children-$b"; else echo "$b"; fi ;;
esac
`)
	write("blkid", `
want="${@: -2:1}"; dev="${@: -1}"; b="$(basename "$dev")"
f="$STUB/$want-$b"
[ -f "$f" ] || exit 2
cat "$f"
`)
	write("findmnt", `exit 1`)
	// mkfs takes a moment and then leaves its label behind, as the real one does:
	// a second unit that looks while the first one works must see the lock, not
	// a blank disk.
	for _, fs := range []string{"ext4", "xfs", "btrfs"} {
		write("mkfs."+fs, `
echo "$0 $*" >> "$STUB/mkfs.log"
sleep 0.3
label=""; prev=""
for a in "$@"; do if [ "$prev" = -L ]; then label="$a"; fi; prev="$a"; done
dev="${@: -1}"; b="$(basename "$dev")"
printf '%s\n' "$label" > "$STUB/LABEL-$b"
echo ext4 > "$STUB/TYPE-$b"
`)
	}
	write("mdadm", `
if [ "$1" = --examine ]; then f="$STUB/examine-$(basename "$2")"; [ -f "$f" ] && cat "$f"; exit 0; fi
echo "mdadm $*" >> "$STUB/mdadm.log"`)
	return &stubs{dir: dir}
}

func (s *stubs) set(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// run executes a script of the templates directory with the stubs first in PATH.
func (s *stubs) run(t *testing.T, script string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{filepath.Join("templates", script)}, args...)...) //nolint:gosec // the script is the repository's
	cmd.Env = append(os.Environ(), "PATH="+s.dir+":"+os.Getenv("PATH"), "STUB="+s.dir, "SCARPENTER_BY_ID_DIR="+filepath.Join(s.dir, "by-id"),
		"SCARPENTER_VOLUME_LOCK="+filepath.Join(s.dir, "lock"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (s *stubs) mkfsLog(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(s.dir, "mkfs.log")) //nolint:gosec // temp dir of the test
	return string(b)
}

func TestVolumeScriptCreatesTheFilesystemOnceAndNeverWipes(t *testing.T) {
	t.Run("a blank device by id gets the label", func(t *testing.T) {
		s := newStubs(t)
		s.set(t, "disk", "")
		dev := filepath.Join(s.dir, "disk")
		out, err := s.run(t, "scarpenter-volume.sh", "--label", "scv-aaaa", "--fs", "xfs", "--by-id", dev, "--timeout", "0")
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if log := s.mkfsLog(t); !strings.Contains(log, "mkfs.xfs -q -L scv-aaaa "+dev) {
			t.Errorf("mkfs log %q", log)
		}
	})
	t.Run("a device that already carries the label is left alone", func(t *testing.T) {
		s := newStubs(t)
		s.set(t, "disk", "")
		s.set(t, "LABEL-disk", "scv-aaaa\n")
		s.set(t, "TYPE-disk", "xfs\n")
		out, err := s.run(t, "scarpenter-volume.sh", "--label", "scv-aaaa", "--fs", "xfs", "--by-id", filepath.Join(s.dir, "disk"), "--timeout", "0")
		if err != nil || s.mkfsLog(t) != "" {
			t.Errorf("rebooted node: err %v, mkfs %q, output %s", err, s.mkfsLog(t), out)
		}
	})
	t.Run("a foreign filesystem is never overwritten", func(t *testing.T) {
		s := newStubs(t)
		s.set(t, "disk", "")
		s.set(t, "TYPE-disk", "ext4\n")
		s.set(t, "LABEL-disk", "precious\n")
		out, err := s.run(t, "scarpenter-volume.sh", "--label", "scv-aaaa", "--fs", "xfs", "--by-id", filepath.Join(s.dir, "disk"), "--timeout", "0")
		if err == nil || s.mkfsLog(t) != "" || !strings.Contains(out, "refusing to overwrite") {
			t.Errorf("foreign filesystem: err %v, mkfs %q, output %s", err, s.mkfsLog(t), out)
		}
	})
	t.Run("a missing device fails instead of degrading", func(t *testing.T) {
		s := newStubs(t)
		out, err := s.run(t, "scarpenter-volume.sh", "--label", "scv-aaaa", "--fs", "ext4", "--by-id", filepath.Join(s.dir, "absent"), "--timeout", "0")
		if err == nil || !strings.Contains(out, "no device") {
			t.Errorf("err %v, output %s", err, out)
		}
	})
	t.Run("serial pattern and size pick the right disk among others", func(t *testing.T) {
		s := newStubs(t)
		s.set(t, "disks", strings.Join([]string{
			"/dev/xvda 8589934592 disk vol0root",     // the root disk: another size
			"/dev/xvdf 107374182400 disk othermodel", // the serial does not match
			"/dev/xvdg 107374182400 disk vol0aaa",    // one of our other volumes
			"/dev/xvdh 107374182400 disk vol0bbb",    // blank: ours
			"/dev/xvdi 107374182400 part vol0ccc",    // not a disk
		}, "\n")+"\n")
		s.set(t, "LABEL-xvdg", "scv-other\n")
		s.set(t, "TYPE-xvdg", "xfs\n")
		out, err := s.run(t, "scarpenter-volume.sh", "--label", "scv-mine", "--fs", "ext4", "--serial", "vol0[ab]*", "--size", "107374182400", "--timeout", "0")
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if log := s.mkfsLog(t); !strings.Contains(log, "mkfs.ext4 -q -F -L scv-mine /dev/xvdh") || strings.Count(log, "mkfs") != 1 {
			t.Errorf("mkfs log %q", log)
		}
	})
	t.Run("a disk with partitions is not a candidate", func(t *testing.T) {
		s := newStubs(t)
		s.set(t, "disks", "/dev/xvdf 100 disk vol0a\n")
		s.set(t, "children-xvdf", "xvdf\nxvdf1\n")
		out, err := s.run(t, "scarpenter-volume.sh", "--label", "scv-mine", "--fs", "ext4", "--serial", "vol*", "--size", "100", "--timeout", "0")
		if err == nil || s.mkfsLog(t) != "" {
			t.Errorf("a partitioned disk was used: %v, %s, %s", err, s.mkfsLog(t), out)
		}
	})
	t.Run("two volumes of the same size never take the same disk", func(t *testing.T) {
		s := newStubs(t)
		s.set(t, "disks", "/dev/nvme1n1 107374182400 disk vol0aaa\n/dev/nvme2n1 107374182400 disk vol0bbb\n")
		lock := filepath.Join(s.dir, "lock")
		var wg sync.WaitGroup
		for _, label := range []string{"scv-aaaaaaaa", "scv-bbbbbbbb"} {
			wg.Go(func() {
				cmd := exec.Command("bash", filepath.Join("templates", "scarpenter-volume.sh"), "--label", label, "--fs", "ext4", //nolint:gosec // repository script
					"--serial", "vol*", "--size", "107374182400", "--timeout", "0")
				cmd.Env = append(os.Environ(), "PATH="+s.dir+":"+os.Getenv("PATH"), "STUB="+s.dir, "SCARPENTER_VOLUME_LOCK="+lock)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Errorf("%s: %v: %s", label, err, out)
				}
			})
		}
		wg.Wait()
		log := s.mkfsLog(t)
		if strings.Count(log, "/dev/nvme1n1") != 1 || strings.Count(log, "/dev/nvme2n1") != 1 {
			t.Errorf("the two volumes did not get one disk each:\n%s", log)
		}
	})
	t.Run("the disk that carries the volume's label wins over an earlier blank one", func(t *testing.T) {
		s := newStubs(t)
		s.set(t, "disks", "/dev/nvme1n1 100 disk vol0a\n/dev/nvme2n1 100 disk vol0b\n")
		s.set(t, "LABEL-nvme2n1", "scv-mine\n")
		s.set(t, "TYPE-nvme2n1", "ext4\n")
		out, err := s.run(t, "scarpenter-volume.sh", "--label", "scv-mine", "--fs", "ext4", "--serial", "vol*", "--size", "100", "--timeout", "0")
		if err != nil || s.mkfsLog(t) != "" {
			t.Errorf("a reboot with a new blank disk: err %v, mkfs %q, output %s", err, s.mkfsLog(t), out)
		}
	})
	t.Run("a disk with a foreign filesystem is skipped for the next one", func(t *testing.T) {
		s := newStubs(t)
		s.set(t, "disks", "/dev/nvme1n1 100 disk vol0a\n/dev/nvme2n1 100 disk vol0b\n")
		s.set(t, "TYPE-nvme1n1", "ext4\n")
		out, err := s.run(t, "scarpenter-volume.sh", "--label", "scv-mine", "--fs", "ext4", "--serial", "vol*", "--size", "100", "--timeout", "0")
		if err != nil || !strings.Contains(s.mkfsLog(t), "/dev/nvme2n1") || strings.Contains(s.mkfsLog(t), "/dev/nvme1n1") {
			t.Errorf("err %v, mkfs %q, output %s", err, s.mkfsLog(t), out)
		}
	})
	t.Run("bad arguments", func(t *testing.T) {
		s := newStubs(t)
		if _, err := s.run(t, "scarpenter-volume.sh", "--label", "x"); err == nil {
			t.Error("missing arguments were accepted")
		}
		if _, err := s.run(t, "scarpenter-volume.sh", "--bogus", "x"); err == nil {
			t.Error("an unknown argument was accepted")
		}
	})
}

func TestLocalStorageScript(t *testing.T) {
	prepare := func(t *testing.T, n int) *stubs {
		s := newStubs(t)
		if err := os.MkdirAll(filepath.Join(s.dir, "by-id"), 0o750); err != nil {
			t.Fatal(err)
		}
		for i := range n {
			disk := filepath.Join(s.dir, fmt.Sprintf("nvme%dn1", i))
			s.set(t, fmt.Sprintf("nvme%dn1", i), "")
			if err := os.Symlink(disk, filepath.Join(s.dir, "by-id", fmt.Sprintf("nvme-Instance_Storage_%d", i))); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(disk, filepath.Join(s.dir, "by-id", fmt.Sprintf("nvme-Instance_Storage_%d-part1", i))); err != nil {
				t.Fatal(err)
			}
		}
		return s
	}
	t.Run("mount formats every disk once", func(t *testing.T) {
		s := prepare(t, 2)
		out, err := s.run(t, "scarpenter-local-storage.sh", "--policy", "mount", "--pattern", "nvme-Instance_Storage_*", "--count", "2", "--timeout", "0")
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		log := s.mkfsLog(t)
		if strings.Count(log, "mkfs.ext4") != 2 || !strings.Contains(log, "-L scl-0 ") || !strings.Contains(log, "-L scl-1 ") {
			t.Errorf("mkfs log %q (partition links must be ignored)", log)
		}
	})
	t.Run("raid0 stripes the disks", func(t *testing.T) {
		s := prepare(t, 2)
		// The array device does not exist in the stub: make the label check see a blank md device.
		out, err := s.run(t, "scarpenter-local-storage.sh", "--policy", "raid0", "--pattern", "nvme-Instance_Storage_*", "--count", "2", "--timeout", "0")
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		b, _ := os.ReadFile(filepath.Join(s.dir, "mdadm.log")) //nolint:gosec // temp dir of the test
		if !strings.Contains(string(b), "--create /dev/md/scarpenter-local") || !strings.Contains(string(b), "--raid-devices=2") {
			t.Errorf("mdadm log %q", b)
		}
	})
	t.Run("raid0 never takes a disk with a foreign filesystem", func(t *testing.T) {
		s := prepare(t, 2)
		s.set(t, "TYPE-nvme1n1", "xfs\n")
		out, err := s.run(t, "scarpenter-local-storage.sh", "--policy", "raid0", "--pattern", "nvme-Instance_Storage_*", "--count", "2", "--timeout", "0")
		b, _ := os.ReadFile(filepath.Join(s.dir, "mdadm.log")) //nolint:gosec // temp dir of the test
		if err == nil || len(b) != 0 || !strings.Contains(out, "refusing") {
			t.Errorf("err %v, mdadm %q, output %s", err, b, out)
		}
	})
	t.Run("raid0 members of our array are assembled, not created again", func(t *testing.T) {
		s := prepare(t, 2)
		for i := range 2 {
			s.set(t, fmt.Sprintf("TYPE-nvme%dn1", i), "linux_raid_member\n")
			s.set(t, fmt.Sprintf("examine-nvme%dn1", i), "Name : any:scarpenter-local\n")
		}
		out, err := s.run(t, "scarpenter-local-storage.sh", "--policy", "raid0", "--pattern", "nvme-Instance_Storage_*", "--count", "2", "--timeout", "0")
		b, _ := os.ReadFile(filepath.Join(s.dir, "mdadm.log")) //nolint:gosec // temp dir of the test
		if err != nil || !strings.Contains(string(b), "--assemble") || strings.Contains(string(b), "--create") {
			t.Errorf("err %v, mdadm %q, output %s", err, b, out)
		}
	})
	t.Run("raid0 members of another array are refused", func(t *testing.T) {
		s := prepare(t, 1)
		s.set(t, "TYPE-nvme0n1", "linux_raid_member\n")
		s.set(t, "examine-nvme0n1", "Name : host:data\n")
		_, err := s.run(t, "scarpenter-local-storage.sh", "--policy", "raid0", "--pattern", "nvme-Instance_Storage_*", "--count", "1", "--timeout", "0")
		if err == nil {
			t.Error("a member of someone else's array was used")
		}
	})
	t.Run("fewer disks than expected fails", func(t *testing.T) {
		s := prepare(t, 1)
		out, err := s.run(t, "scarpenter-local-storage.sh", "--policy", "mount", "--pattern", "nvme-Instance_Storage_*", "--count", "2", "--timeout", "0")
		if err == nil || !strings.Contains(out, "1 of 2") {
			t.Errorf("err %v, output %s", err, out)
		}
	})
	t.Run("a foreign signature is never overwritten", func(t *testing.T) {
		s := prepare(t, 1)
		s.set(t, "TYPE-nvme0n1", "ext4\n")
		out, err := s.run(t, "scarpenter-local-storage.sh", "--policy", "mount", "--pattern", "nvme-Instance_Storage_*", "--count", "1", "--timeout", "0")
		if err == nil || s.mkfsLog(t) != "" || !strings.Contains(out, "refusing") {
			t.Errorf("err %v, mkfs %q, output %s", err, s.mkfsLog(t), out)
		}
	})
}
