// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocilayout

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/ocichain"
)

func TestOpenTarIndexesLayoutEntries(t *testing.T) {
	first := []byte("first blob")
	second := []byte("second blob, longer")
	replaced := []byte("replacement of the first blob")
	dotted := []byte("blob under a ./ prefix")
	after := []byte("blob after the sparse entries")
	index := []byte(`{"schemaVersion":2,"manifests":[]}`)
	firstDesc := blobDesc(first)
	secondDesc := blobDesc(second)
	dottedDesc := blobDesc(dotted)
	afterDesc := blobDesc(after)
	linked := blobDesc([]byte("named by a symlink and a hardlink only"))
	sparse := blobDesc([]byte("named by a sparse entry only"))

	archive := writeArchive(t, func(tw *tar.Writer, raw io.Writer) {
		addFile(t, tw, "oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`))
		addHeader(t, tw, &tar.Header{Typeflag: tar.TypeDir, Name: "blobs/", Mode: 0o755})
		addHeader(t, tw, &tar.Header{Typeflag: tar.TypeDir, Name: "blobs/sha256/", Mode: 0o755})
		addFile(t, tw, "blobs/sha256/"+firstDesc.Digest.Hex(), first)
		addFile(t, tw, "blobs/sha256/"+secondDesc.Digest.Hex(), second)
		addHeader(t, tw, &tar.Header{Typeflag: tar.TypeSymlink, Name: "blobs/sha256/" + linked.Digest.Hex(), Linkname: "/etc/hostname"})
		addHeader(t, tw, &tar.Header{Typeflag: tar.TypeLink, Name: "blobs/sha256/" + linked.Digest.Hex(), Linkname: "blobs/sha256/" + firstDesc.Digest.Hex()})
		addHeader(t, tw, &tar.Header{Typeflag: tar.TypeChar, Name: "blobs/sha256/" + linked.Digest.Hex(), Devmajor: 1, Devminor: 3})
		addHeader(t, tw, &tar.Header{Typeflag: tar.TypeFifo, Name: "blobs/sha256/" + linked.Digest.Hex()})
		addFile(t, tw, "other/notes.txt", []byte("not part of the layout"))
		addFile(t, tw, "blobs/sha256/not-a-digest", []byte("malformed blob name"))
		addFile(t, tw, "./blobs/sha256/"+dottedDesc.Digest.Hex(), dotted)
		addFile(t, tw, "blobs/sha256/"+firstDesc.Digest.Hex(), replaced)
		addFile(t, tw, "index.json", index)
		addFile(t, tw, "manifest.json", []byte(`[]`))
		addFile(t, tw, "/index.json", []byte("absolute name"))
		addFile(t, tw, "../index.json", []byte("parent-relative name"))
		addSparseFile(t, tw, raw, "index.json", []byte("sparse fragment"))
		addSparseFile(t, tw, raw, "blobs/sha256/"+sparse.Digest.Hex(), []byte("sparse fragment"))
		addFile(t, tw, "blobs/sha256/"+afterDesc.Digest.Hex(), after)
	})
	src := openTar(t, archive)

	// The last entry of a name wins, as it does on extraction.
	for _, c := range []struct {
		desc ocispec.Descriptor
		want []byte
	}{{firstDesc, replaced}, {secondDesc, second}, {dottedDesc, dotted}, {afterDesc, after}} {
		if got := readBlob(t, src, c.desc); !bytes.Equal(got, c.want) {
			t.Errorf("Open(%s) = %q, want %q", c.desc.Digest, got, c.want)
		}
	}
	got, err := src.IndexJSON()
	if err != nil {
		t.Fatalf("IndexJSON: %v", err)
	}
	if !bytes.Equal(got, index) {
		t.Errorf("IndexJSON = %q, want %q", got, index)
	}

	// Only regular files are blobs: the links, device and FIFO named like
	// one are not served, and neither is a sparse file, whose bytes are not
	// contiguous in the archive. The sparse index.json after the real one
	// did not replace it either.
	if _, err := src.Open(context.Background(), linked); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Open of a non-regular entry: error = %v, want os.ErrNotExist", err)
	}
	if _, err := src.Open(context.Background(), sparse); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Open of a sparse entry: error = %v, want os.ErrNotExist", err)
	}
	if _, err := src.Open(context.Background(), blobDesc([]byte("never archived"))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Open of an absent blob: error = %v, want os.ErrNotExist", err)
	}
	sha512 := ocispec.Descriptor{Digest: digest.Digest("sha512:" + strings.Repeat("a", 128)), Size: 4}
	if _, err := src.Open(context.Background(), sha512); err == nil || !strings.Contains(err.Error(), "non-sha256") {
		t.Errorf("Open of a sha512 descriptor: error = %v, want non-sha256 rejection", err)
	}

	// An entry shorter than its descriptor is served as it is; the verifying
	// reader downstream reports the mismatch.
	short := secondDesc
	short.Size++
	rc, err := ocichain.OpenBlob(context.Background(), src, short)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	defer closeReader(t, rc)
	if _, err := io.ReadAll(rc); err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Errorf("read of a short entry: error = %v, want a size mismatch", err)
	}
}

