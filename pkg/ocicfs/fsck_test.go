// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocicfs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/composefs"
	"github.com/foundriesio/ocicfs/internal/devtest"
	"github.com/foundriesio/ocicfs/internal/objectstore"
	"github.com/foundriesio/ocicfs/internal/octest"
	"github.com/foundriesio/ocicfs/internal/verity"
)

// fsckModes runs fn on an empty store without fs-verity and on one with it,
// skipping a mode the build or the environment does not provide.
func fsckModes(t *testing.T, fn func(t *testing.T, eng Engine, storeRoot string)) {
	t.Helper()
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	run := func(t *testing.T, storeRoot string) {
		eng, err := New(registryConfig(storeRoot))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		fn(t, eng, storeRoot)
	}
	t.Run("off", func(t *testing.T) {
		if verity.Mode == verity.ModeRequired {
			t.Skip("a build made in required mode creates no store without fs-verity")
		}
		run(t, plainStoreRoot(t))
	})
	t.Run("required", func(t *testing.T) {
		if verity.Mode == verity.ModeOff {
			t.Skip("a build made in off mode never enables fs-verity")
		}
		storeRoot, cleanup, err := devtest.VerityRoot()
		if errors.Is(err, devtest.ErrNoVerity) {
			t.Skipf("verity-capable store root unavailable: %v", err)
		}
		if err != nil {
			t.Fatalf("VerityRoot: %v", err)
		}
		t.Cleanup(cleanup)
		run(t, storeRoot)
	})
}

// fsckBody is the content of the file unique to the image gcPull builds for
// unique.
func fsckBody(unique string) string {
	return "unique payload for " + unique + ", padded far beyond the sixty-four byte inline threshold\n"
}

// fsckPaths returns the two names the store gives an object with content
// body, and the fs-verity digest the second carries.
func fsckPaths(t *testing.T, storeRoot, body string) (objPath, viewPath, fsv string) {
	t.Helper()
	fsv, err := verity.Digest(context.Background(), strings.NewReader(body))
	if err != nil {
		t.Fatalf("verity.Digest: %v", err)
	}
	return octest.ObjectPath(storeRoot, digest.FromString(body)),
		filepath.Join(storeRoot, "objects", "fsverity-sha256-12", viewRelativeName(fsv)), fsv
}

// fsckDamage replaces the object with content victim by the one with
// content donor, under the victim's two names: an object whose bytes match
// neither name. The donor's own names are gone afterwards.
func fsckDamage(t *testing.T, storeRoot, victim, donor string) {
	t.Helper()
	victimObj, victimView, _ := fsckPaths(t, storeRoot, victim)
	donorObj, donorView, _ := fsckPaths(t, storeRoot, donor)
	for from, to := range map[string]string{donorObj: victimObj, donorView: victimView} {
		if err := os.Rename(from, to); err != nil {
			t.Fatal(err)
		}
	}
}

// fsckDonor stores an object no image needs and returns its content.
func fsckDonor(t *testing.T, eng Engine, name string) string {
	t.Helper()
	lay := gcPull(t, eng, name)
	if err := eng.Remove(context.Background(), lay.Ref); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	return fsckBody(name)
}

func fsck(t *testing.T, eng Engine, refs ...string) *FsckReport {
	t.Helper()
	report, err := eng.Fsck(context.Background(), refs)
	if err != nil {
		t.Fatalf("Fsck: %v", err)
	}
	return report
}

// fsckImageOf returns the report's entry for the image lay.
func fsckImageOf(t *testing.T, report *FsckReport, lay octest.Layout) FsckImage {
	t.Helper()
	for _, img := range report.Images {
		if img.Digest == lay.Root {
			return img
		}
	}
	t.Fatalf("image %s not in the report: %+v", lay.Root, report)
	return FsckImage{}
}

// wantOneObject asserts img is not intact because of exactly one object with
// the given problem, and returns it.
func wantOneObject(t *testing.T, img FsckImage, problem string) FsckObject {
	t.Helper()
	if img.Intact || len(img.Objects) != 1 || img.Objects[0].Problem != problem {
		t.Fatalf("image = %+v, want one %s object", img, problem)
	}
	return img.Objects[0]
}

func mustRemoveFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func TestFsckIntactStore(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, _ string) {
		layA := gcPull(t, eng, "a")
		layB := gcPull(t, eng, "b")
		// Garbage is checked too, and intact garbage is not a finding.
		fsckDonor(t, eng, "garbage")

		report := fsck(t, eng)
		if !report.Intact || len(report.Images) != 2 || len(report.Unreferenced) != 0 {
			t.Fatalf("store-wide report = %+v", report)
		}
		for _, lay := range []octest.Layout{layA, layB} {
			img := fsckImageOf(t, report, lay)
			if !img.Intact || img.Error != "" || len(img.Refs) != 1 || img.Refs[0] != mustNormalizeRef(t, lay.Ref) || img.Platform == "" {
				t.Fatalf("image = %+v", img)
			}
		}
		if byRef := fsck(t, eng, layA.Ref); !byRef.Intact || len(byRef.Images) != 1 || byRef.Images[0].Digest != layA.Root {
			t.Fatalf("report for one reference = %+v", byRef)
		}
	})
}

func TestFsckDamagedObject(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		layA := gcPull(t, eng, "a")
		layB := gcPull(t, eng, "b")
		fsckDamage(t, storeRoot, fsckBody("a"), fsckDonor(t, eng, "donor"))

		report := fsck(t, eng)
		if report.Intact || len(report.Unreferenced) != 0 {
			t.Fatalf("report = %+v", report)
		}
		o := wantOneObject(t, fsckImageOf(t, report, layA), FsckDamaged)
		_, _, fsv := fsckPaths(t, storeRoot, fsckBody("a"))
		if o.Digest != digest.FromString(fsckBody("a")) || o.Fsverity != fsv {
			t.Fatalf("damaged object = %+v", o)
		}
		if img := fsckImageOf(t, report, layB); !img.Intact {
			t.Fatalf("the other image = %+v", img)
		}
		if byRef := fsck(t, eng, layB.Ref); !byRef.Intact {
			t.Fatalf("report for the intact image = %+v", byRef)
		}
		wantOneObject(t, fsckImageOf(t, fsck(t, eng, layA.Ref), layA), FsckDamaged)
	})
}

func TestFsckDamagedSharedObject(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		layA := gcPull(t, eng, "a")
		layB := gcPull(t, eng, "b")
		fsckDamage(t, storeRoot, gcSharedBody, fsckDonor(t, eng, "donor"))

		report := fsck(t, eng)
		wantOneObject(t, fsckImageOf(t, report, layA), FsckDamaged)
		wantOneObject(t, fsckImageOf(t, report, layB), FsckDamaged)
	})
}

