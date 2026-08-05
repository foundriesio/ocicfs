// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/foundriesio/knarr/internal/devtest"
	"github.com/foundriesio/knarr/internal/octest"
)

const gcSharedBody = "shared payload stored once in the CAS, larger than the inline threshold\n"

// gcEngine opens an engine on a verity-capable root and pulls one image
// holding a shared file and one unique file, skipping outside the privileged
// dev container.
func gcEngine(t *testing.T, unique string) (Engine, string, octest.Layout) {
	t.Helper()
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

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lay := gcPull(t, eng, unique)
	return eng, storeRoot, lay
}

func gcPull(t *testing.T, eng Engine, unique string) octest.Layout {
	t.Helper()
	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
		octest.File{Name: "etc/shared.txt", Mode: 0o644, Body: gcSharedBody},
		octest.File{Name: "etc/" + unique + ".txt", Mode: 0o644, Body: "unique payload for " + unique + ", padded far beyond the sixty-four byte inline threshold\n"},
	)
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}
	if _, err := eng.Pull(context.Background(), lay.Ref, WithFromDir(lay.Dir)); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull %s: %v", lay.Ref, err)
	}
	return lay
}

func gcUniqueDigest(unique string) digest.Digest {
	return digest.SHA256.FromString("unique payload for " + unique + ", padded far beyond the sixty-four byte inline threshold\n")
}

func assertObjectAbsent(t *testing.T, root string, d digest.Digest) {
	t.Helper()
	if _, err := os.Stat(octest.ObjectPath(root, d)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("object %s still present: %v", d, err)
	}
}

// assertNoEmptyDirs asserts gc pruned the empty shard directories deletion
// left behind across the store's fan-out trees.
func assertNoEmptyDirs(t *testing.T, storeRoot string) {
	t.Helper()
	for _, tree := range []string{
		filepath.Join(storeRoot, "objects", "sha256"),
		filepath.Join(storeRoot, "objects", "fsverity-sha256-12"),
		filepath.Join(storeRoot, "images", "sha256"),
		filepath.Join(storeRoot, "refs", "sha256"),
		filepath.Join(storeRoot, "holds", "sha256"),
	} {
		err := filepath.WalkDir(tree, func(path string, de fs.DirEntry, walkErr error) error {
			if walkErr != nil || path == tree || !de.IsDir() {
				return walkErr
			}
			entries, err := os.ReadDir(path)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				t.Errorf("empty directory %s survived gc", path)
			}
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("walk %s: %v", tree, err)
		}
	}
}

