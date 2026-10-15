// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocicfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrlayout "github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/devtest"
	"github.com/foundriesio/ocicfs/internal/imagestore"
	"github.com/foundriesio/ocicfs/internal/octest"
)

// The tests here pin one invariant from several directions: an image is keyed
// by its platform manifest, so the door it arrived through — a caller that
// resolved the platform itself, or a reference that landed on the index — never
// changes which binding, holder directory, or record the store ends up with.

// TestIdentityMixedDoorPullKeepsOneImageE2E is the regression: an image staged
// by a caller that resolved the platform manifest itself, then pulled again by
// a tag that resolves to the index. The second pull must land on the image the
// first one stored — one binding, the hold still covering it, and no layer
// refetched — rather than storing a second copy under the index digest and
// leaving the held one orphaned.
func TestIdentityMixedDoorPullKeepsOneImageE2E(t *testing.T) {
	storeRoot := verityStoreRoot(t)
	lay := identityLayout(t, "mixed-door payload past the sixty-four byte inline limit\n")
	reg := octest.NewRegistry()
	t.Cleanup(reg.Close)
	counter := &countingTransport{inner: reg.Transport()}
	useRemoteDefaultTransport(t, counter)

	ref := "registry.example.com/team/mixed:latest"
	pushIndexTag(t, reg, ref, lay)
	eng := newEngine(t, storeRoot)

	// The staged door: the caller hands ocicfs the platform manifest it already
	// selected, and the tag is recorded as an alias for it.
	staged, err := eng.Pull(context.Background(), ref,
		WithBlobSource(layoutBlobs(t, lay.Dir)), WithResolvedRoot(lay.Manifest))
	if devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	}
	if err != nil {
		t.Fatalf("staged Pull: %v", err)
	}
	if staged.Digest != lay.Manifest.String() {
		t.Fatalf("staged pull digest = %s, want the platform manifest %s", staged.Digest, lay.Manifest)
	}
	if err := eng.Hold(context.Background(), lay.Manifest.String(), "cfs"); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	// The CLI door: the same tag, resolved by the registry to the index.
	counter.reset()
	cli, err := eng.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("registry Pull: %v", err)
	}
	if cli.Digest != lay.Manifest.String() {
		t.Fatalf("registry pull digest = %s, want the platform manifest %s", cli.Digest, lay.Manifest)
	}
	// The image was already materialized under this identity, so the second
	// pull reads the index and nothing else: a layer fetch here would mean the
	// verification keyed on the wrong digest and re-downloaded the image.
	if got := counter.blobs.Load(); got != 0 {
		t.Fatalf("registry pull fetched %d layer blobs, want 0", got)
	}

	// One image in the store, keyed by the platform manifest.
	bindings := storedBindings(t, storeRoot)
	if len(bindings) != 1 || bindings[0] != lay.Manifest {
		t.Fatalf("bindings = %v, want exactly the identity %s", bindings, lay.Manifest)
	}
	if _, err := newImageStore(t, storeRoot).Get(lay.Root, lay.Platform); !errors.Is(err, imagestore.ErrNotFound) {
		t.Fatalf("index-keyed binding error = %v, want ErrNotFound", err)
	}

	// The record keeps the index as provenance and the manifest as identity.
	rec := readPullRef(t, storeRoot, mustNormalizeRef(t, ref))
	if rec.Resolved.Digest != lay.Root {
		t.Fatalf("record resolved = %s, want the index %s", rec.Resolved.Digest, lay.Root)
	}
	if rec.Identity.Digest != lay.Manifest {
		t.Fatalf("record identity = %s, want the manifest %s", rec.Identity.Digest, lay.Manifest)
	}
	// The index the fast path fetched is stored, so a later lookup by its
	// digest resolves from local content.
	assertObjectExists(t, storeRoot, lay.Root)

	// The bug this closes: remove resolving to the index found no holders and
	// proceeded, orphaning the held binding.
	err = eng.Remove(context.Background(), ref)
	if err == nil || !strings.Contains(err.Error(), "held by cfs") {
		t.Fatalf("Remove while held error = %v, want a decline naming cfs", err)
	}
	if !strings.Contains(err.Error(), lay.Manifest.String()) {
		t.Fatalf("decline = %v, want it to name the identity digest %s", err, lay.Manifest)
	}
}