// Without fs-verity an object's bytes can change in place.
func TestFsckChangedBytes(t *testing.T) {
	if verity.Mode == verity.ModeRequired {
		t.Skip("a build made in required mode creates no store without fs-verity")
	}
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	storeRoot := plainStoreRoot(t)
	eng, err := New(registryConfig(storeRoot))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lay := gcPull(t, eng, "a")
	objPath, _, _ := fsckPaths(t, storeRoot, fsckBody("a"))
	if err := os.WriteFile(objPath, []byte("flipped"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantOneObject(t, fsckImageOf(t, fsck(t, eng), lay), FsckDamaged)
}

func TestFsckMissingObject(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		lay := gcPull(t, eng, "a")
		objPath, viewPath, fsv := fsckPaths(t, storeRoot, fsckBody("a"))
		mustRemoveFile(t, objPath)
		mustRemoveFile(t, viewPath)

		for _, report := range []*FsckReport{fsck(t, eng), fsck(t, eng, lay.Ref)} {
			o := wantOneObject(t, fsckImageOf(t, report, lay), FsckMissing)
			if o.Fsverity != fsv || o.Digest != "" || report.Intact {
				t.Fatalf("missing object = %+v", o)
			}
		}
	})
}

func TestFsckMisnamedObject(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		// An object that lost its view name: intact on its own, misnamed
		// for the image that reaches it by that name.
		layA := gcPull(t, eng, "a")
		_, viewA, _ := fsckPaths(t, storeRoot, fsckBody("a"))
		mustRemoveFile(t, viewA)
		// An object that lost its content-addressed name.
		layB := gcPull(t, eng, "b")
		objB, _, fsvB := fsckPaths(t, storeRoot, fsckBody("b"))
		mustRemoveFile(t, objB)

		for _, report := range []*FsckReport{fsck(t, eng), fsck(t, eng, layA.Ref, layB.Ref)} {
			if a := wantOneObject(t, fsckImageOf(t, report, layA), FsckMisnamed); a.Digest != digest.FromString(fsckBody("a")) {
				t.Fatalf("object without a view name = %+v", a)
			}
			if b := wantOneObject(t, fsckImageOf(t, report, layB), FsckMisnamed); b.Fsverity != fsvB {
				t.Fatalf("object without a content-addressed name = %+v", b)
			}
			if len(report.Unreferenced) != 0 {
				t.Fatalf("unreferenced = %+v", report.Unreferenced)
			}
		}
	})
}

func TestFsckImageLevelFailures(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		ctx := context.Background()

		// No binding.
		noBinding := gcPull(t, eng, "nobinding")
		mustRemoveFile(t, octest.BindingPath(storeRoot, noBinding.Root, noBinding.Platform))

		// No manifest.
		noManifest := gcPull(t, eng, "nomanifest")
		mustRemoveFile(t, octest.ObjectPath(storeRoot, noManifest.Root))

		// A composefs image that is not the one the binding records.
		swapped := gcPull(t, eng, "swapped")
		other := gcPull(t, eng, "other")
		swappedInfo, err := eng.Inspect(ctx, swapped.Ref)
		if err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		otherInfo, err := eng.Inspect(ctx, other.Ref)
		if err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		if err := os.Rename(octest.ObjectPath(storeRoot, otherInfo.Composefs.Digest), octest.ObjectPath(storeRoot, swappedInfo.Composefs.Digest)); err != nil {
			t.Fatal(err)
		}

		report := fsck(t, eng)
		if report.Intact {
			t.Fatalf("report = %+v", report)
		}
		if img := fsckImageOf(t, report, noBinding); img.Intact || !strings.Contains(img.Error, "no composefs image") {
			t.Fatalf("image without a binding = %+v", img)
		}
		img := fsckImageOf(t, report, noManifest)
		if o := wantOneObject(t, img, FsckMissing); o.Digest != noManifest.Root || img.Error == "" {
			t.Fatalf("image without a manifest = %+v", img)
		}
		img = fsckImageOf(t, report, swapped)
		if img.Intact || len(img.Objects) != 1 || img.Objects[0].Digest != swappedInfo.Composefs.Digest || img.Objects[0].Problem == FsckMissing {
			t.Fatalf("image with a foreign composefs image = %+v", img)
		}
		if o := wantOneObject(t, fsckImageOf(t, report, other), FsckMissing); o.Digest != otherInfo.Composefs.Digest {
			t.Fatalf("image without its composefs image = %+v", o)
		}
	})
}

func TestFsckUnreferencedAndHeld(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		ctx := context.Background()
		// An image kept by a hold alone is checked like one a reference
		// names.
		held := gcPull(t, eng, "held")
		if err := eng.Hold(ctx, held.Ref, "agent"); err != nil {
			t.Fatalf("Hold: %v", err)
		}
		mustRemoveFile(t, octest.RefPath(storeRoot, mustNormalizeRef(t, held.Ref)))
		// Damaged garbage: a later pull would reuse it.
		garbage := fsckDonor(t, eng, "garbage")
		fsckDamage(t, storeRoot, garbage, fsckDonor(t, eng, "donor"))

		report := fsck(t, eng)
		if img := fsckImageOf(t, report, held); !img.Intact || len(img.Refs) != 0 {
			t.Fatalf("held image = %+v", img)
		}
		if report.Intact || len(report.Unreferenced) != 1 || report.Unreferenced[0].Problem != FsckDamaged ||
			report.Unreferenced[0].Digest != digest.FromString(garbage) {
			t.Fatalf("report = %+v", report)
		}

		fsckDamage(t, storeRoot, fsckBody("held"), fsckDonor(t, eng, "donor2"))
		wantOneObject(t, fsckImageOf(t, fsck(t, eng), held), FsckDamaged)
	})
}

// fsckAliasThroughMissingIndex gives the image lay a second reference whose
// root is an index the store does not have, plus healthy aliases so that
// some reference is listed before the broken one. It returns the index.
func fsckAliasThroughMissingIndex(t *testing.T, eng Engine, lay octest.Layout) digest.Digest {
	t.Helper()
	e := eng.(*engine)
	rec, err := e.refs.Get(mustNormalizeRef(t, lay.Ref))
	if err != nil {
		t.Fatalf("refs.Get: %v", err)
	}
	for _, name := range []string{"example.com/alias-1:v1", "example.com/alias-2:v1", "example.com/alias-3:v1"} {
		if err := e.refs.Put(name, rec.Resolved, rec.Identity, rec.PulledAt, rec.Platforms); err != nil {
			t.Fatalf("refs.Put: %v", err)
		}
	}
	index := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: digest.FromString("an index that is gone"), Size: 1}
	if err := e.refs.Put("example.com/through-index:v1", index, rec.Identity, rec.PulledAt, rec.Platforms); err != nil {
		t.Fatalf("refs.Put: %v", err)
	}
	return index.Digest
}