// TestGCReclaimsUnwantedKeepsShared is the core sweep contract: after one of
// two overlapping images is removed, gc reclaims its unshared objects and
// keeps everything the surviving image still references, and a prior dry run
// reports the same counts while deleting nothing.
func TestGCReclaimsUnwantedKeepsShared(t *testing.T) {
	eng, storeRoot, layA := gcEngine(t, "keep")
	layB := gcPull(t, eng, "drop")
	ctx := context.Background()

	bBinding := readPullBinding(t, storeRoot, layB.Root, layB.Platform)
	bImage := bBinding.RootFS.Descriptor.Digest
	if err := eng.Remove(ctx, layB.Ref); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	dry, err := eng.GC(ctx, WithDryRun())
	if err != nil {
		t.Fatalf("GC dry run: %v", err)
	}
	if dry.ObjectsRemoved == 0 || dry.BytesFreed == 0 {
		t.Fatalf("dry run = %+v, want removals reported", dry)
	}
	assertObjectExists(t, storeRoot, layB.Manifest)
	assertObjectExists(t, storeRoot, gcUniqueDigest("drop"))

	report, err := eng.GC(ctx)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if report.ObjectsRemoved != dry.ObjectsRemoved || report.BytesFreed != dry.BytesFreed ||
		report.BindingsRemoved != dry.BindingsRemoved {
		t.Fatalf("GC = %+v, dry run promised %+v", report, dry)
	}
	if report.InUseSkipped != 0 {
		t.Fatalf("GC = %+v, want no in-use skips", report)
	}
	// The listing names every deleted store object; orphan view entries, the
	// only ObjectsRemoved contributors it omits, need external damage to
	// exist, so here the two agree exactly.
	if len(report.Removed) != report.ObjectsRemoved {
		t.Fatalf("GC listed %d removed digests, counted %d", len(report.Removed), report.ObjectsRemoved)
	}
	for _, d := range report.Removed {
		if digest.Digest(d).Validate() != nil {
			t.Fatalf("GC listed malformed digest %q", d)
		}
	}

	// The removed image's own chain and objects are gone...
	assertObjectAbsent(t, storeRoot, layB.Manifest)
	assertObjectAbsent(t, storeRoot, layB.Config)
	assertObjectAbsent(t, storeRoot, bImage)
	assertObjectAbsent(t, storeRoot, gcUniqueDigest("drop"))
	// ...while everything the survivor references stays, including the shared
	// file object.
	assertObjectExists(t, storeRoot, layA.Manifest)
	assertObjectExists(t, storeRoot, digest.SHA256.FromString(gcSharedBody))
	assertObjectExists(t, storeRoot, gcUniqueDigest("keep"))
	assertNoEmptyDirs(t, storeRoot)

	target := t.TempDir()
	if err := eng.Mount(ctx, layA.Ref, target); err != nil {
		t.Fatalf("Mount survivor: %v", err)
	}
	t.Cleanup(func() {
		if err := eng.Unmount(context.Background(), target); err != nil && !errors.Is(err, ErrNotMounted) {
			t.Errorf("Unmount: %v", err)
		}
	})
	data, err := os.ReadFile(filepath.Join(target, "etc", "shared.txt")) // #nosec G304 -- mount target under t-managed dir
	if err != nil || string(data) != gcSharedBody {
		t.Fatalf("read through survivor mount = %q, %v", data, err)
	}
}

// TestGCInUseTransitiveProtection pins the decision record's transitive rule:
// an in-use-but-unwanted image survives via its mount lock, and the data
// objects only it references survive via the redirect walk of that locked
// image — then fall together once unmounted.
func TestGCInUseTransitiveProtection(t *testing.T) {
	eng, storeRoot, lay := gcEngine(t, "inuse")
	ctx := context.Background()
	image := readPullBinding(t, storeRoot, lay.Root, lay.Platform).RootFS.Descriptor.Digest

	target := filepath.Join(t.TempDir(), "meta")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := eng.Mount(ctx, lay.Ref, target, WithMetadataOnly()); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	mounted := true
	t.Cleanup(func() {
		if !mounted {
			return
		}
		if err := eng.Unmount(context.Background(), target); err != nil {
			t.Errorf("Unmount: %v", err)
		}
	})
	if err := eng.Remove(ctx, lay.Ref); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	report, err := eng.GC(ctx)
	if err != nil {
		t.Fatalf("GC while mounted: %v", err)
	}
	if report.InUseSkipped == 0 {
		t.Fatalf("GC = %+v, want the mounted image skipped as in use", report)
	}
	assertObjectExists(t, storeRoot, image)
	assertObjectExists(t, storeRoot, digest.SHA256.FromString(gcSharedBody))
	assertObjectExists(t, storeRoot, gcUniqueDigest("inuse"))

	if err := eng.Unmount(ctx, target); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	mounted = false

	report, err = eng.GC(ctx)
	if err != nil {
		t.Fatalf("GC after unmount: %v", err)
	}
	if report.InUseSkipped != 0 || report.ObjectsRemoved == 0 {
		t.Fatalf("GC after unmount = %+v, want the image and its objects reclaimed", report)
	}
	assertObjectAbsent(t, storeRoot, image)
	assertObjectAbsent(t, storeRoot, digest.SHA256.FromString(gcSharedBody))
	assertObjectAbsent(t, storeRoot, gcUniqueDigest("inuse"))
	assertNoEmptyDirs(t, storeRoot)
}

