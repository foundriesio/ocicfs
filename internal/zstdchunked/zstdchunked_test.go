// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package zstdchunked

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestPosition(t *testing.T) {
	valid := map[string]string{
		ManifestPositionKey: "8:20:30:1",
		ManifestChecksumKey: digest.FromString("toc").String(),
	}

	tests := []struct {
		name        string
		annotations map[string]string
		blobSize    int64
		wantFound   bool
		wantErr     bool
	}{
		{name: "absent", annotations: nil, blobSize: 100},
		{name: "valid", annotations: valid, blobSize: 28, wantFound: true},
		{name: "only position", annotations: map[string]string{ManifestPositionKey: valid[ManifestPositionKey]}, blobSize: 100, wantErr: true},
		{name: "only checksum", annotations: map[string]string{ManifestChecksumKey: valid[ManifestChecksumKey]}, blobSize: 100, wantErr: true},
		{name: "too few fields", annotations: manifestAnnotations("8:20:30", valid[ManifestChecksumKey]), blobSize: 100, wantErr: true},
		{name: "too many fields", annotations: manifestAnnotations("8:20:30:1:0", valid[ManifestChecksumKey]), blobSize: 100, wantErr: true},
		{name: "not decimal", annotations: manifestAnnotations("8:x:30:1", valid[ManifestChecksumKey]), blobSize: 100, wantErr: true},
		{name: "integer overflow", annotations: manifestAnnotations("9223372036854775808:1:1:1", valid[ManifestChecksumKey]), blobSize: 100, wantErr: true},
		{name: "negative offset", annotations: manifestAnnotations("-1:20:30:1", valid[ManifestChecksumKey]), blobSize: 100, wantErr: true},
		{name: "offset before frame header", annotations: manifestAnnotations("7:20:30:1", valid[ManifestChecksumKey]), blobSize: 100, wantErr: true},
		{name: "negative compressed length", annotations: manifestAnnotations("8:-1:30:1", valid[ManifestChecksumKey]), blobSize: 100, wantErr: true},
		{name: "negative uncompressed length", annotations: manifestAnnotations("8:20:-1:1", valid[ManifestChecksumKey]), blobSize: 100, wantErr: true},
		{name: "wrong type", annotations: manifestAnnotations("8:20:30:2", valid[ManifestChecksumKey]), blobSize: 100, wantErr: true},
		{name: "compressed too large", annotations: manifestAnnotations(fmt.Sprintf("8:%d:30:1", maxTOCBytes+1), valid[ManifestChecksumKey]), blobSize: maxTOCBytes + 100, wantErr: true},
		{name: "uncompressed too large", annotations: manifestAnnotations(fmt.Sprintf("8:20:%d:1", maxTOCBytes+1), valid[ManifestChecksumKey]), blobSize: 100, wantErr: true},
		{name: "offset beyond blob", annotations: manifestAnnotations("101:0:30:1", valid[ManifestChecksumKey]), blobSize: 100, wantErr: true},
		{name: "manifest beyond blob", annotations: manifestAnnotations("90:20:30:1", valid[ManifestChecksumKey]), blobSize: 100, wantErr: true},
		{name: "negative blob size", annotations: valid, blobSize: -1, wantErr: true},
		{name: "wrong checksum algorithm", annotations: manifestAnnotations("8:20:30:1", "sha512:"+strings.Repeat("0", 128)), blobSize: 100, wantErr: true},
		{name: "wrong checksum length", annotations: manifestAnnotations("8:20:30:1", "sha256:00"), blobSize: 100, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, found, err := Position(tt.annotations, tt.blobSize)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Position() error = %v, want error %v", err, tt.wantErr)
			}
			if found != tt.wantFound {
				t.Fatalf("Position() found = %v, want %v", found, tt.wantFound)
			}
			if tt.name == "valid" {
				want := Pos{Offset: 8, CompressedLen: 20, UncompressedLen: 30, Checksum: digest.FromString("toc")}
				if got != want {
					t.Fatalf("Position() = %+v, want %+v", got, want)
				}
			}
		})
	}
}