// An image two references name, one of them through a root that is missing:
// that reference's failure is the image's, whichever is listed first.
func TestFsckSecondRootMissing(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, _ string) {
		index := fsckAliasThroughMissingIndex(t, eng, gcPull(t, eng, "a"))
		report := fsck(t, eng)
		if len(report.Images) != 1 || len(report.Images[0].Refs) != 5 {
			t.Fatalf("report = %+v", report)
		}
		img := report.Images[0]
		if o := wantOneObject(t, img, FsckMissing); o.Digest != index || img.Error == "" {
			t.Fatalf("image = %+v", img)
		}
	})
}

// A reference is checked through the root its record names, as the
// store-wide check does, and the image behind a root that is gone is checked
// all the same.
func TestFsckReferenceThroughMissingIndex(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		index := fsckAliasThroughMissingIndex(t, eng, gcPull(t, eng, "a"))
		img := fsck(t, eng, "example.com/through-index:v1").Images[0]
		if o := wantOneObject(t, img, FsckMissing); o.Digest != index || img.Error == "" {
			t.Fatalf("image = %+v", img)
		}
		objPath, viewPath, _ := fsckPaths(t, storeRoot, fsckBody("a"))
		mustRemoveFile(t, objPath)
		mustRemoveFile(t, viewPath)
		if img := fsck(t, eng, "example.com/through-index:v1").Images[0]; len(img.Objects) != 2 {
			t.Fatalf("image without a file = %+v", img)
		}
	})
}

// A damaged config is an object its image needs, though the image's
// metadata no longer resolves through it.
func TestFsckDamagedConfig(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		lay := gcPull(t, eng, "a")
		donor := fsckDonor(t, eng, "donor")
		config, err := os.ReadFile(octest.ObjectPath(storeRoot, lay.Config)) // #nosec G304 -- a path inside the test's store
		if err != nil {
			t.Fatal(err)
		}
		fsckDamage(t, storeRoot, string(config), donor)
		for _, refs := range [][]string{nil, {lay.Ref}} {
			report := fsck(t, eng, refs...)
			img := fsckImageOf(t, report, lay)
			if o := wantOneObject(t, img, FsckDamaged); o.Digest != lay.Config || img.Error == "" || len(report.Unreferenced) != 0 {
				t.Fatalf("fsck %v = %+v", refs, report)
			}
		}
	})
}

// fsckUnwantedImage pulls an image under a hold and releases it: the binding
// stays, and no reference or hold keeps the image. With damage it then
// removes one of the image's file objects.
func fsckUnwantedImage(t *testing.T, eng Engine, storeRoot string, damage bool) octest.Layout {
	t.Helper()
	ctx := context.Background()
	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
		octest.File{Name: "etc/unwanted.txt", Mode: 0o644, Body: fsckBody("unwanted")})
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}
	if _, err := eng.Pull(ctx, lay.Ref, WithFromDir(lay.Dir), WithHold("runtime")); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if err := eng.Release(ctx, lay.Root.String(), "runtime"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if damage {
		objPath, viewPath, _ := fsckPaths(t, storeRoot, fsckBody("unwanted"))
		mustRemoveFile(t, objPath)
		mustRemoveFile(t, viewPath)
	}
	return lay
}

// A remove can delete an unwanted image's binding after the check listed it.
// That image is gone, not damaged.
func TestFsckUnwantedBindingRemovedMeanwhile(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		lay := fsckUnwantedImage(t, eng, storeRoot, false)
		fsckBeforeBinding = func() {
			fsckBeforeBinding = nil
			// The runtime reference outlives the released hold.
			if err := eng.Remove(context.Background(), lay.Ref); err != nil {
				t.Errorf("Remove: %v", err)
			}
		}
		t.Cleanup(func() { fsckBeforeBinding = nil })
		if report := fsck(t, eng); !report.Intact || len(report.Images) != 0 {
			t.Fatalf("report = %+v", report)
		}
		if _, err := os.Stat(octest.BindingPath(storeRoot, lay.Root, lay.Platform)); err == nil {
			t.Fatal("the remove left the binding: the test did not interleave")
		}
	})
}

// An image nothing keeps is checked as long as it has a binding: a pull of it
// would answer that it is already there.
func TestFsckUnwantedBinding(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		lay := fsckUnwantedImage(t, eng, storeRoot, true)
		report := fsck(t, eng)
		if report.Intact || len(report.Images) != 1 {
			t.Fatalf("report = %+v", report)
		}
		wantOneObject(t, fsckImageOf(t, report, lay), FsckMissing)
	})
}

func TestFsckUnknownReference(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, _ string) {
		gcPull(t, eng, "a")
		report, err := eng.Fsck(context.Background(), []string{"example.com/absent:latest"})
		if !errors.Is(err, ErrNotFound) || report != nil {
			t.Fatalf("Fsck = %+v, %v; want ErrNotFound", report, err)
		}
	})
}

// A failure that is no evidence about an object's bytes stops the check, and
// no report is made from a part of the checks.
func TestFsckOperationalError(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, _ string) {
		gcPull(t, eng, "a")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if report, err := eng.Fsck(ctx, nil); !errors.Is(err, context.Canceled) || report != nil {
			t.Fatalf("canceled Fsck = %+v, %v", report, err)
		}

		// Cancellation in the middle of a check is not damage either.
		ctx, cancel = context.WithCancel(context.Background())
		fsckHook(t, cancel)
		if report, err := eng.Fsck(ctx, nil); !errors.Is(err, context.Canceled) || report != nil {
			t.Fatalf("Fsck canceled midway = %+v, %v", report, err)
		}
	})
}

