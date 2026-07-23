// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package verity

import (
	"bytes"
	"context"
	"testing"
)

func TestDigestKnownVectors(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "empty",
			want: "sha256:3d248ca542a24fc62d1c43b916eae5016878e2533c88238480b26128a1f1af95",
		},
		{
			name: "sub-block",
			data: []byte("abc"),
			want: "sha256:700b6bd8510f0b4f9bac8b9cf0459151a1c4a99f467892bb4bd289a67df8e19c",
		},
		{
			name: "exact-block",
			data: make([]byte, BlockSize),
			want: "sha256:babc284ee4ffe7f449377fbf6692715b43aec7bc39c094a95878904d34bac97e",
		},
		{
			name: "multi-block",
			data: make([]byte, 3*BlockSize+17),
			want: "sha256:8ea745426d7b00c769737cfb273ed969d2b500b5668c8065492345e3900cfe91",
		},
		{
			name: "multi-level",
			data: make([]byte, 129*BlockSize),
			want: "sha256:2331d9bc1bfa1c8c1a2272b1bc04acca57ec879136c554d313b45b77b94f326e",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Digest(context.Background(), bytes.NewReader(tt.data))
			if err != nil {
				t.Fatalf("Digest: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Digest = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDigestCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Digest(ctx, bytes.NewReader([]byte("data"))); err == nil {
		t.Fatal("Digest succeeded with canceled context")
	}
}
