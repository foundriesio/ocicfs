// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/devtest"
	"github.com/foundriesio/ocicfs/internal/imagestore"
	"github.com/foundriesio/ocicfs/internal/ocilayout"
	"github.com/foundriesio/ocicfs/internal/octest"
	"github.com/foundriesio/ocicfs/internal/refstore"
	"github.com/foundriesio/ocicfs/pkg/ocicfs"
)

func TestCommandsCoverM1Verbs(t *testing.T) {
	root, _ := newRootCommand(io.Discard, io.Discard)
	want := []string{"copy", "gc", "hold", "inspect", "ls", "mount", "publish", "pull", "release", "remove", "seal", "unmount", "version"}

	// cobra adds its own "help" and "completion" commands; compare only ocicfs's.
	builtins := map[string]bool{"help": true, "completion": true}
	var got []string
	for _, c := range root.Commands() {
		if builtins[c.Name()] {
			continue
		}
		got = append(got, c.Name())
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("command set = %v, want %v", got, want)
	}
}

func TestRunVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"--version"}, &out, &errOut); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "ocicfs") {
		t.Errorf("version output = %q, want it to mention ocicfs", out.String())
	}
}

func TestRunNoArgs(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), nil, &out, &errOut); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRunUnknownSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"bogus"}, &out, &errOut)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "unknown command") {
		t.Errorf("stderr = %q, want it to mention unknown command", errOut.String())
	}
}

func TestRunSubcommandNotImplemented(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"--root", t.TempDir(), "publish", "dir", "example.com/team/app:latest"}, &out, &errOut)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "not implemented") {
		t.Errorf("stderr = %q, want it to mention not implemented", errOut.String())
	}
}

func TestRunMountRWReportsUnsupported(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{
		"--root", t.TempDir(),
		"mount", "--rw", "app@sha256:" + strings.Repeat("a", 64), t.TempDir(),
	}, &out, &errOut)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "writable mounts are not supported") {
		t.Errorf("stderr = %q, want writable unsupported", errOut.String())
	}
}

func TestRunMountRWAndMetadataOnlyIsUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{
		"--root", t.TempDir(),
		"mount", "--rw", "--metadata-only", "app@sha256:" + strings.Repeat("a", 64), t.TempDir(),
	}, &out, &errOut)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "mutually exclusive") {
		t.Errorf("stderr = %q, want mutual-exclusion error", errOut.String())
	}
}

func TestRunUnmountDispatchesToEngine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut bytes.Buffer
	code := run(ctx, []string{"--root", t.TempDir(), "unmount", "/mnt/ocicfs-test"}, &out, &errOut)
	if code != 130 {
		t.Errorf("exit code = %d, want 130", code)
	}
	if !strings.Contains(errOut.String(), "cancelled") {
		t.Errorf("stderr = %q, want cancellation", errOut.String())
	}
}

func TestRunSubcommandUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	// pull requires exactly one ref argument.
	code := run(context.Background(), []string{"pull"}, &out, &errOut)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "Usage") {
		t.Errorf("stderr = %q, want it to mention usage", errOut.String())
	}
}

// TestRunFormatJSONToStdout verifies the machine-readable output contract: a
// --format json error result is emitted as JSON on stdout while diagnostics
// stay on stderr. The engine stub fails, so success output is exercised in
// engine-level tests; here we assert the stream separation holds for the flag
// surface and that an invalid format is a usage error.
func TestRunInvalidFormatIsUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"ls", "--format", "yaml"}, &out, &errOut)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty on a usage error", out.String())
	}
	if !strings.Contains(errOut.String(), "invalid format") {
		t.Errorf("stderr = %q, want it to mention invalid format", errOut.String())
	}
}

func TestRunLsJSON(t *testing.T) {
	root := t.TempDir()
	imageRoot, ref := writeCLIImage(t, root, "app@%s", ocispec.Platform{OS: "linux", Architecture: "amd64"})

	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"--root", root, "ls", "--format", "json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, errOut.String())
	}
	var got []struct {
		Ref    string `json:"ref"`
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v; out=%q", err, out.String())
	}
	if len(got) != 1 || got[0].Ref != ref || got[0].Digest != imageRoot.String() || got[0].Size != 4096 {
		t.Fatalf("ls JSON = %#v", got)
	}
}