func TestOpenTarRejectsCompressed(t *testing.T) {
	for _, c := range []struct {
		kind  string
		magic []byte
	}{
		{"gzip", []byte{0x1f, 0x8b, 0x08}},
		{"zstd", []byte{0x28, 0xb5, 0x2f, 0xfd}},
		{"xz", []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}},
		{"bzip2", []byte("BZh9")},
	} {
		t.Run(c.kind, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "image.tar")
			if err := os.WriteFile(p, append(c.magic, []byte("rest of the stream")...), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := OpenTar(p)
			if err == nil || !strings.Contains(err.Error(), "decompress it first") || !strings.Contains(err.Error(), c.kind) {
				t.Fatalf("OpenTar error = %v, want a %s decompress-first rejection", err, c.kind)
			}
		})
	}

	t.Run("not a tar", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "image.tar")
		if err := os.WriteFile(p, bytes.Repeat([]byte("not a tar archive\n"), 64), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenTar(p); !errors.Is(err, tar.ErrHeader) {
			t.Fatalf("OpenTar error = %v, want tar.ErrHeader", err)
		}
	})
}

func TestOpenTarRange(t *testing.T) {
	blob := []byte("0123456789abcdefghij")
	desc := blobDesc(blob)
	src := openTar(t, writeArchive(t, func(tw *tar.Writer, _ io.Writer) {
		addFile(t, tw, "padding", bytes.Repeat([]byte("x"), 700))
		addFile(t, tw, "blobs/sha256/"+desc.Digest.Hex(), blob)
	}))

	for _, r := range []struct{ off, n int64 }{{3, 5}, {0, 20}, {19, 1}, {7, 0}, {20, 0}} {
		rc, whole, err := src.OpenRange(context.Background(), desc, r.off, r.n)
		if err != nil {
			t.Fatalf("OpenRange(%d, %d): %v", r.off, r.n, err)
		}
		if whole {
			t.Errorf("OpenRange(%d, %d) reported whole", r.off, r.n)
		}
		got, err := io.ReadAll(rc)
		closeReader(t, rc)
		if err != nil {
			t.Fatalf("read range (%d, %d): %v", r.off, r.n, err)
		}
		if want := blob[r.off : r.off+r.n]; !bytes.Equal(got, want) {
			t.Errorf("OpenRange(%d, %d) = %q, want %q", r.off, r.n, got, want)
		}
	}

	for _, r := range []struct{ off, n int64 }{{18, 3}, {21, 0}, {0, 21}, {-1, 2}, {2, -1}, {1, 1<<63 - 1}} {
		if _, _, err := src.OpenRange(context.Background(), desc, r.off, r.n); err == nil {
			t.Errorf("OpenRange(%d, %d) = nil error, want rejection", r.off, r.n)
		}
	}
	// The descriptor bounds the range, not the entry, and the entry must hold
	// what the descriptor allows.
	short := desc
	short.Size = 4
	if _, _, err := src.OpenRange(context.Background(), short, 5, 1); err == nil {
		t.Error("OpenRange past the descriptor size succeeded")
	}
	long := desc
	long.Size = 40
	if _, _, err := src.OpenRange(context.Background(), long, 25, 1); err == nil {
		t.Error("OpenRange past the entry end succeeded")
	}

	ctx, cancel := context.WithCancel(context.Background())
	rc, _, err := src.OpenRange(ctx, desc, 2, 6)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	defer closeReader(t, rc)
	cancel()
	if _, err := io.ReadAll(rc); !errors.Is(err, context.Canceled) {
		t.Errorf("read error = %v, want context.Canceled", err)
	}
	if _, _, err := src.OpenRange(ctx, desc, 2, 6); !errors.Is(err, context.Canceled) {
		t.Errorf("OpenRange error = %v, want context.Canceled at open", err)
	}
	if _, err := src.Open(ctx, desc); !errors.Is(err, context.Canceled) {
		t.Errorf("Open error = %v, want context.Canceled at open", err)
	}
}

