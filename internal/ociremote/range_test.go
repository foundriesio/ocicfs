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

// rangeTestSource serves a registry over a real socket whose /v2/ ping answers
// 200 (counted) and whose blob endpoint runs serve, and returns a source that
// fetches from it.
// localhostRepo returns repo addressed as localhost instead of 127.0.0.1.
func localhostRepo(t *testing.T, repo name.Repository) name.Repository {
	t.Helper()
	r, err := name.NewRepository("localhost:" + strings.TrimPrefix(repo.RegistryStr(), "127.0.0.1:") + "/" + repo.RepositoryStr())
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	return r
}

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
	repo, err := name.NewRepository(strings.TrimPrefix(srv.URL, "http://") + "/team/app")
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	return &source{repo: repo, transport: srv.Client().Transport}, &pings
}