// TestIdentityReverseDoorHoldSurvivesRecordChurnE2E runs the doors the other way
// round: the tag is pulled from the registry first and held, then a staged pull
// overwrites the record with the manifest as its provenance. The identity is
// unchanged by that churn, so the hold still covers the image.
func TestIdentityReverseDoorHoldSurvivesRecordChurnE2E(t *testing.T) {
	storeRoot := verityStoreRoot(t)
	lay := identityLayout(t, "reverse-door payload past the sixty-four byte inline limit\n")
	reg := octest.NewRegistry()
	t.Cleanup(reg.Close)
	useRemoteDefaultTransport(t, reg.Transport())

	ref := "registry.example.com/team/reverse:latest"
	pushIndexTag(t, reg, ref, lay)
	eng := newEngine(t, storeRoot)

	if _, err := eng.Pull(context.Background(), ref); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("registry Pull: %v", err)
	}
	rec := readPullRef(t, storeRoot, mustNormalizeRef(t, ref))
	if rec.Resolved.Digest != lay.Root || rec.Identity.Digest != lay.Manifest {
		t.Fatalf("record = %#v, want resolved %s and identity %s", rec, lay.Root, lay.Manifest)
	}
	if err := eng.Hold(context.Background(), ref, "cfs"); err != nil {
		t.Fatalf("Hold by tag: %v", err)
	}
	assertHolders(t, storeRoot, lay.Manifest, []string{"cfs"})

	// The staged door repoints the record's provenance at the manifest.
	if _, err := eng.Pull(context.Background(), ref,
		WithBlobSource(layoutBlobs(t, lay.Dir)), WithResolvedRoot(lay.Manifest)); err != nil {
		t.Fatalf("staged Pull: %v", err)
	}
	rec = readPullRef(t, storeRoot, mustNormalizeRef(t, ref))
	if rec.Resolved.Digest != lay.Manifest || rec.Identity.Digest != lay.Manifest {
		t.Fatalf("record after staged pull = %#v, want both digests %s", rec, lay.Manifest)
	}

	err := eng.Remove(context.Background(), ref)
	if err == nil || !strings.Contains(err.Error(), "held by cfs") {
		t.Fatalf("Remove while held error = %v, want a decline naming cfs", err)
	}
	if err := eng.Release(context.Background(), ref, "cfs"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := eng.Remove(context.Background(), ref); err != nil {
		t.Fatalf("Remove after release: %v", err)
	}
}

