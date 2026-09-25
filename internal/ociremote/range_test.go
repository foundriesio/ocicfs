// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ociremote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/ocichain"
)

var _ ocichain.RangeSource = (*source)(nil)

// TestOpenRangeServesRangesOverOneTransport fetches many ranges of a pushed
// layer, concurrently, and checks each against the layer bytes. The registry
// is pinged exactly once: the authenticated transport is built on the first
// range and shared by the rest.
func TestOpenRangeServesRangesOverOneTransport(t *testing.T) {
	host, tr := startRegistry(t)
	img := hostImage(t, 1)
	root := writeImage(t, host, tr, "team/app", img)
	manifest, err := img.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	layers, err := img.Layers()
	if err != nil {
		t.Fatalf("layers: %v", err)
	}
	blob := readCompressedLayer(t, layers[0])
	desc := descOf(manifest.Layers[0])

	pings := &pathCounter{inner: tr, path: "/v2/"}
	src := testSource(t, host, "team/app", root, pings)
	ctx := context.Background()

	ranges := []struct{ off, n int64 }{{0, 1}, {0, desc.Size}, {desc.Size - 1, 1}, {10, 100}, {100, 10}, {desc.Size / 2, desc.Size / 3}}
	var wg sync.WaitGroup
	for _, r := range ranges {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc, whole, err := src.OpenRange(ctx, desc, r.off, r.n)
			if err != nil {
				t.Errorf("OpenRange(%d, %d): %v", r.off, r.n, err)
				return
			}
			defer func() { _ = rc.Close() }()
			if whole {
				t.Errorf("OpenRange(%d, %d) reported whole", r.off, r.n)
			}
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Errorf("read range (%d, %d): %v", r.off, r.n, err)
				return
			}
			if want := blob[r.off : r.off+r.n]; !bytes.Equal(got, want) {
				t.Errorf("OpenRange(%d, %d) returned %d bytes that differ from the layer", r.off, r.n, len(got))
			}
		}()
	}
	wg.Wait()
	if got := pings.hits.Load(); got != 1 {
		t.Errorf("registry pinged %d times across %d ranges, want 1", got, len(ranges))
	}

	absent := ocispec.Descriptor{Digest: digest.Digest("sha256:" + strings.Repeat("c", 64)), Size: 10}
	if _, _, err := src.OpenRange(ctx, absent, 0, 4); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("OpenRange(absent blob) error = %v, want os.ErrNotExist", err)
	}
}

func TestOpenRangeWholeAnswerIsVerified(t *testing.T) {
	blob := bytes.Repeat([]byte("whole-blob-bytes;"), 64)
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)/2] ^= 0xff

	for _, tt := range []struct {
		name    string
		served  []byte
		wantErr string
	}{
		{name: "faithful", served: blob},
		{name: "tampered", served: tampered, wantErr: "digest mismatch"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src, _ := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
				if _, err := w.Write(tt.served); err != nil {
					t.Errorf("write blob: %v", err)
				}
			})
			rc, whole, err := src.OpenRange(context.Background(), desc, 5, 20)
			if err != nil {
				t.Fatalf("OpenRange: %v", err)
			}
			defer func() { _ = rc.Close() }()
			if !whole {
				t.Fatal("a 200 answer must report whole")
			}
			got, err := io.ReadAll(rc)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("read error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(got, blob) {
				t.Fatalf("whole answer returned %d bytes, want the %d blob bytes", len(got), len(blob))
			}
		})
	}
}

func TestOpenRangeRejectsBadAnswers(t *testing.T) {
	blob := []byte("0123456789abcdefghij")
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
	const off, n = 4, 6
	partial := func(contentRange string, body []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Range", contentRange)
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.WriteHeader(http.StatusPartialContent)
			if _, err := w.Write(body); err != nil {
				t.Errorf("write body: %v", err)
			}
		}
	}
	for _, tt := range []struct {
		name     string
		serve    http.HandlerFunc
		wantErr  string
		notExist bool
	}{
		{name: "416", serve: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusRequestedRangeNotSatisfiable) }, wantErr: "416"},
		{name: "404", serve: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }, wantErr: "404", notExist: true},
		{name: "wrong total", serve: partial("bytes 4-9/21", blob[off:off+n]), wantErr: "Content-Range"},
		{name: "wrong span", serve: partial("bytes 4-10/20", blob[off:off+n+1]), wantErr: "Content-Range"},
		{name: "missing Content-Range", serve: partial("", blob[off:off+n]), wantErr: "Content-Range"},
		{name: "Content-Length mismatch", serve: partial("bytes 4-9/20", blob[off:off+n-1]), wantErr: "Content-Length"},
		{name: "Content-Range trailing junk", serve: partial("bytes 4-9/20 x", blob[off:off+n]), wantErr: "Content-Range"},
		{name: "Content-Range signed field", serve: partial("bytes 4-+9/20", blob[off:off+n]), wantErr: "Content-Range"},
		{name: "Content-Range unsatisfied", serve: partial("bytes */20", blob[off:off+n]), wantErr: "Content-Range"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src, _ := rangeTestSource(t, tt.serve)
			_, _, err := src.OpenRange(context.Background(), desc, off, n)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("OpenRange error = %v, want %q", err, tt.wantErr)
			}
			if errors.Is(err, os.ErrNotExist) != tt.notExist {
				t.Fatalf("OpenRange error = %v, want os.ErrNotExist=%v", err, tt.notExist)
			}
		})
	}
}