// A store run with fs-verity that holds an intact object without it is in
// the wrong mode, not damaged.
func TestFsckObjectWithoutVerity(t *testing.T) {
	if verity.Mode == verity.ModeOff {
		t.Skip("a build made in off mode never enables fs-verity")
	}
	eng, storeRoot, _ := gcEngine(t, "a")
	objPath, viewPath, _ := fsckPaths(t, storeRoot, fsckBody("a"))
	mustRemoveFile(t, objPath)
	mustRemoveFile(t, viewPath)
	if err := os.WriteFile(objPath, []byte(fsckBody("a")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(objPath, viewPath); err != nil {
		t.Fatal(err)
	}
	report, err := eng.Fsck(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "fs-verity") || report != nil {
		t.Fatalf("Fsck = %+v, %v; want the no-fs-verity error", report, err)
	}
}

// fsckPullVariant makes the host linux/arm64 for the test and pulls an image
// whose config spells that platform with its default variant, as images
// built for arm64 do.
func fsckPullVariant(t *testing.T, eng Engine, unique string) octest.Layout {
	t.Helper()
	host := hostPlatform
	hostPlatform = func() ocispec.Platform { return ocispec.Platform{OS: "linux", Architecture: "arm64"} }
	t.Cleanup(func() { hostPlatform = host })
	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"},
		octest.File{Name: "etc/" + unique + ".txt", Mode: 0o644, Body: fsckBody(unique)})
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

// The reference records linux/arm64/v8 and the binding is stored as
// linux/arm64. They are one image, also when its metadata does not resolve
// and only those two spellings are left to tell.
func TestFsckDefaultVariantListedOnce(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		lay := fsckPullVariant(t, eng, "variant")
		if report := fsck(t, eng); !report.Intact || len(report.Images) != 1 || report.Images[0].Platform != "linux/arm64/v8" {
			t.Fatalf("report = %+v", report)
		}
		mustRemoveFile(t, octest.ObjectPath(storeRoot, lay.Config))
		if report := fsck(t, eng); report.Intact || len(report.Images) != 1 {
			t.Fatalf("report without the config = %+v", report)
		}
		// A check by reference names the platform as the record does.
		if img := fsck(t, eng, lay.Ref).Images[0]; img.Intact || img.Platform != "linux/arm64/v8" {
			t.Fatalf("image by reference = %+v", img)
		}
	})
}

func fsckHook(t *testing.T, fn func()) {
	t.Helper()
	fsckBetweenPhases = func() {
		fsckBetweenPhases = nil
		fn()
	}
	t.Cleanup(func() { fsckBetweenPhases = nil })
}

// A pull that commits while the check runs produces no finding, and a
// collection is refused meanwhile.
func TestFsckDuringPull(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, _ string) {
		gcPull(t, eng, "a")
		var gcErr error
		fsckHook(t, func() {
			gcPull(t, eng, "b")
			_, gcErr = eng.GC(context.Background())
		})
		report := fsck(t, eng)
		if !report.Intact || len(report.Images) != 1 {
			t.Fatalf("report = %+v", report)
		}
		if !errors.Is(gcErr, ErrStoreBusy) {
			t.Fatalf("GC during a check = %v, want ErrStoreBusy", gcErr)
		}
		if report := fsck(t, eng); !report.Intact || len(report.Images) != 2 {
			t.Fatalf("report after the pull = %+v", report)
		}
	})
}

func fsckFix(t *testing.T, eng Engine, opts ...FsckOption) *FsckReport {
	t.Helper()
	report, err := eng.Fsck(context.Background(), nil, append([]FsckOption{WithFix()}, opts...)...)
	if err != nil {
		t.Fatalf("Fsck with a repair: %v", err)
	}
	return report
}

// A repair says it removed the binding of an image whose config names the
// architecture's default variant, which the binding's own platform omits.
func TestFsckFixDefaultVariant(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		lay := fsckPullVariant(t, eng, "variant")
		objPath, viewPath, _ := fsckPaths(t, storeRoot, fsckBody("variant"))
		mustRemoveFile(t, objPath)
		mustRemoveFile(t, viewPath)
		report := fsckFix(t, eng)
		if len(report.Images) != 1 {
			t.Fatalf("report = %+v", report)
		}
		wantFixed(t, report.Images[0], false, true)
		if isPresent(octest.BindingPath(storeRoot, lay.Root, lay.Platform)) {
			t.Fatal("the binding was left")
		}
	})
}

// A file put in the place of an object's content-addressed name: the repair
// deletes it and gives the object, intact under its view name, that name
// back, so nothing needs a pull.
func TestFsckFixReplacedObject(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		lay := gcPull(t, eng, "a")
		objPath, _, _ := fsckPaths(t, storeRoot, fsckBody("a"))
		mustRemoveFile(t, objPath)
		if err := os.WriteFile(objPath, []byte("dsads\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		report := fsck(t, eng)
		wantOneObject(t, fsckImageOf(t, report, lay), FsckMisnamed)
		if len(report.Unreferenced) != 1 || report.Unreferenced[0].Problem != FsckDamaged {
			t.Fatalf("report = %+v", report)
		}
		report = fsckFix(t, eng)
		wantFixed(t, fsckImageOf(t, report, lay), true, false)
		if !*report.Unreferenced[0].Fixed {
			t.Fatalf("report of the repair = %+v", report)
		}
		wantIntact(t, eng)
		if got, err := os.ReadFile(objPath); err != nil || string(got) != fsckBody("a") { // #nosec G304 -- test store path
			t.Fatalf("object = %q, %v", got, err)
		}
	})
}

// wantFixed asserts img lists exactly one failed object and that the repair
// fixed it or left it, and removed img's binding or kept it.
func wantFixed(t *testing.T, img FsckImage, fixed, bindingRemoved bool) FsckObject {
	t.Helper()
	if len(img.Objects) != 1 || img.BindingRemoved == nil || *img.BindingRemoved != bindingRemoved {
		t.Fatalf("image = %+v, want one object and binding removed = %v", img, bindingRemoved)
	}
	o := img.Objects[0]
	if o.Problem != FsckMissing && (o.Fixed == nil || *o.Fixed != fixed) {
		t.Fatalf("object = %+v, want fixed = %v", o, fixed)
	}
	return o
}

func wantIntact(t *testing.T, eng Engine) {
	t.Helper()
	if report := fsck(t, eng); !report.Intact {
		t.Fatalf("store not intact: %+v", report)
	}
}

func isPresent(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func repull(t *testing.T, eng Engine, lay octest.Layout, opts ...PullOption) {
	t.Helper()
	if len(opts) == 0 {
		opts = []PullOption{WithFromDir(lay.Dir)}
	}
	if _, err := eng.Pull(context.Background(), lay.Ref, opts...); err != nil {
		t.Fatalf("Pull %s: %v", lay.Ref, err)
	}
}

// layerReads counts the layer blobs a pull opens.
type layerReads struct {
	BlobSource
	n int
}

func (s *layerReads) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	if strings.Contains(desc.MediaType, "layer") {
		s.n++
	}
	return s.BlobSource.Open(ctx, desc)
}