func TestRunRemoveDispatchesToEngine(t *testing.T) {
	root := t.TempDir()
	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	imageRoot, ref := writeCLIImage(t, root, "registry.example.com/app@%s", platform)

	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"--root", root, "remove", "--force", ref}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
	refStore, err := refstore.New(root)
	if err != nil {
		t.Fatalf("refstore.New: %v", err)
	}
	if _, err := refStore.Get(ref); !errors.Is(err, refstore.ErrNotFound) {
		t.Fatalf("Get ref error = %v, want ErrNotFound", err)
	}
	imgStore, err := imagestore.New(root)
	if err != nil {
		t.Fatalf("imagestore.New: %v", err)
	}
	if _, err := imgStore.Get(imageRoot, platform); !errors.Is(err, imagestore.ErrNotFound) {
		t.Fatalf("Get binding error = %v, want ErrNotFound", err)
	}
}

func TestRunPullNegativeIngestWorkersFails(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{
		"--root", t.TempDir(),
		"pull", "--ingest-workers", "-1", "app@sha256:" + strings.Repeat("a", 64),
	}, &out, &errOut)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "ingest workers must not be negative") {
		t.Errorf("stderr = %q, want the engine's negative-workers error", errOut.String())
	}
}

func TestRunPullHelpListsIngestWorkers(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"pull", "--help"}, &out, &errOut); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "--ingest-workers") || !strings.Contains(out.String(), "(default 2)") {
		t.Errorf("pull --help = %q, want --ingest-workers with default 2", out.String())
	}
}

// TestRunPullFromDirE2E drives a real `ocicfs pull --from-dir` through the command
// front end: it exercises flag binding, engine construction from --root, and the
// --format json output contract end to end, against a local OCI layout. It is
// verity-gated because the pull seals content with fs-verity.
func TestRunPullFromDirE2E(t *testing.T) {
	storeRoot, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable store root unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("VerityRoot: %v", err)
	}
	t.Cleanup(cleanup)
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}

	platform := ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), platform,
		octest.File{Name: "etc/config.txt", Mode: 0o644, Body: "hello from ocicfs\n"},
	)
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}

	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{
		"--root", storeRoot,
		"pull", "--from-dir", lay.Dir, "--format", "json", lay.Ref,
	}, &out, &errOut)
	if code != 0 {
		if strings.Contains(errOut.String(), "fs-verity") {
			t.Skipf("fs-verity unsupported on the test filesystem: %s", errOut.String())
		}
		t.Fatalf("exit code = %d, stderr = %q", code, errOut.String())
	}

	var got struct {
		Ref    string `json:"ref"`
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v; out=%q", err, out.String())
	}
	if got.Ref != lay.Ref {
		t.Errorf("ref = %q, want %q", got.Ref, lay.Ref)
	}
	if got.Digest != lay.Root.String() {
		t.Errorf("digest = %q, want %s", got.Digest, lay.Root)
	}
	if got.Size <= 0 {
		t.Errorf("size = %d, want > 0", got.Size)
	}
	if _, err := os.Stat(octest.BindingPath(storeRoot, lay.Root, lay.Platform)); err != nil {
		t.Errorf("binding not written by CLI pull: %v", err)
	}
}

// noChildren lists no manifests for any archive entry.
func noChildren(ocispec.Descriptor) ([]digest.Digest, error) { return nil, nil }

