package sandbox

import (
	"archive/tar"
	"bytes"
	"strings"
	"testing"

	"github.com/yanpgwang/mango/internal/domain"
)

func TestOpenSandboxMemoryVolumesUseOneManagedClaimWithPublicAndControlMounts(t *testing.T) {
	mounts := []MemoryStoreMount{
		{
			Identity: "sesrsc_rw", StoreID: "memstore_rw",
			RuntimePath: "/mnt/memory/project", Access: domain.MemoryAccessReadWrite,
		},
		{
			Identity: "sesrsc_ro", StoreID: "memstore_ro",
			RuntimePath: "/mnt/memory/reference", Access: domain.MemoryAccessReadOnly,
		},
	}
	volumes, err := openSandboxMemoryVolumes("session", mounts)
	if err != nil {
		t.Fatal(err)
	}
	if len(volumes) != 4 {
		t.Fatalf("volumes = %d, want 4", len(volumes))
	}
	for index, mount := range mounts {
		public, control := volumes[index*2], volumes[index*2+1]
		if public.PVC == nil || control.PVC == nil ||
			public.PVC.ClaimName != control.PVC.ClaimName {
			t.Fatalf("mount %s does not share one PVC: %+v %+v", mount.Identity, public, control)
		}
		if public.PVC.CreateIfNotExists == nil || !*public.PVC.CreateIfNotExists ||
			public.PVC.DeleteOnSandboxTermination == nil ||
			!*public.PVC.DeleteOnSandboxTermination {
			t.Fatalf("mount %s PVC lifecycle = %+v", mount.Identity, public.PVC)
		}
		if public.MountPath != mount.RuntimePath ||
			control.MountPath != openSandboxMemoryControlPath(mount) {
			t.Fatalf("mount %s paths = %q, %q", mount.Identity, public.MountPath, control.MountPath)
		}
		if public.ReadOnly != (mount.Access == domain.MemoryAccessReadOnly) || control.ReadOnly {
			t.Fatalf("mount %s read-only policy = public:%t control:%t", mount.Identity, public.ReadOnly, control.ReadOnly)
		}
	}
	if volumes[0].PVC.ClaimName == volumes[2].PVC.ClaimName {
		t.Fatal("distinct Memory Stores reused one PVC")
	}
}

func TestReadOpenSandboxMemoryArchive(t *testing.T) {
	var body bytes.Buffer
	w := tar.NewWriter(&body)
	for _, header := range []*tar.Header{
		{Name: "./notes/", Typeflag: tar.TypeDir, Mode: 0o777},
		{Name: "./notes/a.md", Typeflag: tar.TypeReg, Mode: 0o666, Size: 5},
	} {
		if err := w.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := w.Write([]byte("hello")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := readOpenSandboxMemoryArchive(bytes.NewReader(body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || current[0].Path != "/notes/a.md" ||
		string(current[0].Content) != "hello" {
		t.Fatalf("current = %+v", current)
	}
}

func TestReadOpenSandboxMemoryArchiveRejectsLinksAndOversizeFiles(t *testing.T) {
	tests := []struct {
		name   string
		header *tar.Header
		body   string
	}{
		{
			name: "symbolic link",
			header: &tar.Header{
				Name: "./link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd",
			},
		},
		{
			name: "oversize",
			header: &tar.Header{
				Name: "./large", Typeflag: tar.TypeReg, Size: openSandboxMemoryFileLimit + 1,
			},
			body: strings.Repeat("x", openSandboxMemoryFileLimit+1),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var body bytes.Buffer
			w := tar.NewWriter(&body)
			if err := w.WriteHeader(test.header); err != nil {
				t.Fatal(err)
			}
			if test.body != "" {
				if _, err := w.Write([]byte(test.body)); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := readOpenSandboxMemoryArchive(bytes.NewReader(body.Bytes())); err == nil || !IsPermanent(err) {
				t.Fatalf("error = %v, want permanent rejection", err)
			}
		})
	}
}