// A damaged object two wanted images and one unwanted binding share: the
// repair deletes it and removes all three bindings, gc refuses until the
// wanted images are pulled again, and only the first of those pulls reads a
// layer.
func TestFsckFixSharedObject(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		ctx := context.Background()
		layA := gcPull(t, eng, "a")
		layB := gcPull(t, eng, "b")
		unwanted := gcPull(t, eng, "unwanted")
		mustRemoveFile(t, octest.RefPath(storeRoot, mustNormalizeRef(t, unwanted.Ref)))
		fsckDamage(t, storeRoot, gcSharedBody, fsckDonor(t, eng, "donor"))

		report := fsckFix(t, eng)
		if report.Intact {
			t.Fatal("a report with damage found reads intact after the repair")
		}
		wantFixed(t, fsckImageOf(t, report, layA), true, true)
		wantFixed(t, fsckImageOf(t, report, layB), true, true)
		objPath, viewPath, _ := fsckPaths(t, storeRoot, gcSharedBody)
		if isPresent(objPath) || isPresent(viewPath) {
			t.Fatal("the damaged object survived the repair")
		}
		for _, lay := range []octest.Layout{layA, layB, unwanted} {
			if isPresent(octest.BindingPath(storeRoot, lay.Root, lay.Platform)) {
				t.Fatalf("binding of %s survived the repair", lay.Ref)
			}
		}
		if _, err := eng.Inspect(ctx, layA.Ref); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Inspect of an image to pull again = %v, want ErrNotFound", err)
		}
		if _, err := eng.GC(ctx); err == nil || !strings.Contains(err.Error(), "ocicfs pull") {
			t.Fatalf("GC before the pulls = %v, want a refusal naming `ocicfs pull`", err)
		}

		first := &layerReads{BlobSource: layoutBlobs(t, layA.Dir)}
		repull(t, eng, layA, WithBlobSource(first))
		second := &layerReads{BlobSource: layoutBlobs(t, layB.Dir)}
		repull(t, eng, layB, WithBlobSource(second))
		if first.n != 1 || second.n != 0 {
			t.Fatalf("layer reads = %d then %d, want 1 then 0", first.n, second.n)
		}
		if _, err := eng.GC(ctx); err != nil {
			t.Fatalf("GC after the pulls: %v", err)
		}
		wantIntact(t, eng)
	})
}

// Misnamed objects are repaired in place: nothing is deleted, no binding is
// removed, and no source is needed.
func TestFsckFixMisnamed(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		// A lost view name.
		layA := gcPull(t, eng, "a")
		_, viewA, _ := fsckPaths(t, storeRoot, fsckBody("a"))
		mustRemoveFile(t, viewA)
		// A wrong content-addressed name over intact bytes.
		layB := gcPull(t, eng, "b")
		objB, _, _ := fsckPaths(t, storeRoot, fsckBody("b"))
		wrong := octest.ObjectPath(storeRoot, digest.FromString("some other content"))
		if err := os.MkdirAll(filepath.Dir(wrong), 0o755); err != nil { // #nosec G301 -- test store directory
			t.Fatal(err)
		}
		if err := os.Rename(objB, wrong); err != nil {
			t.Fatal(err)
		}
		report := fsckFix(t, eng)
		for _, lay := range []octest.Layout{layA, layB} {
			if o := wantFixed(t, fsckImageOf(t, report, lay), true, false); o.Problem != FsckMisnamed {
				t.Fatalf("object of %s = %+v", lay.Ref, o)
			}
		}
		if isPresent(wrong) {
			t.Fatal("the wrong content-addressed name survived the repair")
		}
		wantIntact(t, eng)
	})
}

// A misnamed object whose correct name another object holds is left as it
// is: no name is taken from either object and both contents stay readable.
func TestFsckFixLeavesTakenNames(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		// Two objects carry each other's view name.
		layA := gcPull(t, eng, "a")
		layB := gcPull(t, eng, "b")
		objA, viewA, _ := fsckPaths(t, storeRoot, fsckBody("a"))
		objB, viewB, _ := fsckPaths(t, storeRoot, fsckBody("b"))
		for _, mv := range [][2]string{{viewA, viewA + ".tmp"}, {viewB, viewA}, {viewA + ".tmp", viewB}} {
			if err := os.Rename(mv[0], mv[1]); err != nil {
				t.Fatal(err)
			}
		}
		// An object that lost its content-addressed name to a second copy
		// of the same content.
		layC := gcPull(t, eng, "c")
		objC, viewC, _ := fsckPaths(t, storeRoot, fsckBody("c"))
		mustRemoveFile(t, objC)
		if err := os.Rename(viewC, viewC+".first"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := eng.(*engine).obj.WriteObject(context.Background(), strings.NewReader(fsckBody("c"))); err != nil {
			t.Fatalf("WriteObject: %v", err)
		}
		mustRemoveFile(t, viewC)
		if err := os.Rename(viewC+".first", viewC); err != nil {
			t.Fatal(err)
		}

		report := fsckFix(t, eng)
		for _, lay := range []octest.Layout{layA, layB, layC} {
			o := wantFixed(t, fsckImageOf(t, report, lay), false, false)
			if o.Problem != FsckMisnamed || !strings.Contains(o.Detail, "held by another object") {
				t.Fatalf("object of %s = %+v", lay.Ref, o)
			}
		}
		for path, want := range map[string]string{objA: fsckBody("a"), objB: fsckBody("b"), objC: fsckBody("c"), viewC: fsckBody("c")} {
			got, err := os.ReadFile(path) // #nosec G304 -- test store path
			if err != nil || string(got) != want {
				t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
			}
		}
		if !isPresent(viewA) || !isPresent(viewB) {
			t.Fatal("the repair removed a view name")
		}
	})
}

// A root an image is reached through is not the binding's: when it is
// missing, the report says so on the image and the binding stays, since a
// pull of that reference stores the root again with the binding in place.
func TestFsckFixKeepsBindingOverMissingRoot(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		lay := gcPull(t, eng, "a")
		fsckAliasThroughMissingIndex(t, eng, lay)
		img := fsckImageOf(t, fsckFix(t, eng), lay)
		if o := wantFixed(t, img, false, false); o.Problem != FsckMissing || img.Error == "" {
			t.Fatalf("image = %+v", img)
		}
		if !isPresent(octest.BindingPath(storeRoot, lay.Root, lay.Platform)) {
			t.Fatal("the repair removed the binding of a complete image")
		}
		// The same with the broken reference removed between the check and
		// the repair: what the check listed for it no longer counts.
		fsckBeforeFixHook(t, func() {
			if err := eng.Remove(context.Background(), "example.com/through-index:v1"); err != nil {
				t.Errorf("Remove: %v", err)
			}
		})
		fsckFix(t, eng)
		if !isPresent(octest.BindingPath(storeRoot, lay.Root, lay.Platform)) {
			t.Fatal("the repair removed a healthy image's binding over a reference that is gone")
		}
		wantIntact(t, eng)
	})
}