// TestArchiveRefPairedIndex covers an archive that lists an index beside one
// of its own manifests, as ocicfs copy writes an image: it holds one image,
// the index, while an index beside a manifest it does not list is two.
func TestArchiveRefPairedIndex(t *testing.T) {
	idx := digest.SHA256.FromString("index")
	child := digest.SHA256.FromString("child")
	other := digest.SHA256.FromString("other")
	children := func(d ocispec.Descriptor) ([]digest.Digest, error) {
		if d.Digest == idx {
			return []digest.Digest{child, digest.SHA256.FromString("absent")}, nil
		}
		return nil, nil
	}
	index := func(entries ...ocispec.Descriptor) []byte {
		b, err := json.Marshal(ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: entries})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	named := func(d digest.Digest, name string) ocispec.Descriptor {
		return ocispec.Descriptor{Digest: d, Annotations: map[string]string{"io.containerd.image.name": name}}
	}

	tests := []struct {
		name  string
		index []byte
		ref   string
		want  string
	}{
		{"no ref, index first", index(named(idx, "ghcr.io/team/app:1"), named(child, "ghcr.io/team/app@"+child.String())), "", "ghcr.io/team/app@" + idx.String()},
		{"no ref, index second", index(named(child, "ghcr.io/team/app@"+child.String()), named(idx, "ghcr.io/team/app:1")), "", "ghcr.io/team/app@" + idx.String()},
		{"bare name", index(named(child, "ghcr.io/team/app@"+child.String()), named(idx, "ghcr.io/team/app:1")), "app", "app@" + idx.String()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := archiveRef(tt.index, tt.ref, children)
			if err != nil {
				t.Fatalf("archiveRef: %v", err)
			}
			if got != tt.want {
				t.Fatalf("archiveRef = %q, want %q", got, tt.want)
			}
		})
	}

	_, err := archiveRef(index(named(idx, "ghcr.io/team/app:1"), named(other, "ghcr.io/team/other:1")), "", children)
	if err == nil || !strings.Contains(err.Error(), "2 images") {
		t.Fatalf("an index beside a manifest it does not list: err = %v, want the two images listed", err)
	}
	failing := func(ocispec.Descriptor) ([]digest.Digest, error) { return nil, errors.New("unreadable") }
	if _, err := archiveRef(index(named(idx, "a:1"), named(child, "b:1")), "", failing); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("an unreadable entry: err = %v, want it reported", err)
	}
}