func TestFixtureLayers(t *testing.T) {
	layouts := []string{"layout", "release-n", "release-n1"}
	for _, layout := range layouts {
		t.Run(layout, func(t *testing.T) {
			t.Parallel()
			layoutDir := filepath.Join("testdata", layout)
			for _, ref := range layoutRefs(t, layoutDir) {
				if ref == "mixed" {
					continue
				}
				for i := 0; ; i++ {
					desc, blob, ok := tryLoadLayer(t, layoutDir, ref, i)
					if !ok {
						break
					}
					pos, found, err := Position(desc.Annotations, int64(len(blob)))
					if err != nil {
						t.Fatalf("Position(%s layer %d): %v", ref, i, err)
					}
					if !found {
						t.Fatalf("Position(%s layer %d) did not find TOC", ref, i)
					}
					toc, err := Parse(blob[pos.Offset:pos.Offset+pos.CompressedLen], pos)
					if err != nil {
						t.Fatalf("Parse(%s layer %d): %v", ref, i, err)
					}
					assertReconstructs(t, blob, toc)
				}
			}
		})
	}
}

func TestBasicMetadata(t *testing.T) {
	desc, blob := loadLayer(t, filepath.Join("testdata", "layout"), "basic", 0)
	pos, found, err := Position(desc.Annotations, int64(len(blob)))
	if err != nil || !found {
		t.Fatalf("Position() = _, %v, %v", found, err)
	}
	toc, err := Parse(blob[pos.Offset:pos.Offset+pos.CompressedLen], pos)
	if err != nil {
		t.Fatal(err)
	}

	capEntry := findEntry(t, toc, "usr/bin/cap")
	capability := []byte(capEntry.Xattrs["security.capability"])
	if len(capability) != 20 || !bytes.Equal(capability[:4], []byte{0x01, 0x00, 0x00, 0x02}) {
		t.Fatalf("security.capability = %x", capability)
	}
	if got := capEntry.Xattrs["user.note"]; got != "kept in the record, dropped at dump" {
		t.Fatalf("user.note = %q", got)
	}

	assertTime(t, findEntry(t, toc, "etc/subsecond").ModTime, 1_700_000_000, 500_000_000)
	assertTime(t, findEntry(t, toc, "etc/zero-mtime").ModTime, 0, 0)
	assertTime(t, findEntry(t, toc, "etc/pre-epoch").ModTime, -5, 0)

	char := findEntry(t, toc, "dev/null")
	if char.Type != TypeChar || char.Devmajor != 1 || char.Devminor != 3 {
		t.Fatalf("dev/null = %+v", char)
	}
	block := findEntry(t, toc, "dev/loop0")
	if block.Type != TypeBlock || block.Devmajor != 7 || block.Devminor != 0 {
		t.Fatalf("dev/loop0 = %+v", block)
	}
	if fifo := findEntry(t, toc, "dev/fifo"); fifo.Type != TypeFifo {
		t.Fatalf("dev/fifo type = %q", fifo.Type)
	}
	if symlink := findEntry(t, toc, "etc/link"); symlink.Type != TypeSymlink || symlink.Linkname != "inline" {
		t.Fatalf("etc/link = %+v", symlink)
	}
	hardlink := findEntry(t, toc, "etc/hardlink")
	if hardlink.Type != TypeHardlink || hardlink.Linkname != "usr/bin/tool" || hardlink.Digest != "" {
		t.Fatalf("etc/hardlink = %+v", hardlink)
	}
	empty := findEntry(t, toc, "etc/empty")
	if empty.Type != TypeReg || empty.Size != 0 || empty.Digest != "" {
		t.Fatalf("etc/empty = %+v", empty)
	}
	zeros := findEntry(t, toc, "usr/lib/zeros.bin")
	if zeros.Size != 11_000 || zeros.Digest == "" {
		t.Fatalf("usr/lib/zeros.bin = %+v", zeros)
	}
	for name, size := range map[string]int64{"etc/inline": 4, "etc/b63": 63, "etc/b64": 64, "etc/b65": 65} {
		entry := findEntry(t, toc, name)
		if entry.Size != size || entry.Digest == "" {
			t.Errorf("%s = size %d, digest %q; want size %d and a digest", name, entry.Size, entry.Digest, size)
		}
	}
}