// The repair reaches a store whose only finding is a binding nothing keeps
// over a missing object: it removes the binding, and a pull then restores
// the image.
func TestFsckFixUnwantedBinding(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		lay := fsckUnwantedImage(t, eng, storeRoot, true)
		wantFixed(t, fsckImageOf(t, fsckFix(t, eng), lay), false, true)
		if isPresent(octest.BindingPath(storeRoot, lay.Root, lay.Platform)) {
			t.Fatal("the binding survived the repair")
		}
		repull(t, eng, lay)
		wantIntact(t, eng)
		objPath, _, _ := fsckPaths(t, storeRoot, fsckBody("unwanted"))
		if !isPresent(objPath) {
			t.Fatal("the pull did not restore the object")
		}
	})
}

// A mounted image that lost its content-addressed name still protects the
// objects it refers to.
func TestFsckFixMountedImageWithViewNameOnly(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		lay := gcPull(t, eng, "a")
		info, err := eng.Inspect(context.Background(), lay.Ref)
		if err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		lockObjectShared(t, storeRoot, info.Composefs.Digest)
		mustRemoveFile(t, octest.ObjectPath(storeRoot, info.Composefs.Digest))
		fsckDamage(t, storeRoot, fsckBody("a"), fsckDonor(t, eng, "donor"))
		objPath, _, _ := fsckPaths(t, storeRoot, fsckBody("a"))

		// Without its content-addressed name the image cannot be read
		// through its binding, so its two failed objects are listed as no
		// image's: the image itself, misnamed, and the damaged file.
		report := fsckFix(t, eng)
		if img := fsckImageOf(t, report, lay); img.BindingRemoved == nil || *img.BindingRemoved {
			t.Fatalf("image = %+v", img)
		}
		if len(report.Unreferenced) != 2 {
			t.Fatalf("unreferenced = %+v", report.Unreferenced)
		}
		for _, o := range report.Unreferenced {
			switch o.Problem {
			case FsckMisnamed:
				if o.Fixed == nil || !*o.Fixed {
					t.Fatalf("the image's lost name was not repaired: %+v", o)
				}
			case FsckDamaged:
				if o.Fixed == nil || *o.Fixed || !o.InUse {
					t.Fatalf("damaged object of a mounted image = %+v", o)
				}
			default:
				t.Fatalf("object = %+v", o)
			}
		}
		if !isPresent(objPath) || !isPresent(octest.ObjectPath(storeRoot, info.Composefs.Digest)) {
			t.Fatal("the repair removed an object a mounted image uses, or did not restore the image's name")
		}
	})
}

// A failed object that changed between the check and the repair is not acted
// on and not reported as fixed.
func TestFsckFixObjectChangedBeforeRepair(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		lay := gcPull(t, eng, "a")
		fsckDamage(t, storeRoot, fsckBody("a"), fsckDonor(t, eng, "donor"))
		objPath, viewPath, _ := fsckPaths(t, storeRoot, fsckBody("a"))
		fsckBeforeFixHook(t, func() {
			if err := os.Remove(viewPath); err != nil {
				t.Error(err)
			}
		})
		o := wantFixed(t, fsckImageOf(t, fsckFix(t, eng), lay), false, true)
		if !strings.Contains(o.Detail, "changed during the run") || !isPresent(objPath) {
			t.Fatalf("object = %+v", o)
		}
		// The next run sees the remnant for what it is.
		if report := fsckFix(t, eng); len(report.Unreferenced) != 1 || isPresent(objPath) {
			t.Fatalf("second repair = %+v", report)
		}
	})
}

// A damaged object that has one name only, as a deletion interrupted between
// its two unlinks leaves it, is removed, and a pull then restores the image.
func TestFsckFixSingleNameRemnants(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		layA := gcPull(t, eng, "a")
		layB := gcPull(t, eng, "b")
		fsckDamage(t, storeRoot, fsckBody("a"), fsckDonor(t, eng, "donor-a"))
		fsckDamage(t, storeRoot, fsckBody("b"), fsckDonor(t, eng, "donor-b"))
		objA, viewA, _ := fsckPaths(t, storeRoot, fsckBody("a"))
		objB, viewB, _ := fsckPaths(t, storeRoot, fsckBody("b"))
		mustRemoveFile(t, objA)  // view name only
		mustRemoveFile(t, viewB) // content-addressed name only

		report := fsckFix(t, eng)
		wantFixed(t, fsckImageOf(t, report, layA), true, true)
		// The image reaches its object by the view name, which is gone;
		// the remnant under the other name is no image's.
		if o := wantFixed(t, fsckImageOf(t, report, layB), false, true); o.Problem != FsckMissing {
			t.Fatalf("object = %+v", o)
		}
		if len(report.Unreferenced) != 1 || report.Unreferenced[0].Fixed == nil || !*report.Unreferenced[0].Fixed {
			t.Fatalf("unreferenced = %+v", report.Unreferenced)
		}
		for _, path := range []string{objA, viewA, objB, viewB} {
			if isPresent(path) {
				t.Fatalf("%s survived the repair", path)
			}
		}
		repull(t, eng, layA)
		repull(t, eng, layB)
		wantIntact(t, eng)
	})
}

// The damaged composefs image of a mounted image is left with its binding,
// and the pass goes on to the other bindings.
func TestFsckFixKeepsMountedComposefsImage(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		ctx := context.Background()
		mounted := gcPull(t, eng, "mounted")
		info, err := eng.Inspect(ctx, mounted.Ref)
		if err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		// The donor takes the image's content-addressed name; its own view
		// name goes, so the damaged image has one name.
		donorObj, donorView, _ := fsckPaths(t, storeRoot, fsckDonor(t, eng, "donor"))
		if err := os.Rename(donorObj, octest.ObjectPath(storeRoot, info.Composefs.Digest)); err != nil {
			t.Fatal(err)
		}
		mustRemoveFile(t, donorView)
		lockObjectShared(t, storeRoot, info.Composefs.Digest)

		other := gcPull(t, eng, "other")
		objPath, viewPath, _ := fsckPaths(t, storeRoot, fsckBody("other"))
		mustRemoveFile(t, objPath)
		mustRemoveFile(t, viewPath)

		// What the mounted image refers to cannot be read from a damaged
		// image, so every damaged file object counts as used by it.
		third := gcPull(t, eng, "third")
		fsckDamage(t, storeRoot, fsckBody("third"), fsckDonor(t, eng, "donor-third"))

		report := fsckFix(t, eng)
		if o := wantFixed(t, fsckImageOf(t, report, third), false, false); !o.InUse {
			t.Fatalf("damaged object beside an unreadable mounted image = %+v", o)
		}
		img := fsckImageOf(t, report, mounted)
		if o := wantFixed(t, img, false, false); !o.InUse || o.Digest != info.Composefs.Digest {
			t.Fatalf("mounted composefs image = %+v", o)
		}
		if !isPresent(octest.ObjectPath(storeRoot, info.Composefs.Digest)) ||
			!isPresent(octest.BindingPath(storeRoot, mounted.Root, mounted.Platform)) {
			t.Fatal("a mounted composefs image or its binding was removed")
		}
		wantFixed(t, fsckImageOf(t, report, other), false, true)
		if isPresent(octest.BindingPath(storeRoot, other.Root, other.Platform)) {
			t.Fatal("the binding of the other image survived the repair")
		}
	})
}