func TestOpenTarDescribesMissingRoot(t *testing.T) {
	layers := []ocispec.Descriptor{fakeLayerDesc("a")}
	cfg := configJSON("linux", "amd64", "", diffIDsFor(layers))
	cfgDesc := blobDesc(cfg)
	man := manifestJSON(cfgDesc, layers)
	manDesc := blobDesc(man)
	manDesc.Platform = &ocispec.Platform{OS: "linux", Architecture: "amd64"}
	src := openTar(t, writeArchive(t, func(tw *tar.Writer, _ io.Writer) {
		addFile(t, tw, "blobs/sha256/"+cfgDesc.Digest.Hex(), cfg)
		addFile(t, tw, "blobs/sha256/"+manDesc.Digest.Hex(), man)
		addFile(t, tw, "index.json", indexJSON([]ocispec.Descriptor{manDesc}))
	}))

	missing := digest.SHA256.FromString("dropped-index-digest")
	_, err := ocichain.Resolve(context.Background(), src, missing, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil {
		t.Fatal("expected error for a root digest absent from the archive")
	}
	msg := err.Error()
	if !strings.Contains(msg, "not present in the layout") {
		t.Errorf("error should explain the digest is absent: %v", err)
	}
	if !strings.Contains(msg, manDesc.Digest.String()) {
		t.Errorf("error should list the manifest in index.json (%s): %v", manDesc.Digest, err)
	}
	if !strings.Contains(msg, "linux/amd64") {
		t.Errorf("error should annotate the listed manifest's platform: %v", err)
	}

	// Without an index.json there is nothing to list.
	bare := openTar(t, writeArchive(t, func(tw *tar.Writer, _ io.Writer) {
		addFile(t, tw, "blobs/sha256/"+cfgDesc.Digest.Hex(), cfg)
	}))
	if _, err := ocichain.ReadRootBlob(context.Background(), bare, missing); err == nil || !strings.Contains(err.Error(), "no index.json") {
		t.Errorf("error = %v, want the missing index.json named", err)
	}
}

// writeArchive writes a tar under a temp directory with the entries build adds
// and returns its path. build gets the archive file as raw for entries the
// tar writer cannot emit.
func writeArchive(t *testing.T, build func(tw *tar.Writer, raw io.Writer)) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "image.tar")
	f, err := os.Create(p) // #nosec G304 -- temp path of this test
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	build(tw, f)
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}
	return p
}

func addFile(t *testing.T, tw *tar.Writer, name string, body []byte) {
	t.Helper()
	addHeader(t, tw, &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(body))})
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func addHeader(t *testing.T, tw *tar.Writer, hdr *tar.Header) {
	t.Helper()
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write header %s: %v", hdr.Name, err)
	}
}

// addSparseFile adds a regular file described by a GNU sparse map in PAX
// format 0.1: a hand-built extended header block written to raw, since the
// tar writer does not emit sparse records, then the entry itself holding only
// fragment, which the map places at offset 100 of a 1000-byte file.
func addSparseFile(t *testing.T, tw *tar.Writer, raw io.Writer, name string, fragment []byte) {
	t.Helper()
	if err := tw.Flush(); err != nil {
		t.Fatalf("flush tar writer: %v", err)
	}
	records := paxRecord("GNU.sparse.major", "0") +
		paxRecord("GNU.sparse.minor", "1") +
		paxRecord("GNU.sparse.numblocks", "1") +
		paxRecord("GNU.sparse.map", "100,"+strconv.Itoa(len(fragment))) +
		paxRecord("GNU.sparse.size", "1000")
	var blk [512]byte
	copy(blk[0:], "PaxHeaders/sparse")
	copy(blk[100:], "0000644\x00")
	copy(blk[108:], "0000000\x00")
	copy(blk[116:], "0000000\x00")
	copy(blk[124:], fmt.Sprintf("%011o\x00", len(records)))
	copy(blk[136:], "00000000000\x00")
	copy(blk[148:], "        ")
	blk[156] = tar.TypeXHeader
	copy(blk[257:], "ustar\x0000")
	sum := 0
	for _, b := range blk {
		sum += int(b)
	}
	copy(blk[148:], fmt.Sprintf("%06o\x00 ", sum))
	body := append([]byte(records), make([]byte, (512-len(records)%512)%512)...)
	if _, err := raw.Write(append(blk[:], body...)); err != nil {
		t.Fatalf("write sparse header: %v", err)
	}
	addFile(t, tw, name, fragment)
}

// paxRecord formats one PAX record: its length in decimal, counting itself,
// then " key=value\n".
func paxRecord(k, v string) string {
	size := len(k) + len(v) + 3
	size += len(strconv.Itoa(size))
	rec := strconv.Itoa(size) + " " + k + "=" + v + "\n"
	if len(rec) != size {
		rec = strconv.Itoa(len(rec)) + " " + k + "=" + v + "\n"
	}
	return rec
}

func openTar(t *testing.T, p string) *Tar {
	t.Helper()
	src, err := OpenTar(p)
	if err != nil {
		t.Fatalf("OpenTar: %v", err)
	}
	t.Cleanup(func() { closeReader(t, src) })
	return src
}

func readBlob(t *testing.T, src ocichain.BlobSource, desc ocispec.Descriptor) []byte {
	t.Helper()
	rc, err := src.Open(context.Background(), desc)
	if err != nil {
		t.Fatalf("Open(%s): %v", desc.Digest, err)
	}
	defer closeReader(t, rc)
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", desc.Digest, err)
	}
	return got
}

func blobDesc(data []byte) ocispec.Descriptor {
	return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayer, Digest: digest.SHA256.FromBytes(data), Size: int64(len(data))}
}