func TestFixtureOrder(t *testing.T) {
	tests := []struct {
		ref  string
		want []string
	}{
		{ref: "child-before-parent", want: []string{"./", "usr/", "dir/x", "dir/"}},
		{ref: "hardlink-before-target", want: []string{"./", "usr/", "hl", "a"}},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			t.Parallel()
			desc, blob := loadLayer(t, filepath.Join("testdata", "layout"), tt.ref, 0)
			pos, found, err := Position(desc.Annotations, int64(len(blob)))
			if err != nil || !found {
				t.Fatalf("Position() = _, %v, %v", found, err)
			}
			toc, err := Parse(blob[pos.Offset:pos.Offset+pos.CompressedLen], pos)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, len(toc.Entries))
			for i := range toc.Entries {
				got[i] = toc.Entries[i].Name
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("entry order = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMixedLayers(t *testing.T) {
	layout := filepath.Join("testdata", "layout")
	for i, want := range []bool{true, false} {
		desc, blob := loadLayer(t, layout, "mixed", i)
		_, found, err := Position(desc.Annotations, int64(len(blob)))
		if err != nil {
			t.Fatalf("layer %d: %v", i, err)
		}
		if found != want {
			t.Fatalf("layer %d found = %v, want %v", i, found, want)
		}
	}
}

func TestParseRejectsInvalidEntries(t *testing.T) {
	desc, blob := loadLayer(t, filepath.Join("testdata", "layout"), "basic", 0)
	pos, found, err := Position(desc.Annotations, int64(len(blob)))
	if err != nil || !found {
		t.Fatalf("Position() = _, %v, %v", found, err)
	}

	overflow := json.Number("9223372036854775808")
	tests := []struct {
		name   string
		mutate func(*testing.T, []map[string]any) []map[string]any
	}{
		{name: "missing file digest", mutate: mutateNamed("usr/bin/tool", func(e map[string]any) { delete(e, "digest") })},
		{name: "digest algorithm", mutate: mutateNamed("usr/bin/tool", func(e map[string]any) { e["digest"] = "sha512:" + strings.Repeat("0", 128) })},
		{name: "digest length", mutate: mutateNamed("usr/bin/tool", func(e map[string]any) { e["digest"] = "sha256:00" })},
		{name: "negative offset", mutate: mutateNamed("usr/bin/tool", func(e map[string]any) { e["offset"] = -1 })},
		{name: "overflowing offset", mutate: mutateNamed("usr/bin/tool", func(e map[string]any) { e["offset"] = overflow })},
		{name: "reversed offset", mutate: mutateNamed("etc/empty", func(e map[string]any) { e["offset"] = 1 })},
		{name: "negative size", mutate: mutateNamed("usr/bin/tool", func(e map[string]any) { e["size"] = -1 })},
		{name: "overflowing size", mutate: mutateNamed("usr/bin/tool", func(e map[string]any) { e["size"] = overflow })},
		{name: "negative uid", mutate: mutateNamed("usr/bin/tool", func(e map[string]any) { e["uid"] = -1 })},
		{name: "overflowing uid", mutate: mutateNamed("usr/bin/tool", func(e map[string]any) { e["uid"] = overflow })},
		{name: "negative gid", mutate: mutateNamed("usr/bin/tool", func(e map[string]any) { e["gid"] = -1 })},
		{name: "overflowing gid", mutate: mutateNamed("usr/bin/tool", func(e map[string]any) { e["gid"] = overflow })},
		{name: "negative device major", mutate: mutateNamed("dev/null", func(e map[string]any) { e["devMajor"] = -1 })},
		{name: "overflowing device major", mutate: mutateNamed("dev/null", func(e map[string]any) { e["devMajor"] = overflow })},
		{name: "negative device minor", mutate: mutateNamed("dev/null", func(e map[string]any) { e["devMinor"] = -1 })},
		{name: "overflowing device minor", mutate: mutateNamed("dev/null", func(e map[string]any) { e["devMinor"] = overflow })},
		{name: "invalid xattr", mutate: mutateNamed("usr/bin/cap", func(e map[string]any) { e["xattrs"].(map[string]any)["user.note"] = "!" })},
		{name: "unknown entry type", mutate: mutateNamed("dev/fifo", func(e map[string]any) { e["type"] = "socket" })},
		{name: "overlapping files", mutate: func(t *testing.T, entries []map[string]any) []map[string]any {
			first := firstNamed(t, entries, "usr/bin/tool")
			second := firstNamed(t, entries, "usr/bin/cap")
			second["offset"] = first["endOffset"].(json.Number)
			second["offset"] = json.Number(fmt.Sprint(mustInt64(t, second["offset"]) - 1))
			return entries
		}},
		{name: "file past TOC", mutate: mutateNamed("run/pid", func(e map[string]any) { e["endOffset"] = pos.Offset - 7 })},
		{name: "empty file byte range", mutate: mutateNamed("usr/bin/tool", func(e map[string]any) { e["endOffset"] = e["offset"] })},
		{name: "symlink names byte range", mutate: mutateNamed("etc/link", func(e map[string]any) { e["offset"], e["endOffset"] = 120, 190 })},
		{name: "empty file names byte range", mutate: mutateNamed("etc/empty", func(e map[string]any) { e["offset"], e["endOffset"] = 120, 190 })},
		{name: "directory names byte range", mutate: mutateNamed("usr/", func(e map[string]any) { e["offset"], e["endOffset"] = 120, 190 })},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			newBlob, newPos := mutateTOC(t, blob, pos, tt.mutate)
			if _, err := Parse(newBlob[newPos.Offset:newPos.Offset+newPos.CompressedLen], newPos); err == nil {
				t.Fatal("Parse() succeeded")
			}
		})
	}
}