// TestOpenRangeAcceptsEquivalentContentRange serves the Content-Range in the
// other legal spellings: the unit in another case and numbers with leading
// zeros.
func TestOpenRangeAcceptsEquivalentContentRange(t *testing.T) {
	blob := []byte("0123456789abcdefghij")
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
	const off, n = 4, 6
	for _, contentRange := range []string{"Bytes 4-9/20", "BYTES 4-9/20", "bytes 04-09/020"} {
		t.Run(contentRange, func(t *testing.T) {
			src, _ := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Range", contentRange)
				w.Header().Set("Content-Length", fmt.Sprint(n))
				w.WriteHeader(http.StatusPartialContent)
				if _, err := w.Write(blob[off : off+n]); err != nil {
					t.Errorf("write body: %v", err)
				}
			})
			rc, _, err := src.OpenRange(context.Background(), desc, off, n)
			if err != nil {
				t.Fatalf("OpenRange: %v", err)
			}
			defer func() { _ = rc.Close() }()
			if got, err := io.ReadAll(rc); err != nil || !bytes.Equal(got, blob[off:off+n]) {
				t.Fatalf("read = %q, %v, want %q", got, err, blob[off:off+n])
			}
		})
	}
}

// TestOpenRangeChunkedAnswerIsCheckedByLength serves a 206 without a
// Content-Length, as a chunked answer has none, and checks the body against
// the requested length as it is read: exact passes, short fails, and a longer
// body yields the requested bytes and no more.
func TestOpenRangeChunkedAnswerIsCheckedByLength(t *testing.T) {
	blob := []byte("0123456789abcdefghij")
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
	const off, n = 4, 6
	for _, tt := range []struct {
		name    string
		body    []byte
		wantErr bool
	}{
		{name: "exact", body: blob[off : off+n]},
		{name: "short", body: blob[off : off+n-1], wantErr: true},
		{name: "long", body: blob[off : off+n+1]},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src, _ := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Range", "bytes 4-9/20")
				w.Header().Set("Transfer-Encoding", "chunked")
				w.WriteHeader(http.StatusPartialContent)
				if _, err := w.Write(tt.body); err != nil {
					t.Errorf("write body: %v", err)
				}
			})
			rc, whole, err := src.OpenRange(context.Background(), desc, off, n)
			if err != nil {
				t.Fatalf("OpenRange: %v", err)
			}
			defer func() { _ = rc.Close() }()
			if whole {
				t.Fatal("206 reported whole")
			}
			got, err := io.ReadAll(rc)
			if (err != nil) != tt.wantErr {
				t.Fatalf("read = %q, %v, want error %v", got, err, tt.wantErr)
			}
			if !tt.wantErr && !bytes.Equal(got, blob[off:off+n]) {
				t.Fatalf("read %q, want %q", got, blob[off:off+n])
			}
		})
	}
}

// TestOpenRangeRetriesUnavailable serves a 503 and then the range: the retry
// transport asks again and the caller sees the bytes.
func TestOpenRangeRetriesUnavailable(t *testing.T) {
	blob := []byte("0123456789abcdefghij")
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
	var calls atomic.Int64
	src, _ := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(blob))
	})
	rc, _, err := src.OpenRange(context.Background(), desc, 3, 7)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if got, err := io.ReadAll(rc); err != nil || !bytes.Equal(got, blob[3:10]) {
		t.Fatalf("read = %q, %v, want %q", got, err, blob[3:10])
	}
	if calls.Load() != 2 {
		t.Fatalf("blob endpoint called %d times, want 2", calls.Load())
	}
}