// TestIdentityHoldByIndexDigestE2E holds an image through its index digest and
// releases it three ways: by the index while the index blob is present, by the
// index once that blob is gone (which releases the digest as given and leaves
// the hold in place), and by the identity the decline named.
func TestIdentityHoldByIndexDigestE2E(t *testing.T) {
	storeRoot := verityStoreRoot(t)
	lay := identityLayout(t, "index-hold payload past the sixty-four byte inline limit\n")
	reg := octest.NewRegistry()
	t.Cleanup(reg.Close)
	useRemoteDefaultTransport(t, reg.Transport())

	ref := "registry.example.com/team/held:latest"
	pushIndexTag(t, reg, ref, lay)
	eng := newEngine(t, storeRoot)
	if _, err := eng.Pull(context.Background(), ref); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// A hold given the index digest marks the identity it descends to.
	if err := eng.Hold(context.Background(), lay.Root.String(), "cfs"); err != nil {
		t.Fatalf("Hold by index digest: %v", err)
	}
	assertHolders(t, storeRoot, lay.Manifest, []string{"cfs"})
	assertHolders(t, storeRoot, lay.Root, nil)
	if err := eng.Remove(context.Background(), ref); err == nil || !strings.Contains(err.Error(), "held by cfs") {
		t.Fatalf("Remove while held error = %v, want a decline naming cfs", err)
	}

	// Releasing by the index digest descends the same way and clears it.
	if err := eng.Release(context.Background(), lay.Root.String(), "cfs"); err != nil {
		t.Fatalf("Release by index digest: %v", err)
	}
	assertHolders(t, storeRoot, lay.Manifest, nil)

	// With the index blob gone the descent cannot run, so a release keyed on
	// the index digest is a no-op on a digest nothing holds and the hold stays.
	// It is recoverable because the decline names the digest to release.
	if err := eng.Hold(context.Background(), lay.Root.String(), "cfs"); err != nil {
		t.Fatalf("second Hold by index digest: %v", err)
	}
	if err := os.Remove(octest.ObjectPath(storeRoot, lay.Root)); err != nil {
		t.Fatalf("remove index blob: %v", err)
	}
	if err := eng.Release(context.Background(), lay.Root.String(), "cfs"); err != nil {
		t.Fatalf("Release with the index blob gone: %v", err)
	}
	assertHolders(t, storeRoot, lay.Manifest, []string{"cfs"})
	err := eng.Remove(context.Background(), ref)
	if err == nil || !strings.Contains(err.Error(), lay.Manifest.String()) {
		t.Fatalf("decline = %v, want it to name the digest %s to release", err, lay.Manifest)
	}
	if err := eng.Release(context.Background(), lay.Manifest.String(), "cfs"); err != nil {
		t.Fatalf("Release by identity: %v", err)
	}
	assertHolders(t, storeRoot, lay.Manifest, nil)
	if err := eng.Remove(context.Background(), ref); err != nil {
		t.Fatalf("Remove after release: %v", err)
	}
}

// TestIdentityWrongSizeIndexEntryFailsBothPullPathsE2E pins the verification
// parity between a fresh pull and a cache hit: an index that declares the wrong
// size for the manifest it selects fails the pull whether or not that manifest
// is already materialized locally. The cached leg is the one that discriminates
// — the image is a valid cache hit by every other measure.
func TestIdentityWrongSizeIndexEntryFailsBothPullPathsE2E(t *testing.T) {
	lay := identityLayout(t, "wrong-size payload past the sixty-four byte inline limit\n")
	badRoot := writeWrongSizeIndex(t, lay)
	badRef := "app@" + badRoot.String()
	goodRef := "app@" + lay.Root.String()

	t.Run("cold", func(t *testing.T) {
		storeRoot := verityStoreRoot(t)
		eng := newEngine(t, storeRoot)
		_, err := eng.Pull(context.Background(), badRef, WithFromDir(lay.Dir))
		if devtest.IsVerityUnsupported(err) {
			t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), "size") {
			t.Fatalf("cold Pull error = %v, want a size mismatch", err)
		}
	})

	t.Run("cached", func(t *testing.T) {
		storeRoot := verityStoreRoot(t)
		eng := newEngine(t, storeRoot)
		if _, err := eng.Pull(context.Background(), goodRef, WithFromDir(lay.Dir)); devtest.IsVerityUnsupported(err) {
			t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
		} else if err != nil {
			t.Fatalf("Pull: %v", err)
		}
		_, err := eng.Pull(context.Background(), badRef, WithFromDir(lay.Dir))
		if err == nil || !strings.Contains(err.Error(), "size") {
			t.Fatalf("cached Pull error = %v, want a size mismatch", err)
		}
		// The failed pull left the stored image alone.
		if _, err := eng.Inspect(context.Background(), goodRef); err != nil {
			t.Fatalf("Inspect after the failed pull: %v", err)
		}
	})
}