func TestParseIgnoresChunkRecords(t *testing.T) {
	desc, blob := loadLayer(t, filepath.Join("testdata", "layout"), "basic", 0)
	pos, found, err := Position(desc.Annotations, int64(len(blob)))
	if err != nil || !found {
		t.Fatalf("Position() = _, %v, %v", found, err)
	}
	want, err := Parse(blob[pos.Offset:pos.Offset+pos.CompressedLen], pos)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*testing.T, []map[string]any) []map[string]any
	}{
		{name: "invalid chunk record", mutate: mutateType(typeChunk, func(e map[string]any) {
			e["name"], e["offset"], e["chunkOffset"], e["chunkDigest"] = "other", -1, -1, "sha256:00"
		})},
		{name: "invalid first chunk", mutate: mutateNamed("usr/lib/zeros.bin", func(e map[string]any) {
			e["chunkSize"], e["chunkType"] = -1, "sparse"
		})},
		{name: "no chunk records", mutate: func(_ *testing.T, entries []map[string]any) []map[string]any {
			result := entries[:0]
			for _, entry := range entries {
				if entry["type"] != typeChunk {
					result = append(result, entry)
				}
			}
			return result
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			newBlob, newPos := mutateTOC(t, blob, pos, tt.mutate)
			got, err := Parse(newBlob[newPos.Offset:newPos.Offset+newPos.CompressedLen], newPos)
			if err != nil {
				t.Fatalf("Parse(): %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatal("Parse() differs from the original manifest")
			}
		})
	}
}

func TestParseAcceptsDuplicateNames(t *testing.T) {
	desc, blob := loadLayer(t, filepath.Join("testdata", "layout"), "basic", 0)
	pos, found, err := Position(desc.Annotations, int64(len(blob)))
	if err != nil || !found {
		t.Fatalf("Position() = _, %v, %v", found, err)
	}
	newBlob, newPos := mutateTOC(t, blob, pos, func(t *testing.T, entries []map[string]any) []map[string]any {
		duplicate := cloneMap(firstNamed(t, entries, "etc/"))
		entries = append(entries, duplicate)
		return entries
	})
	toc, err := Parse(newBlob[newPos.Offset:newPos.Offset+newPos.CompressedLen], newPos)
	if err != nil {
		t.Fatalf("Parse(): %v", err)
	}
	var count int
	for _, entry := range toc.Entries {
		if entry.Name == "etc/" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("found %d entries named etc/, want 2", count)
	}
}