// A damaged object a mounted image uses is left, with the image's binding,
// unless the repair is forced.
func TestFsckFixMountedImageObject(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		lay := gcPull(t, eng, "a")
		info, err := eng.Inspect(context.Background(), lay.Ref)
		if err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		lockObjectShared(t, storeRoot, info.Composefs.Digest)
		fsckDamage(t, storeRoot, fsckBody("a"), fsckDonor(t, eng, "donor"))
		objPath, _, _ := fsckPaths(t, storeRoot, fsckBody("a"))
		binding := octest.BindingPath(storeRoot, lay.Root, lay.Platform)

		if o := wantFixed(t, fsckImageOf(t, fsckFix(t, eng), lay), false, false); !o.InUse {
			t.Fatalf("object of a mounted image = %+v", o)
		}
		if !isPresent(objPath) || !isPresent(binding) {
			t.Fatal("an unforced repair removed an object a mounted image uses, or its binding")
		}

		if o := wantFixed(t, fsckImageOf(t, fsckFix(t, eng, WithFixForce()), lay), true, true); o.InUse {
			t.Fatalf("object after a forced repair = %+v", o)
		}
		if isPresent(objPath) || isPresent(binding) {
			t.Fatal("a forced repair left the object or the binding")
		}
		// The mounted composefs image itself is intact and stays.
		if !isPresent(octest.ObjectPath(storeRoot, info.Composefs.Digest)) {
			t.Fatal("the mounted composefs image was removed")
		}
	})
}

// Whatever stops a repair before its first change leaves the store as it
// was, and the report of the check is still returned.
func TestFsckFixBusyStore(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		lay := gcPull(t, eng, "a")
		fsckDamage(t, storeRoot, fsckBody("a"), fsckDonor(t, eng, "donor"))
		objPath, viewPath, _ := fsckPaths(t, storeRoot, fsckBody("a"))
		unchanged := func(what string, report *FsckReport, err, want error) {
			t.Helper()
			if err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatalf("%s: err = %v, want %v", what, err, want)
			}
			if report == nil || len(fsckImageOf(t, report, lay).Objects) != 1 || fsckImageOf(t, report, lay).Objects[0].Fixed != nil {
				t.Fatalf("%s: report = %+v", what, report)
			}
			if !isPresent(objPath) || !isPresent(viewPath) || !isPresent(octest.BindingPath(storeRoot, lay.Root, lay.Platform)) {
				t.Fatalf("%s: the store changed", what)
			}
		}

		// A pull, hold, or mount in progress.
		fsckBeforeFixHook(t, func() {
			unlock, err := eng.(*engine).lockStoreShared()
			if err != nil {
				t.Errorf("lockStoreShared: %v", err)
				return
			}
			t.Cleanup(unlock)
		})
		report, err := eng.Fsck(context.Background(), nil, WithFix())
		unchanged("busy store", report, err, ErrStoreBusy)
	})
}

func TestFsckFixChangesNothingOnError(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		lay := gcPull(t, eng, "a")
		fsckDamage(t, storeRoot, fsckBody("a"), fsckDonor(t, eng, "donor"))
		objPath, viewPath, _ := fsckPaths(t, storeRoot, fsckBody("a"))
		unchanged := func(what string, report *FsckReport, err error) {
			t.Helper()
			if err == nil || report == nil || fsckImageOf(t, report, lay).Objects[0].Fixed != nil {
				t.Fatalf("%s: report = %+v, err = %v", what, report, err)
			}
			if !isPresent(objPath) || !isPresent(viewPath) || !isPresent(octest.BindingPath(storeRoot, lay.Root, lay.Platform)) {
				t.Fatalf("%s: the store changed", what)
			}
		}

		// Cancellation after the check.
		ctx, cancel := context.WithCancel(context.Background())
		fsckBeforeFixHook(t, cancel)
		report, err := eng.Fsck(ctx, nil, WithFix())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled repair = %v", err)
		}
		unchanged("cancellation", report, err)

		// No mkcomposefs: the images could not be rebuilt afterwards.
		t.Setenv("PATH", t.TempDir())
		report, err = eng.Fsck(context.Background(), nil, WithFix())
		if err == nil || !strings.Contains(err.Error(), "mkcomposefs") {
			t.Fatalf("repair without mkcomposefs = %v", err)
		}
		unchanged("no mkcomposefs", report, err)
	})
}

// An object that fails the second check with an error that is no evidence
// about its bytes stops the repair.
func TestFsckFixStopsOnRecheckError(t *testing.T) {
	if verity.Mode == verity.ModeOff {
		t.Skip("a build made in off mode never enables fs-verity")
	}
	eng, storeRoot, _ := gcEngine(t, "a")
	other := gcPull(t, eng, "b")
	fsckDamage(t, storeRoot, fsckBody("a"), fsckDonor(t, eng, "donor-a"))
	fsckDamage(t, storeRoot, fsckBody("b"), fsckDonor(t, eng, "donor-b"))
	objPath, viewPath, _ := fsckPaths(t, storeRoot, fsckBody("a"))
	otherObj, _, _ := fsckPaths(t, storeRoot, fsckBody("b"))
	fsckBeforeFixHook(t, func() {
		// The damaged object is replaced by an intact one without fs-verity.
		for _, path := range []string{objPath, viewPath} {
			if err := os.Remove(path); err != nil {
				t.Error(err)
			}
		}
		if err := os.WriteFile(objPath, []byte(fsckBody("a")), 0o600); err != nil {
			t.Error(err)
		}
		if err := os.Link(objPath, viewPath); err != nil {
			t.Error(err)
		}
	})
	report, err := eng.Fsck(context.Background(), nil, WithFix())
	if err == nil || !strings.Contains(err.Error(), "fs-verity") || report == nil {
		t.Fatalf("Fsck = %+v, %v; want the no-fs-verity error", report, err)
	}
	if !isPresent(otherObj) || !isPresent(octest.BindingPath(storeRoot, other.Root, other.Platform)) {
		t.Fatal("the repair changed the store before stopping")
	}
}