// TestGCHoldProtectsWithoutRef pins hold as a first-class wanted root: an
// image whose only anchor is a hold survives in full, and falls once
// released.
func TestGCHoldProtectsWithoutRef(t *testing.T) {
	eng, storeRoot, lay := gcEngine(t, "held")
	ctx := context.Background()
	image := readPullBinding(t, storeRoot, lay.Root, lay.Platform).RootFS.Descriptor.Digest

	if err := eng.Hold(ctx, lay.Root.String(), "keeper"); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	// Drop the ref record only, leaving the binding: the hold alone wants the
	// image now.
	if err := os.Remove(octest.RefPath(storeRoot, mustNormalizeRef(t, lay.Ref))); err != nil {
		t.Fatalf("remove ref record: %v", err)
	}

	report, err := eng.GC(ctx)
	if err != nil {
		t.Fatalf("GC while held: %v", err)
	}
	if report.BindingsRemoved != 0 {
		t.Fatalf("GC = %+v, want the held image's binding kept", report)
	}
	assertObjectExists(t, storeRoot, image)
	assertObjectExists(t, storeRoot, lay.Manifest)
	assertObjectExists(t, storeRoot, gcUniqueDigest("held"))

	if err := eng.Release(ctx, lay.Root.String(), "keeper"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	report, err = eng.GC(ctx)
	if err != nil {
		t.Fatalf("GC after release: %v", err)
	}
	if report.BindingsRemoved != 1 || report.ObjectsRemoved == 0 {
		t.Fatalf("GC after release = %+v, want the binding and objects reclaimed", report)
	}
	assertObjectAbsent(t, storeRoot, image)
	assertObjectAbsent(t, storeRoot, gcUniqueDigest("held"))
}

// TestGCHeldLooseObjects pins the held-root classifier: a held loose object —
// including one that happens to be JSON — is protected by its digest alone
// and must neither abort the collection nor be routed into chain resolution.
func TestGCHeldLooseObjects(t *testing.T) {
	eng, storeRoot, _ := gcEngine(t, "anchor")
	e := eng.(*engine)
	ctx := context.Background()

	loose, _, err := e.obj.WriteObject(ctx, strings.NewReader("held loose bytes, not an image"))
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}
	looseJSON, _, err := e.obj.WriteObject(ctx, strings.NewReader(`{"some":"json","that":"is not OCI metadata"}`))
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}
	for _, d := range []digest.Digest{loose, looseJSON} {
		unlock, err := e.holds.Lock(d)
		if err != nil {
			t.Fatalf("Lock: %v", err)
		}
		err = e.holds.Hold(d, "keeper")
		unlock()
		if err != nil {
			t.Fatalf("Hold: %v", err)
		}
	}

	if _, err := eng.GC(ctx); err != nil {
		t.Fatalf("GC with held loose objects: %v", err)
	}
	assertObjectExists(t, storeRoot, loose)
	assertObjectExists(t, storeRoot, looseJSON)

	for _, d := range []digest.Digest{loose, looseJSON} {
		if err := eng.Release(ctx, d.String(), "keeper"); err != nil {
			t.Fatalf("Release: %v", err)
		}
	}
	if _, err := eng.GC(ctx); err != nil {
		t.Fatalf("GC after release: %v", err)
	}
	assertObjectAbsent(t, storeRoot, loose)
	assertObjectAbsent(t, storeRoot, looseJSON)
}