// TestIdentityRegistryRepeatPullRefetchesNothingE2E exercises real tag
// resolution: a repeat pull of the same tag re-reads no metadata over the
// network and rewrites the same record, so an agent polling a tag pays for the
// resolution alone.
func TestIdentityRegistryRepeatPullRefetchesNothingE2E(t *testing.T) {
	storeRoot := verityStoreRoot(t)
	lay := identityLayout(t, "repeat-pull payload past the sixty-four byte inline limit\n")
	reg := octest.NewRegistry()
	t.Cleanup(reg.Close)
	counter := &countingTransport{inner: reg.Transport()}
	useRemoteDefaultTransport(t, counter)

	ref := "registry.example.com/team/repeat:latest"
	pushIndexTag(t, reg, ref, lay)
	eng := newEngine(t, storeRoot)
	if _, err := eng.Pull(context.Background(), ref); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("cold Pull: %v", err)
	}
	first := readPullRef(t, storeRoot, mustNormalizeRef(t, ref))

	counter.reset()
	if _, err := eng.Pull(context.Background(), ref); err != nil {
		t.Fatalf("repeat Pull: %v", err)
	}
	if got := counter.manifests.Load(); got != 0 {
		t.Fatalf("repeat pull fetched %d manifests, want 0 (the index and manifest are stored)", got)
	}
	if got := counter.blobs.Load(); got != 0 {
		t.Fatalf("repeat pull fetched %d blobs, want 0", got)
	}

	// pulledAt is a second-precision wall clock and is excluded by
	// construction; everything else a record carries must be reproduced.
	second := readPullRef(t, storeRoot, mustNormalizeRef(t, ref))
	first.PulledAt, second.PulledAt = "", ""
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeat pull wrote a different record:\n first  = %#v\n second = %#v", first, second)
	}
}

// TestIdentityCoexistingRefsShareOneBindingE2E: two references for one image
// that landed on different documents — a tag the registry resolves to the
// index, and a reference pinning the platform manifest — are separate records
// over one identity. Removing either must leave the binding the other still
// resolves to, which is decided by comparing what the records identify rather
// than what they resolved through: those differ here, and only the identity
// says the two references mean the same image.
func TestIdentityCoexistingRefsShareOneBindingE2E(t *testing.T) {
	storeRoot := verityStoreRoot(t)
	lay := identityLayout(t, "coexisting payload past the sixty-four byte inline limit\n")
	reg := octest.NewRegistry()
	t.Cleanup(reg.Close)
	useRemoteDefaultTransport(t, reg.Transport())

	tagRef := "registry.example.com/team/shared:latest"
	pinnedRef := "registry.example.com/team/shared@" + lay.Manifest.String()
	pushIndexTag(t, reg, tagRef, lay)
	eng := newEngine(t, storeRoot)
	if _, err := eng.Pull(context.Background(), tagRef); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull by tag: %v", err)
	}
	if _, err := eng.Pull(context.Background(), pinnedRef); err != nil {
		t.Fatalf("Pull by manifest digest: %v", err)
	}
	if bindings := storedBindings(t, storeRoot); len(bindings) != 1 || bindings[0] != lay.Manifest {
		t.Fatalf("bindings = %v, want exactly the identity %s", bindings, lay.Manifest)
	}

	// The precondition the sharing check has to survive: one image, one
	// identity, two records that resolved through different documents.
	tagRecord := readPullRef(t, storeRoot, mustNormalizeRef(t, tagRef))
	pinnedRecord := readPullRef(t, storeRoot, mustNormalizeRef(t, pinnedRef))
	if tagRecord.Resolved.Digest != lay.Root || pinnedRecord.Resolved.Digest != lay.Manifest {
		t.Fatalf("records resolved through %s and %s, want the index %s and the manifest %s",
			tagRecord.Resolved.Digest, pinnedRecord.Resolved.Digest, lay.Root, lay.Manifest)
	}
	if tagRecord.Identity.Digest != lay.Manifest || pinnedRecord.Identity.Digest != lay.Manifest {
		t.Fatalf("records identify %s and %s, want both to be %s",
			tagRecord.Identity.Digest, pinnedRecord.Identity.Digest, lay.Manifest)
	}

	// Remove the manifest-pinned reference: the surviving tag resolved through
	// the index, so only its identity marks the binding as still needed.
	if err := eng.Remove(context.Background(), pinnedRef); err != nil {
		t.Fatalf("Remove the manifest-pinned ref: %v", err)
	}
	if bindings := storedBindings(t, storeRoot); len(bindings) != 1 {
		t.Fatalf("bindings = %v, want the shared binding kept for the tag", bindings)
	}
	if _, err := eng.Inspect(context.Background(), tagRef); err != nil {
		t.Fatalf("Inspect the surviving tag: %v", err)
	}

	if err := eng.Remove(context.Background(), tagRef); err != nil {
		t.Fatalf("Remove the last ref: %v", err)
	}
	if bindings := storedBindings(t, storeRoot); len(bindings) != 0 {
		t.Fatalf("bindings = %v, want none after the last ref is removed", bindings)
	}
}

