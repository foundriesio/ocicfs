// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package digestutil

import (
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
)

func TestValidateSHA256(t *testing.T) {
	valid := digest.FromString("hello").String()

	tests := []struct {
		name    string
		digest  string
		wantErr string // substring of the expected error; empty means valid
	}{
		{"valid", valid, ""},
		{"empty", "", "malformed digest"},
		{"no colon", "sha256", "malformed digest"},
		{"colon first", ":" + strings.Repeat("a", 64), "malformed digest"},
		{"colon last", "sha256:", "malformed digest"},
		{"non-sha256 algorithm", "sha512:" + strings.Repeat("a", 128), "non-sha256 digest algorithm"},
		{"unlinked algorithm", "md5:" + strings.Repeat("a", 32), "non-sha256 digest algorithm"},
		{"path-bearing algorithm", "../sha256:" + strings.Repeat("a", 64), "non-sha256 digest algorithm"},
		{"path-bearing hex", "sha256:../../../../etc/passwd", "invalid digest"},
		{"uppercase hex", "sha256:" + strings.Repeat("A", 64), "invalid digest"},
		{"short hex", "sha256:abc123", "invalid digest"},
		{"long hex", "sha256:" + strings.Repeat("a", 65), "invalid digest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSHA256(digest.Digest(tt.digest))
			checkErr(t, tt.digest, err, tt.wantErr)
		})
	}
}

// TestParseSHA256 covers the parse wrapper; the shared validation edges are
// exercised by TestValidateSHA256.
func TestParseSHA256(t *testing.T) {
	valid := digest.FromString("hello").String()

	tests := []struct {
		name    string
		digest  string
		wantErr string
	}{
		{"valid", valid, ""},
		{"unparseable", "not-a-digest", "invalid checksum digest format"},
		{"non-sha256 algorithm", "sha512:" + strings.Repeat("a", 128), "unsupported digest algorithm"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := ParseSHA256(tt.digest)
			checkErr(t, tt.digest, err, tt.wantErr)
			if tt.wantErr == "" && d.String() != tt.digest {
				t.Fatalf("ParseSHA256(%q) = %q, want the input digest", tt.digest, d)
			}
		})
	}
}

func checkErr(t *testing.T, in string, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("digest %q: unexpected error %v", in, err)
		}
		return
	}
	if err == nil {
		t.Fatalf("digest %q: got nil, want error containing %q", in, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("digest %q: error = %v, want it to contain %q", in, err, want)
	}
}