func TestArchiveRef(t *testing.T) {
	one := digest.SHA256.FromString("one")
	two := digest.SHA256.FromString("two")
	index := func(entries ...ocispec.Descriptor) []byte {
		b, err := json.Marshal(ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: entries})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	named := func(d digest.Digest, name string) ocispec.Descriptor {
		return ocispec.Descriptor{Digest: d, Annotations: map[string]string{"io.containerd.image.name": name}}
	}
	refName := func(d digest.Digest, tag string) ocispec.Descriptor {
		return ocispec.Descriptor{Digest: d, Annotations: map[string]string{"org.opencontainers.image.ref.name": tag}}
	}

	tests := []struct {
		name    string
		index   []byte
		ref     string
		want    string
		wantErr []string // substrings of the expected error; empty means success
	}{
		{"bare name, one manifest", index(ocispec.Descriptor{Digest: one}), "app", "app@" + one.String(), nil},
		{"bare name, two manifests", index(named(one, "docker.io/library/a:1"), named(two, "docker.io/library/b:2")), "app", "", []string{one.String(), two.String(), "docker.io/library/a:1", "docker.io/library/b:2", "<name>@<digest>"}},
		{"bare name, no manifests", index(), "app", "", []string{"no manifests"}},
		{"bare name, not an index", []byte("nonsense"), "app", "", []string{"index.json"}},
		{"bare name, malformed entry digest", index(ocispec.Descriptor{Digest: "sha256:nope"}), "app", "", []string{"index.json entry"}},
		{"bare name ignores a conflicting annotation", index(named(one, "ghcr.io/other/image:2")), "app", "app@" + one.String(), nil},
		{"pinned ref passes through", nil, "app@" + one.String(), "app@" + one.String(), nil},
		{"pinned ref ignores the annotation", index(named(two, "ghcr.io/other/image:2")), "app@" + one.String(), "app@" + one.String(), nil},
		{"bare digest passes through", nil, one.String(), one.String(), nil},
		{"no ref: tagged name, tag dropped", index(named(one, "ghcr.io/team/app:1.2.3")), "", "ghcr.io/team/app@" + one.String(), nil},
		{"no ref: short name normalized as the store records it", index(named(one, "app:1.2.3")), "", "index.docker.io/library/app@" + one.String(), nil},
		{"no ref: tag and digest both dropped", index(named(one, "ghcr.io/team/app:1.2.3@"+two.String())), "", "ghcr.io/team/app@" + one.String(), nil},
		{"no ref: annotation digest equals the entry", index(named(one, "ghcr.io/team/app@"+one.String())), "", "ghcr.io/team/app@" + one.String(), nil},
		{"no ref: annotation digest differs from the entry", index(named(one, "ghcr.io/team/app@"+two.String())), "", "ghcr.io/team/app@" + one.String(), nil},
		{"no ref: no name annotation", index(ocispec.Descriptor{Digest: one}), "", "", []string{"does not name its image", "pass a reference"}},
		{"no ref: empty name annotation", index(named(one, "")), "", "", []string{"does not name its image", "pass a reference"}},
		{"no ref: malformed name annotation", index(named(one, "Not/A Reference")), "", "", []string{"Not/A Reference", "pass a reference"}},
		{"no ref: a bare tag in ref.name", index(refName(one, "1.2.3")), "", "", []string{"does not name its image", "pass a reference"}},
		{"no ref: a short name in ref.name", index(refName(one, "team/app:1.2.3")), "", "", []string{"does not name its image", "pass a reference"}},
		{"no ref: a fully qualified ref.name, tag dropped", index(refName(one, "ghcr.io/team/app:1.2.3")), "", "ghcr.io/team/app@" + one.String(), nil},
		{"no ref: a ref.name on a registry with a port", index(refName(one, "127.0.0.1:5000/team/app:1.2.3")), "", "127.0.0.1:5000/team/app@" + one.String(), nil},
		{"no ref: a ref.name digest replaced by the entry's", index(refName(one, "ghcr.io/team/app@"+two.String())), "", "ghcr.io/team/app@" + one.String(), nil},
		{"no ref: the containerd name wins over ref.name", index(ocispec.Descriptor{Digest: one, Annotations: map[string]string{"io.containerd.image.name": "ghcr.io/team/app:1", "org.opencontainers.image.ref.name": "ghcr.io/other/image:2"}}), "", "ghcr.io/team/app@" + one.String(), nil},
		{"no ref: two manifests", index(named(one, "docker.io/library/a:1"), named(two, "docker.io/library/b:2")), "", "", []string{"<name>@<digest>"}},
		{"no ref: two identical manifests", index(named(one, "docker.io/library/a:1"), named(one, "docker.io/library/a:1")), "", "", []string{"2 images", "<name>@<digest>"}},
		{"no ref: no manifests", index(), "", "", []string{"no manifests"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := archiveRef(tt.index, tt.ref, noChildren)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("archiveRef: %v", err)
				}
				if got != tt.want {
					t.Fatalf("archiveRef = %q, want %q", got, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("archiveRef = %q, want an error", got)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestRunPullArgumentCount(t *testing.T) {
	pinned := "app@sha256:" + strings.Repeat("a", 64)
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"registry pull without a ref", []string{"pull"}, "a reference is required"},
		{"from-dir without a ref", []string{"pull", "--from-dir", t.TempDir()}, "a reference is required"},
		{"two refs", []string{"pull", pinned, pinned}, "at most 1 argument"},
		{"from-tar with two refs", []string{"pull", "--from-tar", filepath.Join(t.TempDir(), "image.tar"), pinned, pinned}, "at most 1 argument"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := run(context.Background(), append([]string{"--root", t.TempDir()}, tt.args...), &out, &errOut)
			if code != 2 {
				t.Errorf("exit code = %d, want 2; stderr = %q", code, errOut.String())
			}
			if !strings.Contains(errOut.String(), tt.want) {
				t.Errorf("stderr = %q, want %q", errOut.String(), tt.want)
			}
		})
	}
}

// TestRunPullFromTarE2E drives a real `ocicfs pull --from-tar` against a layout
// tarred the way docker save writes one, with index.json after the blobs: a
// pinned reference, a bare name pinned from the archive's index, and no
// reference at all, taken from the name annotation docker save writes and
// recorded as the engine normalizes it. It is verity-gated because the pull
// seals content with fs-verity.
func TestRunPullFromTarE2E(t *testing.T) {
	storeRoot, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable store root unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("VerityRoot: %v", err)
	}
	t.Cleanup(cleanup)
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}

	platform := ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), platform,
		octest.File{Name: "etc/config.txt", Mode: 0o644, Body: "hello from a docker save archive\n"},
	)
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}
	archive := tarLayout(t, lay.Dir, nil)
	// The archive as docker save writes it for `app:1.0`: the index entry
	// carries the reference it was given.
	index, err := os.ReadFile(filepath.Join(lay.Dir, "index.json")) // #nosec G304 -- layout written by this test
	if err != nil {
		t.Fatal(err)
	}
	var idx ocispec.Index
	if err := json.Unmarshal(index, &idx); err != nil {
		t.Fatal(err)
	}
	idx.Manifests[0].Annotations = map[string]string{"io.containerd.image.name": "app:1.0"}
	annotated, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	named := tarLayout(t, lay.Dir, annotated)
	inferredRef := "index.docker.io/library/app@" + lay.Root.String()

	for _, c := range []struct {
		args []string
		ref  string
	}{
		{[]string{"pull", "--from-tar", archive, "--format", "json", lay.Ref}, lay.Ref},
		{[]string{"pull", "--from-tar", archive, "--format", "json", "app"}, lay.Ref},
		{[]string{"pull", "--from-tar", named, "--format", "json"}, inferredRef},
		{[]string{"pull", "--from-tar", named, "--format", "json", lay.Ref}, lay.Ref},
	} {
		var out, errOut bytes.Buffer
		code := run(context.Background(), append([]string{"--root", storeRoot}, c.args...), &out, &errOut)
		if code != 0 {
			if strings.Contains(errOut.String(), "fs-verity") {
				t.Skipf("fs-verity unsupported on the test filesystem: %s", errOut.String())
			}
			t.Fatalf("%v: exit code = %d, stderr = %q", c.args, code, errOut.String())
		}
		var got struct {
			Ref    string `json:"ref"`
			Digest string `json:"digest"`
		}
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("%v: stdout is not JSON: %v; out=%q", c.args, err, out.String())
		}
		if got.Ref != c.ref {
			t.Errorf("%v: ref = %q, want %q", c.args, got.Ref, c.ref)
		}
		if got.Digest != lay.Root.String() {
			t.Errorf("%v: digest = %q, want %s", c.args, got.Digest, lay.Root)
		}
	}
	if _, err := os.Stat(octest.BindingPath(storeRoot, lay.Root, lay.Platform)); err != nil {
		t.Errorf("binding not written by CLI pull: %v", err)
	}

	// The inferred reference is spelled as the store records it, in the text
	// output of the pull as in the listing.
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"--root", storeRoot, "pull", "--from-tar", named}, &out, &errOut); code != 0 {
		t.Fatalf("text pull: exit code = %d, stderr = %q", code, errOut.String())
	}
	if !strings.HasPrefix(out.String(), inferredRef+"\t") {
		t.Errorf("text pull output = %q, want it to start with %s", out.String(), inferredRef)
	}
	out.Reset()
	if code := run(context.Background(), []string{"--root", storeRoot, "ls"}, &out, &errOut); code != 0 {
		t.Fatalf("ls: exit code = %d, stderr = %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), inferredRef) {
		t.Errorf("ls = %q, want the inferred reference recorded as %s", out.String(), inferredRef)
	}
}