// TestIdentityColdIndexPullE2E covers what a fresh index-rooted pull leaves
// behind: the binding and the reported digest are the identity, the whole
// metadata chain including the index is in the object store, and ls reports
// the identity with the composefs image's size.
func TestIdentityColdIndexPullE2E(t *testing.T) {
	storeRoot := verityStoreRoot(t)
	lay := identityLayout(t, "cold-index payload past the sixty-four byte inline limit\n")
	useRemoteDefaultTransport(t, failTransport{})

	eng := newEngine(t, storeRoot)
	img, err := eng.Pull(context.Background(), lay.Ref, WithFromDir(lay.Dir))
	if devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	}
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if img.Digest != lay.Manifest.String() {
		t.Fatalf("pull digest = %s, want the platform manifest %s", img.Digest, lay.Manifest)
	}
	if bindings := storedBindings(t, storeRoot); len(bindings) != 1 || bindings[0] != lay.Manifest {
		t.Fatalf("bindings = %v, want exactly the identity %s", bindings, lay.Manifest)
	}
	// The index a pinned reference may name is stored, so descending it later
	// needs no source.
	assertObjectExists(t, storeRoot, lay.Root)
	assertObjectExists(t, storeRoot, lay.Manifest)
	assertObjectExists(t, storeRoot, lay.Config)

	images, err := eng.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("List = %#v, want one image", images)
	}
	if images[0].Digest != lay.Manifest.String() {
		t.Fatalf("listed digest = %s, want the identity %s", images[0].Digest, lay.Manifest)
	}
	if images[0].Size <= 0 {
		t.Fatalf("listed size = %d, want the composefs image size", images[0].Size)
	}
}