// TestGCAbortsOnBrokenRoots pins the fail-safe: any wanted root gc cannot
// fully enumerate aborts the whole collection with the remedy, deleting
// nothing.
func TestGCAbortsOnBrokenRoots(t *testing.T) {
	ctx := context.Background()

	t.Run("corrupt ref record", func(t *testing.T) {
		eng, storeRoot, _ := gcEngine(t, "corruptref")
		dir := filepath.Join(storeRoot, "refs", "sha256", "aa")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, strings.Repeat("b", 62)+".json"), []byte(`{"schema":"wrong"}`), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		_, err := eng.GC(ctx)
		if err == nil || !strings.Contains(err.Error(), "invalid ref record") {
			t.Fatalf("GC = %v, want the invalid-record abort", err)
		}
		assertObjectExists(t, storeRoot, gcUniqueDigest("corruptref"))
	})

	t.Run("wanted image missing its binding", func(t *testing.T) {
		eng, storeRoot, lay := gcEngine(t, "nobinding")
		if err := os.Remove(octest.BindingPath(storeRoot, lay.Root, lay.Platform)); err != nil {
			t.Fatalf("remove binding: %v", err)
		}
		_, err := eng.GC(ctx)
		if err == nil || !strings.Contains(err.Error(), "has no binding") {
			t.Fatalf("GC = %v, want the missing-binding abort", err)
		}
		assertObjectExists(t, storeRoot, gcUniqueDigest("nobinding"))
	})

	t.Run("held image with no binding", func(t *testing.T) {
		eng, storeRoot, lay := gcEngine(t, "heldnobinding")
		if err := eng.Hold(ctx, lay.Root.String(), "keeper"); err != nil {
			t.Fatalf("Hold: %v", err)
		}
		if err := os.Remove(octest.RefPath(storeRoot, mustNormalizeRef(t, lay.Ref))); err != nil {
			t.Fatalf("remove ref record: %v", err)
		}
		if err := os.Remove(octest.BindingPath(storeRoot, lay.Root, lay.Platform)); err != nil {
			t.Fatalf("remove binding: %v", err)
		}
		_, err := eng.GC(ctx)
		if err == nil || !strings.Contains(err.Error(), "held image") || !strings.Contains(err.Error(), "no binding") {
			t.Fatalf("GC = %v, want the held-no-binding abort", err)
		}
		assertObjectExists(t, storeRoot, gcUniqueDigest("heldnobinding"))
	})

	t.Run("second alias with a broken resolved chain", func(t *testing.T) {
		eng, storeRoot, lay := gcEngine(t, "aliasbroken")
		// A second reference record to the same identity whose resolved root
		// names a blob the store does not hold: sharing an already-verified
		// identity must not exempt the alias's own chain from the fail-safe.
		data, err := os.ReadFile(octest.RefPath(storeRoot, mustNormalizeRef(t, lay.Ref))) // #nosec G304 -- test path derived from the ref it pulled
		if err != nil {
			t.Fatalf("read ref record: %v", err)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("decode ref record: %v", err)
		}
		// The listing is sorted by ref string, so a "zzz" alias is verified
		// after the good record — the position the dedup would have skipped.
		const alias = "zzz-broken-alias"
		sum := sha256.Sum256([]byte(alias))
		doc["ref"] = alias
		doc["refKey"] = map[string]any{"algorithm": "sha256", "digest": "sha256:" + hex.EncodeToString(sum[:])}
		doc["resolved"] = map[string]any{"mediaType": "", "digest": digest.SHA256.FromString("absent resolved root").String(), "size": 1}
		mutated, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("encode alias record: %v", err)
		}
		aliasPath := octest.RefPath(storeRoot, alias)
		if err := os.MkdirAll(filepath.Dir(aliasPath), 0o755); err != nil {
			t.Fatalf("mkdir alias shard: %v", err)
		}
		if err := os.WriteFile(aliasPath, mutated, 0o644); err != nil {
			t.Fatalf("write alias record: %v", err)
		}
		_, err = eng.GC(ctx)
		if err == nil || !strings.Contains(err.Error(), "ref "+alias) {
			t.Fatalf("GC = %v, want the broken-alias abort naming ref %s", err, alias)
		}
		assertObjectExists(t, storeRoot, gcUniqueDigest("aliasbroken"))
	})

	t.Run("repointed binding", func(t *testing.T) {
		eng, storeRoot, layA := gcEngine(t, "repointkeep")
		layB := gcPull(t, eng, "repointother")
		// Repoint image A's binding at image B's composefs object without
		// updating the recorded fs-verity digest: the binding is deletion
		// authority for what gc preserves, so the mismatch must abort the
		// collection, not silently protect the wrong image.
		other := readPullBinding(t, storeRoot, layB.Root, layB.Platform).RootFS.Descriptor
		path := octest.BindingPath(storeRoot, layA.Root, layA.Platform)
		data, err := os.ReadFile(path) // #nosec G304 -- test path derived from the image it pulled
		if err != nil {
			t.Fatalf("read binding: %v", err)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("decode binding: %v", err)
		}
		rootfs := doc["rootfs"].(map[string]any)
		desc := rootfs["descriptor"].(map[string]any)
		desc["digest"] = other.Digest.String()
		desc["size"] = other.Size
		mutated, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("encode binding: %v", err)
		}
		if err := os.WriteFile(path, mutated, 0o644); err != nil {
			t.Fatalf("write binding: %v", err)
		}
		_, err = eng.GC(ctx)
		if err == nil || !strings.Contains(err.Error(), "does not select this image's own composefs object") {
			t.Fatalf("GC = %v, want the repointed-binding abort", err)
		}
		assertObjectExists(t, storeRoot, gcUniqueDigest("repointkeep"))
		assertObjectExists(t, storeRoot, gcUniqueDigest("repointother"))
	})
}