// TestOpenRangeRefusesRedirectToPrivateAddress checks that a redirect from
// the registry to a loopback, private or link-local address is refused before
// it is followed, whatever spelling the address uses, while a redirect to a
// host name is followed.
func TestOpenRangeRefusesRedirectToPrivateAddress(t *testing.T) {
	blob := []byte("0123456789abcdefghij")
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
	var reached atomic.Int64
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(blob))
	}))
	t.Cleanup(storage.Close)
	port := strings.TrimPrefix(storage.URL, "http://127.0.0.1:")

	for _, host := range []string{"127.0.0.1", "127.1", "0x7f000001", "2130706433", "[::1]", "[::ffff:127.0.0.1]", "169.254.169.254", "10.0.0.1", "192.168.1.1", "[fe80::1%25eth0]", "0.0.0.0"} {
		t.Run(host, func(t *testing.T) {
			src, _ := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "http://"+host+":"+port+r.URL.Path, http.StatusTemporaryRedirect)
			})
			// The registry is reached as localhost, so a redirect to a loopback
			// address literal crosses hosts.
			src.repo = localhostRepo(t, src.repo)
			before := reached.Load()
			_, _, err := src.OpenRange(context.Background(), desc, 3, 7)
			if err == nil || !strings.Contains(err.Error(), "refused") {
				t.Fatalf("OpenRange = %v, want a refused redirect", err)
			}
			if reached.Load() != before {
				t.Fatal("the redirect target was contacted")
			}
		})
	}

	t.Run("host name is followed", func(t *testing.T) {
		src, _ := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://localhost:"+port+r.URL.Path, http.StatusTemporaryRedirect)
		})
		rc, _, err := src.OpenRange(context.Background(), desc, 3, 7)
		if err != nil {
			t.Fatalf("OpenRange: %v", err)
		}
		_ = rc.Close()
	})

	t.Run("second hop is checked", func(t *testing.T) {
		hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://169.254.169.254:"+port+r.URL.Path, http.StatusTemporaryRedirect)
		}))
		t.Cleanup(hop.Close)
		hopURL := "http://localhost:" + strings.TrimPrefix(hop.URL, "http://127.0.0.1:")
		src, _ := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, hopURL+r.URL.Path, http.StatusTemporaryRedirect)
		})
		before := reached.Load()
		if _, _, err := src.OpenRange(context.Background(), desc, 3, 7); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Fatalf("OpenRange = %v, want a refused redirect", err)
		}
		if reached.Load() != before {
			t.Fatal("the redirect target was contacted")
		}
	})
}

// TestOpenRangeRedactsSignedURL redirects to storage that is not listening,
// so the request fails with the storage URL in the error, and checks that the
// signed query it carried is not in the message.
func TestOpenRangeRedactsSignedURL(t *testing.T) {
	blob := []byte("0123456789abcdefghij")
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := "http://localhost:" + strings.TrimPrefix(closed.URL, "http://127.0.0.1:")
	closed.Close()
	src, _ := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, closedURL+r.URL.Path+"?X-Amz-Signature=secret-signature", http.StatusTemporaryRedirect)
	})
	_, _, err := src.OpenRange(context.Background(), desc, 3, 7)
	if err == nil {
		t.Fatal("OpenRange succeeded against a closed server")
	}
	if strings.Contains(err.Error(), "secret-signature") {
		t.Fatalf("error leaks the signed query: %v", err)
	}
	if !strings.Contains(err.Error(), "REDACTED") {
		t.Fatalf("error does not show the redaction: %v", err)
	}

	// A Location the client cannot parse is quoted whole in its error.
	src, _ = rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://storage.example/%zz?X-Amz-Signature=secret-signature")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	_, _, err = src.OpenRange(context.Background(), desc, 3, 7)
	if err == nil {
		t.Fatal("OpenRange followed an unparsable Location")
	}
	if strings.Contains(err.Error(), "secret-signature") {
		t.Fatalf("error leaks the signed query of an unparsable Location: %v", err)
	}
}

// TestOpenRangeRedirectKeepsRange sends the blob request through a redirect to
// a second server under another host name, where the Range header must still
// arrive: a registry commonly redirects blob reads to separate storage.
func TestOpenRangeRedirectKeepsRange(t *testing.T) {
	blob := []byte("0123456789abcdefghij")
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
	var gotRange atomic.Value
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange.Store(r.Header.Get("Range"))
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(blob))
	}))
	t.Cleanup(storage.Close)
	storageURL := "http://localhost:" + strings.TrimPrefix(storage.URL, "http://127.0.0.1:")

	src, _ := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, storageURL+r.URL.Path, http.StatusTemporaryRedirect)
	})
	rc, whole, err := src.OpenRange(context.Background(), desc, 3, 7)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if whole {
		t.Fatal("redirected range reported whole")
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, blob[3:10]) {
		t.Fatalf("redirected range = %q, want %q", got, blob[3:10])
	}
	if hdr, _ := gotRange.Load().(string); hdr != "bytes=3-9" {
		t.Fatalf("storage saw Range %q, want bytes=3-9", hdr)
	}
}