// TestIdentityReadVerbsResolveIndexDigestE2E: the read verbs accept whichever
// digest the caller holds. An index digest descends to the identity and lands
// on its binding, a digest with no local blob passes through to the verb's own
// not-found, and a recorded tag resolves from the record alone — so it still
// works once the index blob it was pulled under is gone, which a pinned index
// reference cannot.
func TestIdentityReadVerbsResolveIndexDigestE2E(t *testing.T) {
	storeRoot := verityStoreRoot(t)
	lay := identityLayout(t, "read-verbs payload past the sixty-four byte inline limit\n")
	useRemoteDefaultTransport(t, failTransport{})

	// A tag recorded against the index root, pulled entirely from local blobs.
	ref := "registry.example.com/team/read:latest"
	eng := newEngine(t, storeRoot)
	if _, err := eng.Pull(context.Background(), ref,
		WithBlobSource(layoutBlobs(t, lay.Dir)), WithResolvedRoot(lay.Root)); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	binding, err := newImageStore(t, storeRoot).Get(lay.Manifest, lay.Platform)
	if err != nil {
		t.Fatalf("imagestore.Get: %v", err)
	}

	t.Run("inspect by index digest", func(t *testing.T) {
		for _, name := range []string{lay.Root.String(), lay.Manifest.String(), ref} {
			info, err := eng.Inspect(context.Background(), name)
			if err != nil {
				t.Fatalf("Inspect(%s): %v", name, err)
			}
			if info.Root != lay.Manifest {
				t.Fatalf("Inspect(%s) root = %s, want the identity %s", name, info.Root, lay.Manifest)
			}
			if info.Composefs.Digest != binding.Descriptor.Digest {
				t.Fatalf("Inspect(%s) composefs = %s, want the identity-keyed binding %s",
					name, info.Composefs.Digest, binding.Descriptor.Digest)
			}
		}
	})

	t.Run("absent digest passes through to not-found", func(t *testing.T) {
		absent := digest.SHA256.FromString("never pulled").String()
		_, err := eng.Inspect(context.Background(), absent)
		if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "not pulled") {
			t.Fatalf("Inspect(absent) = %v, want the not-pulled ErrNotFound", err)
		}
	})

	t.Run("mount by index digest", func(t *testing.T) {
		target := filepath.Join(storeRoot, "mnt")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		err := eng.Mount(context.Background(), lay.Root.String(), target)
		if skipMountEnvironment(err) {
			t.Skipf("composefs mount environment unavailable: %v", err)
		}
		if err != nil {
			t.Fatalf("Mount by index digest: %v", err)
		}
		t.Cleanup(func() {
			if err := eng.Unmount(context.Background(), target); err != nil {
				t.Errorf("cleanup unmount: %v", err)
			}
		})
		if _, err := os.Stat(filepath.Join(target, "usr", "bin", "app")); err != nil {
			t.Fatalf("mounted rootfs: %v", err)
		}
	})

	// Destructive: leave the index blob removed for the last check only.
	t.Run("tag resolves with the index blob gone", func(t *testing.T) {
		if err := os.Remove(octest.ObjectPath(storeRoot, lay.Root)); err != nil {
			t.Fatalf("remove index blob: %v", err)
		}
		info, err := eng.Inspect(context.Background(), ref)
		if err != nil {
			t.Fatalf("Inspect by tag with the index blob gone: %v", err)
		}
		if info.Root != lay.Manifest {
			t.Fatalf("Inspect by tag root = %s, want the identity %s", info.Root, lay.Manifest)
		}
		// A pinned index reference has nothing to descend, so it reports the
		// miss its own way rather than silently resolving.
		_, err = eng.Inspect(context.Background(), "registry.example.com/team/read@"+lay.Root.String())
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Inspect by pinned index digest = %v, want ErrNotFound", err)
		}
	})
}