// TestGCStoreBusy pins the fail-fast lock: gc never blocks behind a shared
// holder, it reports the busy store and the caller retries.
func TestGCStoreBusy(t *testing.T) {
	eng, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e := eng.(*engine)
	unlock, err := e.lockStoreShared()
	if err != nil {
		t.Fatalf("lockStoreShared: %v", err)
	}
	if _, err := eng.GC(context.Background()); !errors.Is(err, ErrStoreBusy) {
		t.Fatalf("GC under a shared holder = %v, want ErrStoreBusy", err)
	}
	unlock()

	report, err := eng.GC(context.Background())
	if err != nil {
		t.Fatalf("GC on empty store: %v", err)
	}
	if !reflect.DeepEqual(*report, GCReport{}) {
		t.Fatalf("GC on empty store = %+v, want a zero report", report)
	}
}

// TestGCSkipsUnmeasurableObject pins the census's conservative branch: an
// object without a measurable fs-verity digest cannot be proven unwanted —
// wanted per-file objects are marked only by fs-verity name — so gc must
// skip it with a warning, never delete it, and still complete.
func TestGCSkipsUnmeasurableObject(t *testing.T) {
	eng, storeRoot, _ := gcEngine(t, "measure")
	raw := digest.SHA256.FromString("unmeasurable bytes")
	path := octest.ObjectPath(storeRoot, raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Written directly, without the store's fs-verity seal: measuring it fails.
	if err := os.WriteFile(path, []byte("unmeasurable bytes"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := eng.GC(context.Background()); err != nil {
		t.Fatalf("GC: %v", err)
	}
	assertObjectExists(t, storeRoot, raw)
}

// TestGCAbortsOnHeldIndex pins the held-index abort and its remedy: bindings
// are keyed by the platform-manifest identity, never by an index, so a hold
// stranded on an index digest can only be released — the error must say so
// and must not send the operator on a futile pull.
func TestGCAbortsOnHeldIndex(t *testing.T) {
	eng, storeRoot, _ := gcEngine(t, "heldindex")
	e := eng.(*engine)
	ctx := context.Background()

	index, _, err := e.obj.WriteObject(ctx, strings.NewReader(
		`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`))
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}
	unlock, err := e.holds.Lock(index)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	err = e.holds.Hold(index, "keeper")
	unlock()
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}

	_, err = eng.GC(ctx)
	if err == nil || !strings.Contains(err.Error(), "image index") || !strings.Contains(err.Error(), "release") ||
		strings.Contains(err.Error(), "knarr pull") {
		t.Fatalf("GC = %v, want the held-index abort naming release only", err)
	}
	assertObjectExists(t, storeRoot, gcUniqueDigest("heldindex"))
	assertObjectExists(t, storeRoot, index)
	if err := eng.Release(ctx, index.String(), "keeper"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := eng.GC(ctx); err != nil {
		t.Fatalf("GC after release: %v", err)
	}
	assertObjectExists(t, storeRoot, gcUniqueDigest("heldindex"))
	assertObjectAbsent(t, storeRoot, index)
}