// TestRunPullFromTarDeltaE2E drives `ocicfs pull --from-tar --delta` of a
// zstd:chunked release into a store holding the previous release, so the
// pull must read each layer's table of contents and the one file the release
// adds as byte ranges of the archive: the per-layer diagnostics on stderr
// must report no fallback to a whole-layer read, and the added file's object
// must appear in the store. The fixtures are linux/amd64 images.
func TestRunPullFromTarDeltaE2E(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skipf("the zstd:chunked fixtures are linux/amd64 images; host is %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	storeRoot, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable store root unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("VerityRoot: %v", err)
	}
	t.Cleanup(cleanup)
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}

	fixtures := filepath.Join("..", "..", "internal", "zstdchunked", "testdata")
	base := filepath.Join(fixtures, "release-n")
	next := filepath.Join(fixtures, "release-n1")
	pin := func(dir string) string {
		index, err := os.ReadFile(filepath.Join(dir, "index.json")) // #nosec G304 -- test fixture path
		if err != nil {
			t.Fatalf("read index: %v", err)
		}
		ref, err := archiveRef(index, "release", noChildren)
		if err != nil {
			t.Fatalf("pin the fixture %s: %v", dir, err)
		}
		return ref
	}
	nextRef := pin(next)

	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"--root", storeRoot, "pull", "--from-dir", base, pin(base)}, &out, &errOut)
	if code != 0 {
		if strings.Contains(errOut.String(), "fs-verity") {
			t.Skipf("fs-verity unsupported on the test filesystem: %s", errOut.String())
		}
		t.Fatalf("pull of the base release: exit code = %d, stderr = %q", code, errOut.String())
	}
	// The file release-n1 adds, large enough to be stored as an object.
	added := digest.SHA256.FromBytes([]byte("this file is bigger than sixty-four bytes so it is stored as an object.\nnew\n"))
	if _, err := os.Stat(octest.ObjectPath(storeRoot, added)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the base release stored the added file's object: %v", err)
	}

	out.Reset()
	errOut.Reset()
	code = run(context.Background(), []string{
		"--root", storeRoot, "pull", "--from-tar", tarLayout(t, next, nil), "--delta", "--format", "json", "release",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("delta pull: exit code = %d, stderr = %q", code, errOut.String())
	}
	if _, err := os.Stat(octest.ObjectPath(storeRoot, added)); err != nil {
		t.Errorf("the delta pull did not store the added file's object: %v", err)
	}
	var layers, ranged int
	for _, line := range strings.Split(errOut.String(), "\n") {
		if !strings.Contains(line, `msg="delta: layer"`) {
			continue
		}
		layers++
		if !strings.Contains(line, "fallback=none") {
			t.Errorf("layer fell back from byte ranges: %s", line)
		}
		if logField(t, line, "requests") > 0 {
			ranged++
			if logField(t, line, "fetched") >= logField(t, line, "size") {
				t.Errorf("layer fetched whole rather than by ranges: %s", line)
			}
		}
	}
	if layers != 2 || ranged == 0 {
		t.Errorf("%d layers logged, %d of them fetched by ranges, want 2 and at least 1:\n%s", layers, ranged, errOut.String())
	}
	var got struct {
		Ref    string `json:"ref"`
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v; out=%q", err, out.String())
	}
	if got.Ref != nextRef {
		t.Errorf("ref = %q, want %q", got.Ref, nextRef)
	}
	if "release@"+got.Digest != nextRef {
		t.Errorf("digest = %q, want the pinned %s", got.Digest, nextRef)
	}
}