func TestParseAcceptsOptionalMetadata(t *testing.T) {
	desc, blob := loadLayer(t, filepath.Join("testdata", "layout"), "basic", 0)
	pos, found, err := Position(desc.Annotations, int64(len(blob)))
	if err != nil || !found {
		t.Fatalf("Position() = _, %v, %v", found, err)
	}

	tests := []struct {
		name   string
		mutate func(*testing.T, []map[string]any) []map[string]any
		check  func(*testing.T, *TOC)
	}{
		{
			name: "empty file digest",
			mutate: mutateNamed("etc/empty", func(e map[string]any) {
				e["digest"] = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
			}),
			check: func(t *testing.T, toc *TOC) {
				if got := findEntry(t, toc, "etc/empty").Digest; got != "" {
					t.Fatalf("Digest = %s, want empty", got)
				}
			},
		},
		{
			// The writer copies a hardlink's size from the tar header.
			name:   "hardlink with size",
			mutate: mutateNamed("etc/hardlink", func(e map[string]any) { e["size"] = 77 }),
			check: func(t *testing.T, toc *TOC) {
				if got := findEntry(t, toc, "etc/hardlink"); got.Size != 77 || got.EndOffset != 0 {
					t.Fatalf("Size, EndOffset = %d, %d, want 77, 0", got.Size, got.EndOffset)
				}
			},
		},
		{
			name:   "file at offset zero",
			mutate: mutateNamed("usr/bin/tool", func(e map[string]any) { e["offset"] = 0 }),
			check: func(t *testing.T, toc *TOC) {
				if got := findEntry(t, toc, "usr/bin/tool").Offset; got != 0 {
					t.Fatalf("Offset = %d, want 0", got)
				}
			},
		},
		{
			name:   "missing modtime",
			mutate: mutateNamed("usr/bin/tool", func(e map[string]any) { delete(e, "modtime") }),
			check: func(t *testing.T, toc *TOC) {
				if got := findEntry(t, toc, "usr/bin/tool").ModTime; got != nil {
					t.Fatalf("ModTime = %v, want nil", got)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			newBlob, newPos := mutateTOC(t, blob, pos, tt.mutate)
			toc, err := Parse(newBlob[newPos.Offset:newPos.Offset+newPos.CompressedLen], newPos)
			if err != nil {
				t.Fatalf("Parse(): %v", err)
			}
			tt.check(t, toc)
		})
	}
}

func TestParseRejectsEnvelopeErrors(t *testing.T) {
	desc, blob := loadLayer(t, filepath.Join("testdata", "layout"), "basic", 0)
	pos, found, err := Position(desc.Annotations, int64(len(blob)))
	if err != nil || !found {
		t.Fatalf("Position() = _, %v, %v", found, err)
	}
	compressed := blob[pos.Offset : pos.Offset+pos.CompressedLen]

	tests := []struct {
		name string
		data []byte
		pos  Pos
	}{
		{name: "checksum mismatch", data: compressed, pos: withChecksum(pos, digest.FromString("wrong"))},
		{name: "compressed length short", data: compressed[:len(compressed)-1], pos: pos},
		{name: "compressed length long", data: append(append([]byte(nil), compressed...), 0), pos: pos},
		{name: "uncompressed length short", data: compressed, pos: withUncompressedLen(pos, pos.UncompressedLen-1)},
		{name: "uncompressed length long", data: compressed, pos: withUncompressedLen(pos, pos.UncompressedLen+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse(tt.data, tt.pos); err == nil {
				t.Fatal("Parse() succeeded")
			}
		})
	}
}

func TestParseRejectsJSONEnvelope(t *testing.T) {
	desc, blob := loadLayer(t, filepath.Join("testdata", "layout"), "basic", 0)
	pos, found, err := Position(desc.Annotations, int64(len(blob)))
	if err != nil || !found {
		t.Fatalf("Position() = _, %v, %v", found, err)
	}

	tests := []struct {
		name   string
		mutate func(map[string]any) []byte
	}{
		{name: "version", mutate: func(doc map[string]any) []byte {
			doc["version"] = 2
			return marshalJSON(t, doc)
		}},
		{name: "trailing object", mutate: func(doc map[string]any) []byte {
			return append(marshalJSON(t, doc), []byte(` {}`)...)
		}},
		{name: "trailing garbage", mutate: func(doc map[string]any) []byte {
			return append(marshalJSON(t, doc), []byte(` x`)...)
		}},
		{name: "no version", mutate: func(doc map[string]any) []byte {
			delete(doc, "version")
			return marshalJSON(t, doc)
		}},
		{name: "repeated version", mutate: func(map[string]any) []byte {
			return []byte(`{"version":1,"version":1,"entries":[]}`)
		}},
		{name: "repeated entries", mutate: func(map[string]any) []byte {
			return []byte(`{"version":1,"entries":[],"entries":[]}`)
		}},
		{name: "entries not an array", mutate: func(doc map[string]any) []byte {
			doc["entries"] = map[string]any{}
			return marshalJSON(t, doc)
		}},
		// An element must not inherit the fields of the one before it.
		{name: "empty entry after a valid one", mutate: func(map[string]any) []byte {
			return []byte(`{"version":1,"entries":[{"type":"dir","name":"a/"},{}]}`)
		}},
		{name: "null entry", mutate: func(map[string]any) []byte {
			return []byte(`{"version":1,"entries":[{"type":"dir","name":"a/"},null]}`)
		}},
		{name: "number entry", mutate: func(map[string]any) []byte {
			return []byte(`{"version":1,"entries":[0]}`)
		}},
		{name: "unclosed entries", mutate: func(map[string]any) []byte {
			return []byte(`{"version":1,"entries":[{"type":"dir","name":"a/"}}`)
		}},
		{name: "not an object", mutate: func(map[string]any) []byte {
			return []byte(`[]`)
		}},
		{name: "too many entries", mutate: func(map[string]any) []byte {
			return tocOf(`{"type":"dir"}`, maxEntries+1)
		}},
		// Memory must not grow with elements that are rejected one by one.
		{name: "too many empty entries", mutate: func(map[string]any) []byte {
			return tocOf(`{}`, maxEntries+1)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			doc := decodeDocument(t, blob, pos)
			newBlob, newPos := rewriteTOC(t, blob, pos, tt.mutate(doc))
			if _, err := Parse(newBlob[newPos.Offset:newPos.Offset+newPos.CompressedLen], newPos); err == nil {
				t.Fatal("Parse() succeeded")
			}
		})
	}
}

func TestParseAcceptsEnvelope(t *testing.T) {
	desc, blob := loadLayer(t, filepath.Join("testdata", "layout"), "basic", 0)
	pos, found, err := Position(desc.Annotations, int64(len(blob)))
	if err != nil || !found {
		t.Fatalf("Position() = _, %v, %v", found, err)
	}

	tests := []struct {
		name string
		data []byte
		want int
	}{
		// The writer marshals a layer without entries as null.
		{name: "null entries", data: []byte(`{"version":1,"entries":null}`), want: 0},
		{name: "no entries", data: []byte(`{"version":1}`), want: 0},
		{name: "entries before version", data: []byte(`{"entries":[{"type":"dir","name":"a/"}],"version":1}`), want: 1},
		{name: "unknown key", data: []byte(`{"version":1,"future":{"entries":[1,2]},"entries":[{"type":"dir","name":"a/"}]}`), want: 1},
		{name: "entry cap reached", data: tocOf(`{"type":"dir"}`, maxEntries), want: maxEntries},
		// Chunk records are not kept, so they do not count against the cap.
		{name: "chunk records past the entry cap", data: tocOf(`{"type":"chunk"}`, maxEntries+1), want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			newBlob, newPos := rewriteTOC(t, blob, pos, tt.data)
			toc, err := Parse(newBlob[newPos.Offset:newPos.Offset+newPos.CompressedLen], newPos)
			if err != nil {
				t.Fatalf("Parse(): %v", err)
			}
			if len(toc.Entries) != tt.want {
				t.Fatalf("Parse() returned %d entries, want %d", len(toc.Entries), tt.want)
			}
		})
	}
}