func TestOpenRangeCtxCancelledMidBody(t *testing.T) {
	blob := bytes.Repeat([]byte("x"), 1<<20)
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
	const off, n = 0, 1 << 19
	src, _ := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, off+n-1, len(blob)))
		w.Header().Set("Content-Length", fmt.Sprint(n))
		w.WriteHeader(http.StatusPartialContent)
		if _, err := w.Write(blob[:16]); err != nil {
			t.Errorf("write body: %v", err)
		}
		w.(http.Flusher).Flush()
		// Hold the rest until the client gives up.
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	rc, _, err := src.OpenRange(ctx, desc, off, n)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.ReadFull(rc, make([]byte, 16)); err != nil {
		t.Fatalf("read the flushed prefix: %v", err)
	}
	cancel()
	if _, err := io.ReadAll(rc); !errors.Is(err, context.Canceled) {
		t.Fatalf("read error = %v, want context.Canceled", err)
	}
}

func TestOpenRangeArgumentsCheckedBeforeAnyRequest(t *testing.T) {
	desc := ocispec.Descriptor{Digest: digest.Digest("sha256:" + strings.Repeat("a", 64)), Size: 10}
	src, pings := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected blob request %s", r.URL)
	})
	ctx := context.Background()
	for _, r := range []struct{ off, n int64 }{{8, 3}, {11, 0}, {0, 11}, {-1, 2}, {2, -1}, {1, 1<<63 - 1}} {
		if _, _, err := src.OpenRange(ctx, desc, r.off, r.n); err == nil {
			t.Errorf("OpenRange(%d, %d) = nil error, want rejection", r.off, r.n)
		}
	}
	rc, whole, err := src.OpenRange(ctx, desc, 4, 0)
	if err != nil {
		t.Fatalf("OpenRange(4, 0): %v", err)
	}
	if got, err := io.ReadAll(rc); err != nil || len(got) != 0 || whole {
		t.Fatalf("OpenRange(4, 0) = %d bytes, whole=%v, err=%v; want an empty range", len(got), whole, err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := src.OpenRange(cancelled, desc, 0, 4); !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenRange error = %v, want context.Canceled at open", err)
	}
	if got := pings.Load(); got != 0 {
		t.Fatalf("registry pinged %d times, want none", got)
	}
}

// TestOpenRangeReusesRedirectURL fetches six ranges of one blob from a
// registry that redirects blob reads to storage: the registry is asked once
// and the storage answers every range, each with the right Range header.
func TestOpenRangeReusesRedirectURL(t *testing.T) {
	blob := []byte("0123456789abcdefghij")
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
	st := startStorage(t, serveBlob(blob))
	var blobHits atomic.Int64
	src, _ := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		blobHits.Add(1)
		redirectTo(st.url)(w, r)
	})

	want := fetchSixRanges(t, src, desc, blob)
	if got := blobHits.Load(); got != 1 {
		t.Errorf("registry blob endpoint hit %d times, want 1", got)
	}
	if got := st.hits.Load(); got != 6 {
		t.Errorf("storage hit %d times, want 6", got)
	}
	if fmt.Sprint(st.ranges) != fmt.Sprint(want) {
		t.Errorf("storage saw Range headers %q, want %q", st.ranges, want)
	}
}

// TestOpenRangeRemembersRedirectPerBlob fetches ranges of two blobs: the
// registry resolves each blob once and storage serves the rest.
func TestOpenRangeRemembersRedirectPerBlob(t *testing.T) {
	blobs := map[string][]byte{}
	var descs []ocispec.Descriptor
	for _, text := range []string{"0123456789abcdefghij", "ABCDEFGHIJKLMNOPQRST"} {
		blob := []byte(text)
		desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
		blobs[desc.Digest.String()] = blob
		descs = append(descs, desc)
	}
	st := startStorage(t, func(w http.ResponseWriter, r *http.Request) {
		serveBlob(blobs[path.Base(r.URL.Path)])(w, r)
	})
	var (
		mu       sync.Mutex
		blobHits = map[string]int{}
	)
	src, _ := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		blobHits[r.URL.Path]++
		mu.Unlock()
		redirectTo(st.url)(w, r)
	})

	for _, r := range sixRanges[:3] {
		for _, desc := range descs {
			got, _ := readRange(t, src, desc, r.off, r.n)
			if want := blobs[desc.Digest.String()][r.off : r.off+r.n]; !bytes.Equal(got, want) {
				t.Errorf("OpenRange(%s, %d, %d) = %q, want %q", desc.Digest, r.off, r.n, got, want)
			}
		}
	}
	if len(blobHits) != 2 {
		t.Errorf("registry blob endpoint saw paths %v, want one per blob", blobHits)
	}
	for p, hits := range blobHits {
		if hits != 1 {
			t.Errorf("registry blob endpoint hit %d times for %s, want 1", hits, p)
		}
	}
	if got := st.hits.Load(); got != 6 {
		t.Errorf("storage hit %d times, want 6", got)
	}
}