// logField returns the integer value of key in one slog text line.
func logField(t *testing.T, line, key string) int64 {
	t.Helper()
	for _, field := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(field, key+"="); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				t.Fatalf("field %s of %q: %v", key, line, err)
			}
			return n
		}
	}
	t.Fatalf("field %s missing from %q", key, line)
	return 0
}

// TestRunPullFromTarMultiArchE2E drives `ocicfs pull --from-tar` against the
// archive docker save writes for a multi-platform image without --platform:
// index.json's one entry names the image index verbatim, which lists a child
// per platform, while the archive holds only the children the daemon had —
// here the host's. The image is keyed by the host child whichever of the
// index or that child is pinned; pinning the absent child fails naming it.
func TestRunPullFromTarMultiArchE2E(t *testing.T) {
	storeRoot, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable store root unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("VerityRoot: %v", err)
	}
	t.Cleanup(cleanup)
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}

	hostPlatform := ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	foreignPlatform := ocispec.Platform{OS: "linux", Architecture: "arm64"}
	if runtime.GOARCH == "arm64" {
		foreignPlatform.Architecture = "amd64"
	}
	host := octest.BuildIndexImage(t, hostPlatform, "host")
	foreign := octest.BuildIndexImage(t, foreignPlatform, "foreign")

	// The layout: the index blob, the host child's blobs, none of the
	// foreign child's, and an index.json naming the index as docker save
	// names the image it was given.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	hostBlobs, err := filepath.Glob(filepath.Join(host.Dir, "blobs", "sha256", "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range hostBlobs {
		data, err := os.ReadFile(p) // #nosec G304 -- layout written by this test
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", filepath.Base(p)), data, 0o644); err != nil { // #nosec G703 -- temp layout of this test
			t.Fatal(err)
		}
	}
	child := func(lay octest.Layout, p ocispec.Platform) ocispec.Descriptor {
		return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: lay.Root, Size: lay.RootSize, Platform: &p}
	}
	indexBlob, err := json.Marshal(ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{child(host, hostPlatform), child(foreign, foreignPlatform)},
	})
	if err != nil {
		t.Fatal(err)
	}
	indexDigest := digest.SHA256.FromBytes(indexBlob)
	if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", indexDigest.Hex()), indexBlob, 0o644); err != nil {
		t.Fatal(err)
	}
	layoutIndex, err := json.Marshal(ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{{
			MediaType:   ocispec.MediaTypeImageIndex,
			Digest:      indexDigest,
			Size:        int64(len(indexBlob)),
			Annotations: map[string]string{"io.containerd.image.name": "app:1.0"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	archive := tarLayout(t, dir, layoutIndex)

	for _, c := range []struct {
		name string
		args []string
		ref  string
	}{
		{"no ref", nil, "index.docker.io/library/app@" + indexDigest.String()},
		{"index pinned", []string{"app@" + indexDigest.String()}, "app@" + indexDigest.String()},
		{"host child pinned", []string{"app@" + host.Root.String()}, "app@" + host.Root.String()},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			args := append([]string{"--root", storeRoot, "pull", "--from-tar", archive, "--format", "json"}, c.args...)
			code := run(context.Background(), args, &out, &errOut)
			if code != 0 {
				if strings.Contains(errOut.String(), "fs-verity") {
					t.Skipf("fs-verity unsupported on the test filesystem: %s", errOut.String())
				}
				t.Fatalf("exit code = %d, stderr = %q", code, errOut.String())
			}
			var got struct {
				Ref    string `json:"ref"`
				Digest string `json:"digest"`
			}
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("stdout is not JSON: %v; out=%q", err, out.String())
			}
			if got.Ref != c.ref {
				t.Errorf("ref = %q, want %q", got.Ref, c.ref)
			}
			if got.Digest != host.Root.String() {
				t.Errorf("digest = %q, want the host child %s", got.Digest, host.Root)
			}

			out.Reset()
			if code := run(context.Background(), []string{"--root", storeRoot, "inspect", "--format", "json", c.ref}, &out, &errOut); code != 0 {
				t.Fatalf("inspect: exit code = %d, stderr = %q", code, errOut.String())
			}
			var inspected struct {
				Root string `json:"root"`
			}
			if err := json.Unmarshal(out.Bytes(), &inspected); err != nil {
				t.Fatalf("inspect stdout is not JSON: %v; out=%q", err, out.String())
			}
			if inspected.Root != host.Root.String() {
				t.Errorf("inspect root = %q, want the host child %s", inspected.Root, host.Root)
			}
			out.Reset()
			if code := run(context.Background(), []string{"--root", storeRoot, "ls"}, &out, &errOut); code != 0 {
				t.Fatalf("ls: exit code = %d, stderr = %q", code, errOut.String())
			}
			if !strings.Contains(out.String(), c.ref) {
				t.Errorf("ls = %q, want %s listed", out.String(), c.ref)
			}
		})
	}

	t.Run("foreign child pinned", func(t *testing.T) {
		var out, errOut bytes.Buffer
		code := run(context.Background(), []string{"--root", storeRoot, "pull", "--from-tar", archive, "app@" + foreign.Root.String()}, &out, &errOut)
		if code == 0 {
			t.Fatalf("pull of a child the archive does not hold succeeded: %q", out.String())
		}
		if !strings.Contains(errOut.String(), foreign.Root.String()) || !strings.Contains(errOut.String(), "not present") {
			t.Errorf("stderr = %q, want the absent child %s named as not present", errOut.String(), foreign.Root)
		}
	})
}