// TestIdentityMountChainResolvesIdentityBindingE2E resolves an index-rooted
// image by its chain identity. The scan behind MountChain enumerates records by
// the digest their image is keyed under, which is the only digest the store
// guarantees content for: a record's provenance may name an index no reference
// keeps alive, so the resolution is exercised with that index blob gone.
func TestIdentityMountChainResolvesIdentityBindingE2E(t *testing.T) {
	storeRoot := verityStoreRoot(t)
	lay := identityLayout(t, "mount-chain payload past the sixty-four byte inline limit\n")
	useRemoteDefaultTransport(t, failTransport{})

	ref := "registry.example.com/team/chain:latest"
	eng := newEngine(t, storeRoot)
	if _, err := eng.Pull(context.Background(), ref,
		WithBlobSource(layoutBlobs(t, lay.Dir)), WithResolvedRoot(lay.Root)); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	binding, err := newImageStore(t, storeRoot).Get(lay.Manifest, lay.Platform)
	if err != nil {
		t.Fatalf("imagestore.Get: %v", err)
	}
	info, err := eng.Inspect(context.Background(), ref)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	chainID := info.Layers[len(info.Layers)-1].ChainID.String()

	t.Run("mount by chain id", func(t *testing.T) {
		target := filepath.Join(storeRoot, "chain-mnt")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		err := eng.MountChain(context.Background(), chainID, target)
		if skipMountEnvironment(err) {
			t.Skipf("composefs mount environment unavailable: %v", err)
		}
		if err != nil {
			t.Fatalf("MountChain: %v", err)
		}
		t.Cleanup(func() {
			if err := eng.Unmount(context.Background(), target); err != nil {
				t.Errorf("cleanup unmount: %v", err)
			}
		})
		if _, err := os.Stat(filepath.Join(target, "usr", "bin", "app")); err != nil {
			t.Fatalf("mounted rootfs: %v", err)
		}
	})

	// The index this record resolved through is not the image's key and nothing
	// keeps it: with it gone, the chain must still resolve to the image.
	if err := os.Remove(octest.ObjectPath(storeRoot, lay.Root)); err != nil {
		t.Fatalf("remove index blob: %v", err)
	}
	resolved, err := newChainEngine(t, storeRoot).resolveChain(context.Background(), chainID)
	if err != nil {
		t.Fatalf("resolveChain with the index blob gone: %v", err)
	}
	if resolved.Root != lay.Manifest {
		t.Fatalf("resolved root = %s, want the identity %s", resolved.Root, lay.Manifest)
	}
	if resolved.Composefs.Digest != binding.Descriptor.Digest {
		t.Fatalf("resolved composefs = %s, want the identity-keyed binding %s",
			resolved.Composefs.Digest, binding.Descriptor.Digest)
	}
}

// TestIdentityForceRemoveClearsCrossDoorHoldE2E: a forced remove clears the
// holds of the image it removes, and the hold it has to find was taken through
// the other door and survived the record being rewritten by this one.
func TestIdentityForceRemoveClearsCrossDoorHoldE2E(t *testing.T) {
	storeRoot := verityStoreRoot(t)
	lay := identityLayout(t, "force-remove payload past the sixty-four byte inline limit\n")
	reg := octest.NewRegistry()
	t.Cleanup(reg.Close)
	useRemoteDefaultTransport(t, reg.Transport())

	ref := "registry.example.com/team/forced:latest"
	pushIndexTag(t, reg, ref, lay)
	eng := newEngine(t, storeRoot)

	if _, err := eng.Pull(context.Background(), ref,
		WithBlobSource(layoutBlobs(t, lay.Dir)), WithResolvedRoot(lay.Manifest)); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("staged Pull: %v", err)
	}
	if err := eng.Hold(context.Background(), lay.Manifest.String(), "cfs"); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	// The registry door rewrites the record's provenance to the index.
	if _, err := eng.Pull(context.Background(), ref); err != nil {
		t.Fatalf("registry Pull: %v", err)
	}
	if rec := readPullRef(t, storeRoot, mustNormalizeRef(t, ref)); rec.Resolved.Digest != lay.Root {
		t.Fatalf("record resolved = %s, want the index %s", rec.Resolved.Digest, lay.Root)
	}

	if err := eng.Remove(context.Background(), ref, WithForce()); err != nil {
		t.Fatalf("forced Remove: %v", err)
	}
	assertHolders(t, storeRoot, lay.Manifest, nil)
	if bindings := storedBindings(t, storeRoot); len(bindings) != 0 {
		t.Fatalf("bindings = %v, want none after a forced remove", bindings)
	}
}

