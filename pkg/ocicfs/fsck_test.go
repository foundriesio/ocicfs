// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocicfs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/devtest"
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