// TestOpenRangeReusesSameHostRedirect redirects blob reads to another path on
// the registry itself: the remembered URL is used the same way.
func TestOpenRangeReusesSameHostRedirect(t *testing.T) {
	blob := []byte("0123456789abcdefghij")
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
	var blobHits, storageHits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/v2/team/app/blobs/", func(w http.ResponseWriter, r *http.Request) {
		blobHits.Add(1)
		http.Redirect(w, r, "/storage/"+path.Base(r.URL.Path), http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/storage/", func(w http.ResponseWriter, r *http.Request) {
		storageHits.Add(1)
		serveBlob(blob)(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	src := sourceFor(t, srv)

	fetchSixRanges(t, src, desc, blob)
	if got := blobHits.Load(); got != 1 {
		t.Errorf("registry blob endpoint hit %d times, want 1", got)
	}
	if got := storageHits.Load(); got != 6 {
		t.Errorf("storage path hit %d times, want 6", got)
	}
}

// TestOpenRangeWholeAnswerFromRememberedURL has storage ignore the Range
// header on its second request: the whole blob comes back verified, from the
// remembered URL.
func TestOpenRangeWholeAnswerFromRememberedURL(t *testing.T) {
	blob := []byte("0123456789abcdefghij")
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
	var calls atomic.Int64
	st := startStorage(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			serveBlob(blob)(w, r)
			return
		}
		if _, err := w.Write(blob); err != nil {
			t.Errorf("write blob: %v", err)
		}
	})
	var blobHits atomic.Int64
	src, _ := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		blobHits.Add(1)
		redirectTo(st.url)(w, r)
	})

	if got, whole := readRange(t, src, desc, 3, 7); whole || !bytes.Equal(got, blob[3:10]) {
		t.Fatalf("first range = %q, whole=%v; want %q, false", got, whole, blob[3:10])
	}
	if got, whole := readRange(t, src, desc, 5, 5); !whole || !bytes.Equal(got, blob) {
		t.Fatalf("second range = %q, whole=%v; want the whole blob, true", got, whole)
	}
	if got := blobHits.Load(); got != 1 {
		t.Errorf("registry blob endpoint hit %d times, want 1", got)
	}
	if got := st.hits.Load(); got != 2 {
		t.Errorf("storage hit %d times, want 2", got)
	}
}

// TestOpenRangeAsksRegistryWhenRememberedURLIsRejected lets the storage's
// signed URL expire between ranges: the remembered URL answers 403, the range
// is re-resolved through the registry, and the fresh URL that redirect lands
// on serves the range after it.
func TestOpenRangeAsksRegistryWhenRememberedURLIsRejected(t *testing.T) {
	blob := []byte("0123456789abcdefghij")
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
	var valid atomic.Int64
	valid.Store(1)
	st := startStorage(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sig") != fmt.Sprint(valid.Load()) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		serveBlob(blob)(w, r)
	})
	var blobHits atomic.Int64
	src, _ := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		blobHits.Add(1)
		http.Redirect(w, r, st.url+r.URL.Path+"?sig="+fmt.Sprint(valid.Load()), http.StatusTemporaryRedirect)
	})

	for i, r := range sixRanges[:3] {
		if i == 1 {
			valid.Store(2)
		}
		got, whole := readRange(t, src, desc, r.off, r.n)
		if whole || !bytes.Equal(got, blob[r.off:r.off+r.n]) {
			t.Errorf("OpenRange(%d, %d) = %q, whole=%v; want %q", r.off, r.n, got, whole, blob[r.off:r.off+r.n])
		}
	}
	if got := blobHits.Load(); got != 2 {
		t.Errorf("registry blob endpoint hit %d times, want 2", got)
	}
	if got := st.hits.Load(); got != 4 {
		t.Errorf("storage hit %d times, want 4", got)
	}
}

// TestOpenRangeForgottenURLIsNotAskedAgain has the storage reject every
// request after the first while the registry starts serving ranges itself:
// once the remembered URL has failed, later ranges go to the registry without
// asking the storage first.
func TestOpenRangeForgottenURLIsNotAskedAgain(t *testing.T) {
	blob := []byte("0123456789abcdefghij")
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
	var calls atomic.Int64
	st := startStorage(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		serveBlob(blob)(w, r)
	})
	var blobHits atomic.Int64
	src, _ := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		if blobHits.Add(1) == 1 {
			redirectTo(st.url)(w, r)
			return
		}
		serveBlob(blob)(w, r)
	})

	for _, r := range sixRanges[:3] {
		if got, _ := readRange(t, src, desc, r.off, r.n); !bytes.Equal(got, blob[r.off:r.off+r.n]) {
			t.Errorf("OpenRange(%d, %d) = %q, want %q", r.off, r.n, got, blob[r.off:r.off+r.n])
		}
	}
	if got := blobHits.Load(); got != 3 {
		t.Errorf("registry blob endpoint hit %d times, want 3", got)
	}
	if got := st.hits.Load(); got != 2 {
		t.Errorf("storage hit %d times, want 2", got)
	}
}