// identityLayout builds a two-platform OCI layout whose root is an image index.
// The host platform is listed first, so Layout.Manifest is the manifest ocicfs
// must key the image by and Layout.Root the index a reference may name instead.
func identityLayout(t *testing.T, body string) octest.Layout {
	t.Helper()
	host := hostPlatform()
	foreign := ocispec.Platform{OS: "linux", Architecture: foreignArch(host.Architecture)}
	lay, err := octest.BuildImage(t.TempDir(),
		octest.ImageSpec{Platform: host, Layers: []octest.LayerSpec{{Files: canonicalPullFiles(body)}}},
		octest.ImageSpec{Platform: foreign, Layers: []octest.LayerSpec{{Files: canonicalPullFiles(body)}}},
	)
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	if lay.Root == lay.Manifest {
		t.Fatal("fixture must pin an index, not a manifest")
	}
	return lay
}

// pushIndexTag publishes lay's whole index — every platform manifest and blob
// it names — under tagRef, so the tag resolves to the index digest.
func pushIndexTag(t *testing.T, reg *octest.Registry, tagRef string, lay octest.Layout) {
	t.Helper()
	path, err := ggcrlayout.FromPath(lay.Dir)
	if err != nil {
		t.Fatalf("layout.FromPath: %v", err)
	}
	index, err := path.ImageIndex()
	if err != nil {
		t.Fatalf("layout.ImageIndex: %v", err)
	}
	tag, err := name.NewTag(tagRef)
	if err != nil {
		t.Fatalf("name.NewTag: %v", err)
	}
	if err := remote.WriteIndex(tag, index, remote.WithTransport(reg.Transport())); err != nil {
		t.Fatalf("remote.WriteIndex: %v", err)
	}
}

// writeWrongSizeIndex writes a copy of lay's index into its blob directory with
// the host platform's entry declaring one byte more than that manifest has, and
// returns the digest pinning it. The index itself is well formed and hashes to
// what it claims; only the size it vouches for is wrong.
func writeWrongSizeIndex(t *testing.T, lay octest.Layout) digest.Digest {
	t.Helper()
	blobs := filepath.Join(lay.Dir, "blobs", "sha256")
	raw, err := os.ReadFile(filepath.Join(blobs, lay.Root.Encoded())) // #nosec G304 -- test reads the layout it built
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var index ocispec.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatalf("parse index: %v", err)
	}
	var patched bool
	for i := range index.Manifests {
		if index.Manifests[i].Digest == lay.Manifest {
			index.Manifests[i].Size++
			patched = true
		}
	}
	if !patched {
		t.Fatalf("index does not list the host manifest %s", lay.Manifest)
	}
	raw, err = json.Marshal(index)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	root := digest.SHA256.FromBytes(raw)
	if err := os.WriteFile(filepath.Join(blobs, root.Encoded()), raw, 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	return root
}

// storedBindings returns the digests every image binding under storeRoot is
// keyed by, so a test can assert an image is stored exactly once.
func storedBindings(t *testing.T, storeRoot string) []digest.Digest {
	t.Helper()
	base := filepath.Join(storeRoot, "images", "sha256")
	var out []digest.Digest
	err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		// <aa>/<rest>/<platform>.json
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 3 {
			return fmt.Errorf("unexpected binding path %s", rel)
		}
		out = append(out, digest.NewDigestFromEncoded(digest.SHA256, parts[0]+parts[1]))
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("walk bindings: %v", err)
	}
	return out
}

// newEngine returns an engine over storeRoot.
func newEngine(t *testing.T, storeRoot string) Engine {
	t.Helper()
	eng, err := New(registryConfig(storeRoot))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return eng
}

// countingTransport tallies the registry reads a pull performs, so a test can
// require that an image already in the store costs no refetch.
type countingTransport struct {
	inner     http.RoundTripper
	blobs     atomic.Int64
	manifests atomic.Int64
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet {
		switch {
		case strings.Contains(req.URL.Path, "/blobs/"):
			c.blobs.Add(1)
		case strings.Contains(req.URL.Path, "/manifests/"):
			c.manifests.Add(1)
		}
	}
	return c.inner.RoundTrip(req)
}

func (c *countingTransport) reset() {
	c.blobs.Store(0)
	c.manifests.Store(0)
}