func TestCountingReaderFillsBuffer(t *testing.T) {
	r := &countingReader{r: iotest.OneByteReader(strings.NewReader("0123456789"))}
	buf := make([]byte, 8)
	if n, err := r.Read(buf); n != 8 || err != nil {
		t.Fatalf("Read() = %d, %v, want 8, nil", n, err)
	}
	if n, err := r.Read(buf); n != 2 || !errors.Is(err, io.EOF) {
		t.Fatalf("Read() = %d, %v, want 2, EOF", n, err)
	}
	if r.n != 10 {
		t.Fatalf("counted %d bytes, want 10", r.n)
	}
}

func TestParseAcceptsTrailingWhitespace(t *testing.T) {
	desc, blob := loadLayer(t, filepath.Join("testdata", "layout"), "basic", 0)
	pos, found, err := Position(desc.Annotations, int64(len(blob)))
	if err != nil || !found {
		t.Fatalf("Position() = _, %v, %v", found, err)
	}
	want, err := Parse(blob[pos.Offset:pos.Offset+pos.CompressedLen], pos)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		trailing string
	}{
		{name: "newline", trailing: "\n"},
		{name: "spaces and tab", trailing: " \t \n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			doc := decodeDocument(t, blob, pos)
			newBlob, newPos := rewriteTOC(t, blob, pos, append(marshalJSON(t, doc), tt.trailing...))
			got, err := Parse(newBlob[newPos.Offset:newPos.Offset+newPos.CompressedLen], newPos)
			if err != nil {
				t.Fatalf("Parse(): %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatal("Parse() differs from the original manifest")
			}
		})
	}
}

