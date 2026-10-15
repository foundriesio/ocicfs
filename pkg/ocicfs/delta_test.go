// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocicfs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// The two-release zstd:chunked fixture layouts, built for linux/amd64.
var (
	deltaReleaseN  = filepath.Join("..", "..", "internal", "zstdchunked", "testdata", "release-n")
	deltaReleaseN1 = filepath.Join("..", "..", "internal", "zstdchunked", "testdata", "release-n1")
)

// TestPullDeltaMatchesPlainPullE2E asserts a delta pull from a layout stores
// what a plain pull of the same image stores, is a cache hit when repeated,
// and takes a hold when asked to.
func TestPullDeltaMatchesPlainPullE2E(t *testing.T) {
	requireDeltaFixtures(t)
	deltaRoot, plainRoot := verityStoreRoot(t), verityStoreRoot(t)
	ref := deltaFixtureRef(t, deltaReleaseN1)

	delta := pullInto(t, deltaRoot, ref, WithFromDir(deltaReleaseN1), WithDelta())
	plain := pullInto(t, plainRoot, ref, WithFromDir(deltaReleaseN1))
	if delta.Digest != plain.Digest || delta.Size != plain.Size {
		t.Fatalf("delta pull = %+v, plain pull = %+v", delta, plain)
	}
	assertTreesEqual(t, plainRoot, deltaRoot, "objects")
	assertTreesEqual(t, plainRoot, deltaRoot, "images")
	assertRefsEqual(t, plainRoot, deltaRoot)

	again := pullInto(t, deltaRoot, ref, WithFromDir(deltaReleaseN1), WithDelta())
	if again.Digest != delta.Digest {
		t.Fatalf("repeated delta pull = %+v, want %+v", again, delta)
	}

	held := pullInto(t, deltaRoot, deltaFixtureRef(t, deltaReleaseN), WithFromDir(deltaReleaseN), WithDelta(), WithHold("delta-base"))
	if held.Holder != "delta-base" {
		t.Fatalf("held delta pull reports holder %q, want delta-base", held.Holder)
	}
	eng := newEngine(t, deltaRoot)
	holds, err := eng.Holds(context.Background())
	if err != nil {
		t.Fatalf("Holds: %v", err)
	}
	if got := holds[digest.Digest(held.Digest)]; len(got) != 1 || got[0] != "delta-base" {
		t.Fatalf("holds on %s = %v, want [delta-base]", held.Digest, got)
	}
}

// TestPullDeltaRequiresRangeSource asserts a delta pull refuses a source
// that cannot serve byte ranges, and releases the store afterwards.
func TestPullDeltaRequiresRangeSource(t *testing.T) {
	requireDeltaFixtures(t)
	eng, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The source is refused before the image tools are looked up, so the
	// error names the source even where mkcomposefs is absent.
	t.Setenv("PATH", t.TempDir())
	_, err = eng.Pull(context.Background(), deltaFixtureRef(t, deltaReleaseN), WithBlobSource(layoutBlobs(t, deltaReleaseN)), WithDelta())
	if !errors.Is(err, ErrNoRangeSource) {
		t.Fatalf("Pull error = %v, want ErrNoRangeSource", err)
	}
	if _, err := eng.GC(context.Background()); err != nil {
		t.Fatalf("GC after the refused pull: %v", err)
	}
}

// TestPullAppliesOptionsOnce asserts a pull evaluates each option exactly
// once, whichever path it takes: an option may have side effects of its own.
func TestPullAppliesOptionsOnce(t *testing.T) {
	eng, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, delta := range []bool{false, true} {
		calls := 0
		counting := func(o *PullOptions) {
			calls++
			o.Delta = delta
		}
		if _, err := eng.Pull(context.Background(), "not a reference", counting); err == nil {
			t.Fatal("Pull of an invalid reference succeeded")
		}
		if calls != 1 {
			t.Fatalf("delta=%v: option applied %d times, want once", delta, calls)
		}
	}
}

// TestNewBuildRejectsDelta asserts the layer-at-a-time build refuses the
// delta option before taking the store.
func TestNewBuildRejectsDelta(t *testing.T) {
	requireDeltaFixtures(t)
	eng, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = eng.NewBuild(context.Background(), deltaFixtureRef(t, deltaReleaseN), WithFromDir(deltaReleaseN), WithDelta())
	if err == nil || !strings.Contains(err.Error(), "not available through NewBuild") {
		t.Fatalf("NewBuild error = %v, want the delta refusal", err)
	}
	if _, err := eng.GC(context.Background()); err != nil {
		t.Fatalf("GC after the refused build: %v", err)
	}
}

// requireDeltaFixtures skips a test on a host the fixtures were not built for.
func requireDeltaFixtures(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skipf("the zstd:chunked fixtures are linux/amd64 images; host is %s/%s", runtime.GOOS, runtime.GOARCH)
	}
}

// deltaFixtureRef returns a digest-pinned reference to the one image of a
// fixture layout.
func deltaFixtureRef(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "index.json")) // #nosec G304 -- test fixture path
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var index ocispec.Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("decode index: %v", err)
	}
	if len(index.Manifests) != 1 {
		t.Fatalf("fixture %s holds %d images, want 1", dir, len(index.Manifests))
	}
	return "release@" + index.Manifests[0].Digest.String()
}