// TestOpenRangeAsksRegistryWhenRememberedURLIsUnreachable closes the storage a
// range was served from: the next range fails to connect to the remembered
// URL, is re-resolved through the registry, and is served by the storage that
// now answers, whose URL is remembered in turn so a third range needs no
// registry request.
func TestOpenRangeAsksRegistryWhenRememberedURLIsUnreachable(t *testing.T) {
	blob := []byte("0123456789abcdefghij")
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
	a := startStorage(t, serveBlob(blob))
	b := startStorage(t, serveBlob(blob))
	var target atomic.Value
	target.Store(a.url)
	var blobHits atomic.Int64
	src, _ := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		blobHits.Add(1)
		redirectTo(target.Load().(string))(w, r)
	})

	if got, _ := readRange(t, src, desc, 3, 7); !bytes.Equal(got, blob[3:10]) {
		t.Fatalf("first range = %q, want %q", got, blob[3:10])
	}
	a.srv.Close()
	target.Store(b.url)
	if got, _ := readRange(t, src, desc, 5, 5); !bytes.Equal(got, blob[5:10]) {
		t.Fatalf("second range = %q, want %q", got, blob[5:10])
	}
	if got, _ := readRange(t, src, desc, 10, 10); !bytes.Equal(got, blob[10:20]) {
		t.Fatalf("third range = %q, want %q", got, blob[10:20])
	}
	if got := blobHits.Load(); got != 2 {
		t.Errorf("registry blob endpoint hit %d times, want 2", got)
	}
	if a.hits.Load() != 1 || b.hits.Load() != 2 {
		t.Errorf("storage hits = %d and %d, want 1 and 2", a.hits.Load(), b.hits.Load())
	}
}

// TestOpenRangeFollowsRememberedURLRedirect has the storage a range was served
// from redirect the next one on: the URL that redirect lands on replaces the
// remembered one, so later ranges go straight to it and neither the registry
// nor the first storage is asked again. The remembered URL carries a signed
// query, which the redirect target must not be told about.
func TestOpenRangeFollowsRememberedURLRedirect(t *testing.T) {
	blob := []byte("0123456789abcdefghij")
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
	b := startStorage(t, func(w http.ResponseWriter, r *http.Request) {
		if ref := r.Header.Get("Referer"); ref != "" {
			t.Errorf("the redirect target received Referer %q", ref)
		}
		serveBlob(blob)(w, r)
	})
	var calls atomic.Int64
	a := startStorage(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			serveBlob(blob)(w, r)
			return
		}
		redirectTo(b.url)(w, r)
	})
	var blobHits atomic.Int64
	src, _ := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		blobHits.Add(1)
		http.Redirect(w, r, a.url+r.URL.Path+"?Signature=deadbeef", http.StatusTemporaryRedirect)
	})

	for _, r := range sixRanges[:3] {
		if got, _ := readRange(t, src, desc, r.off, r.n); !bytes.Equal(got, blob[r.off:r.off+r.n]) {
			t.Errorf("OpenRange(%d, %d) = %q, want %q", r.off, r.n, got, blob[r.off:r.off+r.n])
		}
	}
	if got := blobHits.Load(); got != 1 {
		t.Errorf("registry blob endpoint hit %d times, want 1", got)
	}
	if a.hits.Load() != 2 || b.hits.Load() != 2 {
		t.Errorf("storage hits = %d and %d, want 2 and 2", a.hits.Load(), b.hits.Load())
	}
	if want := []string{"bytes=3-9", "bytes=19-19"}; fmt.Sprint(b.ranges) != fmt.Sprint(want) {
		t.Errorf("the redirect target saw Range headers %q, want %q", b.ranges, want)
	}
}

// TestOpenRangeKeepsURLOnCancel cancels the context while the remembered URL
// is being asked: the cancellation itself is returned, whatever the abandoned
// request reported, without falling back to the registry, and the URL stays
// remembered for the next range.
func TestOpenRangeKeepsURLOnCancel(t *testing.T) {
	blob := []byte("0123456789abcdefghij")
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
	var calls atomic.Int64
	arrived := make(chan struct{})
	st := startStorage(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 2 {
			close(arrived)
			<-r.Context().Done()
			return
		}
		serveBlob(blob)(w, r)
	})
	var blobHits atomic.Int64
	src, _ := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		blobHits.Add(1)
		redirectTo(st.url)(w, r)
	})

	if got, _ := readRange(t, src, desc, 3, 7); !bytes.Equal(got, blob[3:10]) {
		t.Fatalf("first range = %q, want %q", got, blob[3:10])
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-arrived
		cancel()
	}()
	if _, _, err := src.OpenRange(ctx, desc, 5, 5); err != context.Canceled {
		t.Fatalf("OpenRange error = %v, want exactly context.Canceled", err)
	}
	if got, _ := readRange(t, src, desc, 5, 5); !bytes.Equal(got, blob[5:10]) {
		t.Fatalf("third range = %q, want %q", got, blob[5:10])
	}
	if got := blobHits.Load(); got != 1 {
		t.Errorf("registry blob endpoint hit %d times, want 1", got)
	}
	if got := st.hits.Load(); got != 3 {
		t.Errorf("storage hit %d times, want 3", got)
	}
}