func TestReconstructRejectsInvalidOutputLength(t *testing.T) {
	desc, blob := loadLayer(t, filepath.Join("testdata", "layout"), "basic", 0)
	pos, found, err := Position(desc.Annotations, int64(len(blob)))
	if err != nil || !found {
		t.Fatalf("Position() = _, %v, %v", found, err)
	}
	toc, err := Parse(blob[pos.Offset:pos.Offset+pos.CompressedLen], pos)
	if err != nil {
		t.Fatal(err)
	}
	entry := findEntry(t, toc, "usr/bin/tool")
	compressed := blob[entry.Offset:entry.EndOffset]

	t.Run("truncated", func(t *testing.T) {
		if _, err := reconstruct(t, entry, bytes.NewReader(compressed[:len(compressed)-1])); err == nil {
			t.Fatal("Reconstruct() succeeded")
		}
	})

	t.Run("too little output", func(t *testing.T) {
		compressed := compressZstd(t, []byte("abc"))
		if _, err := reconstruct(t, Entry{Size: 4}, bytes.NewReader(compressed)); err == nil {
			t.Fatal("Reconstruct() succeeded")
		}
	})

	t.Run("empty file", func(t *testing.T) {
		if _, err := reconstruct(t, Entry{Size: 0}, bytes.NewReader(nil)); err != nil {
			t.Fatalf("Reconstruct(): %v", err)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		if _, err := reconstruct(t, Entry{Size: 1}, bytes.NewReader(nil)); err == nil {
			t.Fatal("Reconstruct() succeeded")
		}
	})

	t.Run("too much output", func(t *testing.T) {
		compressed := compressZstd(t, bytes.Repeat([]byte("x"), 5))
		r, err := Reconstruct(Entry{Size: 4}, bytes.NewReader(compressed))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close() }()
		var output bytes.Buffer
		if _, err := io.Copy(&output, r); err == nil {
			t.Fatal("reading succeeded")
		}
		if output.Len() != 4 {
			t.Fatalf("read %d bytes, want cap of 4", output.Len())
		}
	})
}

func loadLayer(t *testing.T, layoutDir, ref string, i int) (ocispec.Descriptor, []byte) {
	t.Helper()
	desc, blob, ok := tryLoadLayer(t, layoutDir, ref, i)
	if !ok {
		t.Fatalf("image %q has no layer %d", ref, i)
	}
	return desc, blob
}

func tryLoadLayer(t *testing.T, layoutDir, ref string, i int) (ocispec.Descriptor, []byte, bool) {
	t.Helper()
	index := readJSON[ocispec.Index](t, filepath.Join(layoutDir, "index.json"))
	var manifestDesc *ocispec.Descriptor
	for j := range index.Manifests {
		if index.Manifests[j].Annotations[ocispec.AnnotationRefName] == ref {
			manifestDesc = &index.Manifests[j]
			break
		}
	}
	if manifestDesc == nil {
		t.Fatalf("image %q not found", ref)
	}
	manifest := readJSON[ocispec.Manifest](t, blobPath(layoutDir, manifestDesc.Digest))
	if i >= len(manifest.Layers) {
		return ocispec.Descriptor{}, nil, false
	}
	desc := manifest.Layers[i]
	blob, err := os.ReadFile(blobPath(layoutDir, desc.Digest))
	if err != nil {
		t.Fatalf("read layer %s: %v", desc.Digest, err)
	}
	return desc, blob, true
}

func layoutRefs(t *testing.T, layoutDir string) []string {
	t.Helper()
	index := readJSON[ocispec.Index](t, filepath.Join(layoutDir, "index.json"))
	refs := make([]string, 0, len(index.Manifests))
	for _, desc := range index.Manifests {
		refs = append(refs, desc.Annotations[ocispec.AnnotationRefName])
	}
	return refs
}

func readJSON[T any](t *testing.T, path string) T {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test reads checked-in fixture paths
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return value
}

func blobPath(layoutDir string, d digest.Digest) string {
	return filepath.Join(layoutDir, "blobs", d.Algorithm().String(), d.Encoded())
}

func assertReconstructs(t *testing.T, blob []byte, toc *TOC) {
	t.Helper()
	for _, entry := range toc.Entries {
		if entry.Type != TypeReg || entry.Digest == "" {
			continue
		}
		output, err := reconstruct(t, entry, bytes.NewReader(blob[entry.Offset:entry.EndOffset]))
		if err != nil {
			t.Fatalf("Reconstruct(%q): %v", entry.Name, err)
		}
		if int64(len(output)) != entry.Size {
			t.Errorf("Reconstruct(%q) length = %d, want %d", entry.Name, len(output), entry.Size)
		}
		if got := digest.FromBytes(output); got != entry.Digest {
			t.Errorf("Reconstruct(%q) digest = %s, want %s", entry.Name, got, entry.Digest)
		}
	}
}