func fsckBeforeFixHook(t *testing.T, fn func()) {
	t.Helper()
	fsckBeforeFix = func() {
		fsckBeforeFix = nil
		fn()
	}
	t.Cleanup(func() { fsckBeforeFix = nil })
}

// An object that cannot be opened is left where it is, and so is the binding
// of an image whose composefs image it is.
func TestFsckFixKeepsUnopenableObject(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		ctx := context.Background()
		e := eng.(*engine)
		lay := gcPull(t, eng, "a")
		info, err := eng.Inspect(ctx, lay.Ref)
		if err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		entry := objectstore.Entry{Digest: info.Composefs.Digest}
		unopenable := objectstore.ObjectCheck{Status: objectstore.ObjectDamaged, Unopenable: true}
		if out := e.fixObject(entry, unopenable, &mountedSet{}, true); out.fixed || out.inUse || out.detail == "" {
			t.Fatalf("outcome for an unopenable object = %+v", out)
		}
		if !isPresent(octest.ObjectPath(storeRoot, info.Composefs.Digest)) {
			t.Fatal("an unopenable object was removed")
		}
		removed, err := e.removeBrokenBindings(ctx, map[digest.Digest]bool{info.Composefs.Digest: true})
		if err != nil || len(removed) != 0 || !isPresent(octest.BindingPath(storeRoot, lay.Root, lay.Platform)) {
			t.Fatalf("removeBrokenBindings = %v, %v; want the binding kept", removed, err)
		}
	})
}

// A damaged object a hold names directly is deleted like any other, and the
// report says no pull brings it back.
func TestFsckFixHeldLooseObject(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, storeRoot string) {
		e := eng.(*engine)
		body := fsckDonor(t, eng, "loose")
		loose := digest.FromString(body)
		unlock, err := e.holds.Lock(loose)
		if err != nil {
			t.Fatalf("Lock: %v", err)
		}
		err = e.holds.Hold(loose, "keeper")
		unlock()
		if err != nil {
			t.Fatalf("Hold: %v", err)
		}
		fsckDamage(t, storeRoot, body, fsckDonor(t, eng, "donor"))

		report := fsckFix(t, eng)
		if len(report.Unreferenced) != 1 || report.Unreferenced[0].Fixed == nil || !*report.Unreferenced[0].Fixed ||
			!strings.Contains(report.Unreferenced[0].Detail, "ocicfs release") {
			t.Fatalf("unreferenced = %+v", report.Unreferenced)
		}
		if isPresent(octest.ObjectPath(storeRoot, loose)) {
			t.Fatal("the damaged held object survived the repair")
		}
	})
}

// deltaStats records how many files each delta pull made while it is
// installed found missing and fetched. Files small enough to be inlined in the
// composefs image are fetched by every pull.
type deltaStats struct {
	slog.Handler
	missing []int64
}

func (h *deltaStats) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "delta: pull" {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "missing" {
				h.missing = append(h.missing, a.Value.Int64())
			}
			return true
		})
	}
	return h.Handler.Handle(ctx, r)
}

// A delta pull restores an image whose binding a repair removed, and fetches
// the one file the repair deleted: one file more than the same pull fetches
// when only the binding is gone.
func TestFsckFixThenDeltaPull(t *testing.T) {
	requireDeltaFixtures(t)
	storeRoot := verityStoreRoot(t)
	ref := deltaFixtureRef(t, deltaReleaseN1)
	pullInto(t, storeRoot, ref, WithFromDir(deltaReleaseN1), WithDelta())
	eng := newEngine(t, storeRoot)
	info, err := eng.Inspect(context.Background(), ref)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	stats := &deltaStats{Handler: slog.NewTextHandler(io.Discard, nil)}
	old := slog.Default()
	slog.SetDefault(slog.New(stats))
	t.Cleanup(func() { slog.SetDefault(old) })

	// The control: the binding alone is gone.
	mustRemoveFile(t, octest.BindingPath(storeRoot, info.Root, info.Platform))
	pullInto(t, storeRoot, ref, WithFromDir(deltaReleaseN1), WithDelta())

	names, err := composefs.ReferencedObjects(context.Background(), octest.ObjectPath(storeRoot, info.Composefs.Digest))
	if err != nil || len(names) == 0 {
		t.Fatalf("ReferencedObjects = %v, %v", names, err)
	}
	// One of the image's file objects is replaced by other content under
	// both of its names.
	donorObj, donorView, _ := fsckPaths(t, storeRoot, fsckDonor(t, eng, "donor"))
	victimView := filepath.Join(storeRoot, "objects", "fsverity-sha256-12", names[0])
	victim, err := os.ReadFile(victimView) // #nosec G304 -- test store path
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(donorView, victimView); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(donorObj, octest.ObjectPath(storeRoot, digest.FromBytes(victim))); err != nil { // #nosec G703 -- test store path
		t.Fatal(err)
	}

	report := fsckFix(t, eng)
	if len(report.Images) != 1 || report.Images[0].BindingRemoved == nil || !*report.Images[0].BindingRemoved {
		t.Fatalf("report = %+v", report)
	}
	if isPresent(victimView) {
		t.Fatal("the damaged object survived the repair")
	}
	pullInto(t, storeRoot, ref, WithFromDir(deltaReleaseN1), WithDelta())
	if len(stats.missing) != 2 || stats.missing[1] != stats.missing[0]+1 {
		t.Fatalf("delta pulls listed %v files as missing; want one file more after the repair", stats.missing)
	}
	if _, err := eng.GC(context.Background()); err != nil {
		t.Fatalf("GC after the pull: %v", err)
	}
	wantIntact(t, eng)
}

func TestFsckFixUsage(t *testing.T) {
	fsckModes(t, func(t *testing.T, eng Engine, _ string) {
		lay := gcPull(t, eng, "a")
		ctx := context.Background()
		if _, err := eng.Fsck(ctx, []string{lay.Ref}, WithFix()); err == nil {
			t.Fatal("a repair accepted a reference")
		}
		if _, err := eng.Fsck(ctx, nil, WithFixForce()); err == nil {
			t.Fatal("force was accepted without a repair")
		}
		// A repair of an intact store changes nothing and needs no lock.
		if report := fsckFix(t, eng); !report.Intact || report.Images[0].BindingRemoved != nil {
			t.Fatalf("repair of an intact store = %+v", report)
		}
	})
}