// TestOpenRangeKeepsURLOnCancelledAnswer cancels the context once the
// remembered URL's answer is in hand but before it is judged, and that answer
// is a 206 that does not cover the range asked for: the cancellation is
// returned rather than the answer's fault, the URL stays remembered, and no
// request goes to the registry, which could only fail under a cancelled
// context.
func TestOpenRangeKeepsURLOnCancelledAnswer(t *testing.T) {
	blob := []byte("0123456789abcdefghij")
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
	var calls atomic.Int64
	st := startStorage(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) != 2 {
			serveBlob(blob)(w, r)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(blob)))
		w.WriteHeader(http.StatusPartialContent)
		// The caller abandons this answer, so a write error says nothing.
		_, _ = w.Write(blob[:1])
	})
	var blobHits atomic.Int64
	src, _ := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		blobHits.Add(1)
		redirectTo(st.url)(w, r)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := &cancelWhenAnswered{inner: src.transport, host: strings.TrimPrefix(st.url, "http://"), cancel: cancel}
	src.transport = tr

	if got, _ := readRange(t, src, desc, 3, 7); !bytes.Equal(got, blob[3:10]) {
		t.Fatalf("first range = %q, want %q", got, blob[3:10])
	}
	tr.armed.Store(true)
	if _, _, err := src.OpenRange(ctx, desc, 5, 5); err != context.Canceled {
		t.Fatalf("OpenRange error = %v, want exactly context.Canceled", err)
	}
	tr.armed.Store(false)
	if got, _ := readRange(t, src, desc, 5, 5); !bytes.Equal(got, blob[5:10]) {
		t.Fatalf("third range = %q, want %q", got, blob[5:10])
	}
	if got := blobHits.Load(); got != 1 {
		t.Errorf("registry blob endpoint hit %d times, want 1", got)
	}
	if got := st.hits.Load(); got != 3 {
		t.Errorf("storage hit %d times, want 3", got)
	}
}

// cancelWhenAnswered cancels a context as soon as a request to host has been
// answered, once armed. The answer is complete by then, so it reaches the code
// that judges it with the context already cancelled.
type cancelWhenAnswered struct {
	inner  http.RoundTripper
	host   string
	cancel context.CancelFunc
	armed  atomic.Bool
}

func (t *cancelWhenAnswered) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err == nil && t.armed.Load() && req.URL.Host == t.host {
		t.cancel()
	}
	return resp, err
}

// TestOpenRangeRefusesPrivateRedirectFromRememberedURL has the storage
// redirect later requests to a loopback address literal: the redirect from the
// remembered URL is refused, the range is re-resolved through the registry,
// and the redirect is refused again there, without the target ever being
// contacted.
func TestOpenRangeRefusesPrivateRedirectFromRememberedURL(t *testing.T) {
	blob := []byte("0123456789abcdefghij")
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
	var reached atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		serveBlob(blob)(w, r)
	}))
	t.Cleanup(target.Close)
	var calls atomic.Int64
	st := startStorage(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			serveBlob(blob)(w, r)
			return
		}
		redirectTo(target.URL)(w, r)
	})
	var blobHits atomic.Int64
	src, _ := rangeTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		blobHits.Add(1)
		redirectTo(st.url)(w, r)
	})
	// The registry and storage are both reached as localhost, so only the
	// redirect to the loopback literal crosses hosts.
	src.repo = localhostRepo(t, src.repo)

	if got, _ := readRange(t, src, desc, 3, 7); !bytes.Equal(got, blob[3:10]) {
		t.Fatalf("first range = %q, want %q", got, blob[3:10])
	}
	_, _, err := src.OpenRange(context.Background(), desc, 5, 5)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("OpenRange = %v, want a refused redirect", err)
	}
	if got := reached.Load(); got != 0 {
		t.Errorf("the redirect target was contacted %d times", got)
	}
	if got := blobHits.Load(); got != 2 {
		t.Errorf("registry blob endpoint hit %d times, want 2", got)
	}
	if got := st.hits.Load(); got != 3 {
		t.Errorf("storage hit %d times, want 3", got)
	}
}