func TestRunPullFromTarRejectsFromDir(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{
		"--root", t.TempDir(),
		"pull", "--from-dir", t.TempDir(), "--from-tar", filepath.Join(t.TempDir(), "image.tar"),
		"app@sha256:" + strings.Repeat("a", 64),
	}, &out, &errOut)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "--from-dir and --from-tar are mutually exclusive") {
		t.Errorf("stderr = %q, want mutual-exclusion error", errOut.String())
	}
}

// tarLayout writes the OCI image layout under dir as a tar in docker save
// order, with every blob before index.json, and returns the archive's path.
// A non-nil index replaces the layout's index.json in the archive.
func tarLayout(t *testing.T, dir string, index []byte) string {
	t.Helper()
	archive := filepath.Join(t.TempDir(), "image.tar")
	f, err := os.Create(archive) // #nosec G304 -- temp path of this test
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	addBytes := func(name string, body []byte) {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	add := func(name string) {
		body, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- layout written by this test
		if err != nil {
			t.Fatal(err)
		}
		addBytes(name, body)
	}
	add("oci-layout")
	err = filepath.WalkDir(filepath.Join(dir, "blobs"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			add(rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk blobs: %v", err)
	}
	if index != nil {
		addBytes("index.json", index)
	} else {
		add("index.json")
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return archive
}

// jsonParses is a small guard that the JSON encoder configuration stays valid;
// it documents the intended shape of list output.
func TestImageListJSONShape(t *testing.T) {
	const sample = `[{"ref":"foo","digest":"sha256:abc","size":10}]`
	var got []map[string]any
	if err := json.Unmarshal([]byte(sample), &got); err != nil {
		t.Fatalf("sample list output does not parse as JSON: %v", err)
	}
	if len(got) != 1 || got[0]["ref"] != "foo" {
		t.Errorf("unexpected decoded shape: %v", got)
	}
}

// writeCLIImage seeds the state the CLI verbs treat as a pulled image for
// platform p: a real image manifest and its config stored in the object store
// under their own digests -- the manifest is the authoritative presence signal
// the hold verbs read, and every stored blob hashes to the path it is filed
// under, as a real pull guarantees -- plus a ref record and a binding. refFmt
// is a %s template the manifest digest is substituted into; the manifest digest
// and the resulting ref are returned.
func writeCLIImage(t *testing.T, root, refFmt string, p ocispec.Platform) (digest.Digest, string) {
	t.Helper()
	configJSON, err := json.Marshal(ocispec.Image{Platform: p, RootFS: ocispec.RootFS{Type: "layers"}})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	configDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    digest.SHA256.FromBytes(configJSON),
		Size:      int64(len(configJSON)),
	}
	manifestJSON, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.SHA256.FromBytes(manifestJSON),
		Size:      int64(len(manifestJSON)),
	}
	writeCLIObject(t, root, configDesc.Digest, configJSON)
	writeCLIObject(t, root, manifestDesc.Digest, manifestJSON)

	ref := strings.Replace(refFmt, "%s", manifestDesc.Digest.String(), 1)
	refStore, err := refstore.New(root)
	if err != nil {
		t.Fatalf("refstore.New: %v", err)
	}
	if err := refStore.Put(ref, manifestDesc, manifestDesc, time.Unix(0, 0).UTC(), []string{ocilayout.PlatformString(&p)}); err != nil {
		t.Fatalf("Put ref: %v", err)
	}
	imgStore, err := imagestore.New(root)
	if err != nil {
		t.Fatalf("imagestore.New: %v", err)
	}
	if err := imgStore.Put(manifestDesc.Digest, p, imagestore.Binding{
		Descriptor:     ocispec.Descriptor{Digest: digest.SHA256.FromString("composefs"), Size: 4096},
		FsverityDigest: digest.SHA256.FromString("verity").String(),
	}); err != nil {
		t.Fatalf("Put binding: %v", err)
	}
	return manifestDesc.Digest, ref
}

// writeCLIObject stores data at the content-addressed path digest d names.
func writeCLIObject(t *testing.T, root string, d digest.Digest, data []byte) {
	t.Helper()
	if ocicfs.BuildVerityMode == "required" {
		t.Skip("seeds objects without fs-verity, which a build made in required mode refuses")
	}
	path := octest.ObjectPath(root, d)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll object: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile object: %v", err)
	}
}