func reconstruct(t *testing.T, e Entry, compressed io.Reader) ([]byte, error) {
	t.Helper()
	r, err := Reconstruct(e, compressed)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

func findEntry(t *testing.T, toc *TOC, name string) Entry {
	t.Helper()
	for _, entry := range toc.Entries {
		if entry.Name == name {
			return entry
		}
	}
	t.Fatalf("entry %q not found", name)
	return Entry{}
}

func assertTime(t *testing.T, got *time.Time, sec, nsec int64) {
	t.Helper()
	if got == nil || got.Unix() != sec || int64(got.Nanosecond()) != nsec {
		t.Fatalf("time = %v, want Unix(%d, %d)", got, sec, nsec)
	}
}

func manifestAnnotations(position, checksum string) map[string]string {
	return map[string]string{ManifestPositionKey: position, ManifestChecksumKey: checksum}
}

func mutateTOC(t *testing.T, blob []byte, pos Pos, fn func(*testing.T, []map[string]any) []map[string]any) ([]byte, Pos) {
	t.Helper()
	doc := decodeDocument(t, blob, pos)
	entries, ok := doc["entries"].([]any)
	if !ok {
		t.Fatal("manifest entries are not an array")
	}
	maps := make([]map[string]any, len(entries))
	for i, entry := range entries {
		maps[i], ok = entry.(map[string]any)
		if !ok {
			t.Fatalf("manifest entry %d is not an object", i)
		}
	}
	maps = fn(t, maps)
	doc["entries"] = maps
	return rewriteTOC(t, blob, pos, marshalJSON(t, doc))
}

func decodeDocument(t *testing.T, blob []byte, pos Pos) map[string]any {
	t.Helper()
	decoder, err := zstd.NewReader(bytes.NewReader(blob[pos.Offset : pos.Offset+pos.CompressedLen]))
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	data, err := io.ReadAll(decoder)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func rewriteTOC(t *testing.T, blob []byte, pos Pos, data []byte) ([]byte, Pos) {
	t.Helper()
	compressed := compressZstd(t, data)
	prefix := append([]byte(nil), blob[:pos.Offset]...)
	binary.LittleEndian.PutUint32(prefix[pos.Offset-4:], uint32(len(compressed))) // #nosec G115 -- mutated fixture TOCs are bounded by maxTOCBytes
	newBlob := append(prefix, compressed...)
	newBlob = append(newBlob, blob[pos.Offset+pos.CompressedLen:]...)
	return newBlob, Pos{
		Offset:          pos.Offset,
		CompressedLen:   int64(len(compressed)),
		UncompressedLen: int64(len(data)),
		Checksum:        digest.FromBytes(compressed),
	}
}

func compressZstd(t *testing.T, data []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encoder.Write(data); err != nil {
		t.Fatalf("compress TOC: %v", err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatalf("close TOC encoder: %v", err)
	}
	return compressed.Bytes()
}

// tocOf returns a manifest document holding n copies of entry.
func tocOf(entry string, n int) []byte {
	return []byte(`{"version":1,"entries":[` + strings.TrimSuffix(strings.Repeat(entry+",", n), ",") + `]}`)
}

func marshalJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mutateNamed(name string, fn func(map[string]any)) func(*testing.T, []map[string]any) []map[string]any {
	return func(_ *testing.T, entries []map[string]any) []map[string]any {
		for _, entry := range entries {
			if entry["name"] == name {
				fn(entry)
				break
			}
		}
		return entries
	}
}

func mutateType(typ string, fn func(map[string]any)) func(*testing.T, []map[string]any) []map[string]any {
	return func(_ *testing.T, entries []map[string]any) []map[string]any {
		for _, entry := range entries {
			if entry["type"] == typ {
				fn(entry)
				break
			}
		}
		return entries
	}
}

func firstNamed(t *testing.T, entries []map[string]any, name string) map[string]any {
	t.Helper()
	for _, entry := range entries {
		if entry["name"] == name {
			return entry
		}
	}
	t.Fatalf("entry %q not found", name)
	return nil
}

func cloneMap(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func mustInt64(t *testing.T, value any) int64 {
	t.Helper()
	n, err := value.(json.Number).Int64()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func withChecksum(pos Pos, checksum digest.Digest) Pos {
	pos.Checksum = checksum
	return pos
}

func withUncompressedLen(pos Pos, length int64) Pos {
	pos.UncompressedLen = length
	return pos
}