// TestOpenRangeTokenStaysWithRegistry runs a registry that demands a bearer
// token and redirects blob reads to storage: the token is sent to the
// registry only, never to the storage, whether the storage is reached through
// the registry's redirect or directly by the remembered URL.
func TestOpenRangeTokenStaysWithRegistry(t *testing.T) {
	blob := []byte("0123456789abcdefghij")
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(blob), Size: int64(len(blob))}
	st := startStorage(t, func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("storage received Authorization %q", auth)
		}
		serveBlob(blob)(w, r)
	})
	var (
		srv      *httptest.Server
		blobHits atomic.Int64
	)
	authorized := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") == "Bearer t" {
			return true
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="r"`)
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, `{"token":"t"}`); err != nil {
			t.Errorf("write token: %v", err)
		}
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		authorized(w, r)
	})
	mux.HandleFunc("/v2/team/app/blobs/", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		blobHits.Add(1)
		redirectTo(st.url)(w, r)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	src := sourceFor(t, srv)

	fetchSixRanges(t, src, desc, blob)
	if got := blobHits.Load(); got != 1 {
		t.Errorf("registry blob endpoint hit %d times, want 1", got)
	}
	if got := st.hits.Load(); got != 6 {
		t.Errorf("storage hit %d times, want 6", got)
	}
}

// localhostRepo returns repo addressed as localhost instead of 127.0.0.1.
func localhostRepo(t *testing.T, repo name.Repository) name.Repository {
	t.Helper()
	r, err := name.NewRepository("localhost:" + strings.TrimPrefix(repo.RegistryStr(), "127.0.0.1:") + "/" + repo.RepositoryStr())
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	return r
}

// rangeTestSource serves a registry over a real socket whose /v2/ ping answers
// 200 (counted) and whose blob endpoint runs serve, and returns a source that
// fetches from it.
func rangeTestSource(t *testing.T, serve http.HandlerFunc) (*source, *atomic.Int64) {
	t.Helper()
	var pings atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		pings.Add(1)
	})
	mux.HandleFunc("/v2/team/app/blobs/", serve)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return sourceFor(t, srv), &pings
}

// sourceFor returns a source that fetches repository team/app from the
// registry srv serves.
func sourceFor(t *testing.T, srv *httptest.Server) *source {
	t.Helper()
	repo, err := name.NewRepository(strings.TrimPrefix(srv.URL, "http://") + "/team/app")
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	return &source{repo: repo, transport: srv.Client().Transport}
}

// storage is a blob storage server a registry redirects to. It is addressed as
// localhost so that a redirect to it from the 127.0.0.1 registry crosses
// hosts, and it counts its requests and records their Range headers in order.
type storage struct {
	srv    *httptest.Server
	url    string
	hits   atomic.Int64
	mu     sync.Mutex
	ranges []string
}

func startStorage(t *testing.T, serve http.HandlerFunc) *storage {
	t.Helper()
	st := &storage{}
	st.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.hits.Add(1)
		st.mu.Lock()
		st.ranges = append(st.ranges, r.Header.Get("Range"))
		st.mu.Unlock()
		serve(w, r)
	}))
	t.Cleanup(st.srv.Close)
	st.url = "http://localhost:" + strings.TrimPrefix(st.srv.URL, "http://127.0.0.1:")
	return st
}

// serveBlob answers a request with the requested range of blob.
func serveBlob(blob []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(blob))
	}
}

// redirectTo answers a blob request with a temporary redirect to the same
// path under base.
func redirectTo(base string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, base+r.URL.Path, http.StatusTemporaryRedirect)
	}
}

// readRange fetches one range of desc through src and returns its bytes and
// whether the answer was the whole blob.
func readRange(t *testing.T, src *source, desc ocispec.Descriptor, off, n int64) ([]byte, bool) {
	t.Helper()
	rc, whole, err := src.OpenRange(context.Background(), desc, off, n)
	if err != nil {
		t.Fatalf("OpenRange(%d, %d): %v", off, n, err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read range (%d, %d): %v", off, n, err)
	}
	return got, whole
}

// sixRanges are the spans of a 20-byte blob the redirect tests fetch in turn.
var sixRanges = []struct{ off, n int64 }{{0, 1}, {3, 7}, {19, 1}, {0, 20}, {5, 5}, {10, 10}}

// fetchSixRanges fetches sixRanges of blob through src, checking each against
// blob and that none was answered whole, and returns the Range headers the
// requests carried.
func fetchSixRanges(t *testing.T, src *source, desc ocispec.Descriptor, blob []byte) []string {
	t.Helper()
	var headers []string
	for _, r := range sixRanges {
		got, whole := readRange(t, src, desc, r.off, r.n)
		if whole {
			t.Errorf("OpenRange(%d, %d) reported whole", r.off, r.n)
		}
		if want := blob[r.off : r.off+r.n]; !bytes.Equal(got, want) {
			t.Errorf("OpenRange(%d, %d) = %q, want %q", r.off, r.n, got, want)
		}
		headers = append(headers, fmt.Sprintf("bytes=%d-%d", r.off, r.off+r.n-1))
	}
	return headers
}
