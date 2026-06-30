// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package refstore persists reference provenance: a record of the exact image
// reference a pull was given and the immutable descriptor it resolved to. It is
// auxiliary UX/provenance metadata, not a trust anchor and not required to run an
// image -- a digest-pinned image is found via its resolved digest in the object
// store; refs exist for listing, debugging, and naming. Being untrusted, a record
// must be checked on use: its stored ref must equal the reference looked up, its
// refKey must equal sha256(ref), and its resolved digest is only trusted as far
// as the object store verifies the content it names. Each record is a JSON file
// keyed by the SHA-256 of the reference string exactly as given -- never
// canonicalized, so "app:v1" and "docker.io/library/app:v1" key to different
// files:
//
//	refKey = sha256(ref)
//	<root>/refs/sha256/<refkey-xx>/<refkey-rest>.json
//
// The record holds the exact ref, its refKey, the resolved OCI root descriptor
// (whose digest is the image identity, locating its content in the object store),
// the pull time, and the platforms pulled. The file is written atomically and
// durably.
//
//	{ "schema": "application/vnd.knarr.ref.v1+json",
//	  "ref": "registry.example.com/team/app@sha256:...",
//	  "refKey":   {"algorithm":"sha256","digest":"sha256:..."},
//	  "resolved": {"mediaType":"...","digest":"sha256:...","size":N},
//	  "pulledAt": "<RFC3339>", "platforms": ["linux/arm64"] }
package refstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/durable"
)

const refSchema = "application/vnd.knarr.ref.v1+json"

// Store writes reference provenance under the knarr store root.
type Store struct {
	root string
}

// New opens a reference store rooted at root.
func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("refstore: empty store root")
	}
	return &Store{root: root}, nil
}

// Put writes refs/sha256/<refkey-xx>/<refkey-rest>.json atomically.
func (s *Store) Put(ref string, resolved ocispec.Descriptor, pulledAt time.Time, platforms []string) error {
	refKey := key(ref)
	hexKey := refKey.Encoded()
	dir := filepath.Join(s.root, "refs", "sha256", hexKey[:2])
	doc := refDoc{
		Schema: refSchema,
		Ref:    ref,
		RefKey: refKeyDoc{
			Algorithm: "sha256",
			Digest:    refKey,
		},
		Resolved:  descriptorDoc{MediaType: resolved.MediaType, Digest: resolved.Digest, Size: resolved.Size},
		PulledAt:  pulledAt.Format(time.RFC3339),
		Platforms: platforms,
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("refstore: marshal ref: %w", err)
	}
	data = append(data, '\n')

	if err := durable.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("refstore: create ref dir: %w", err)
	}
	if err := durable.WriteFile(filepath.Join(dir, hexKey[2:]+".json"), data, 0o644); err != nil {
		return fmt.Errorf("refstore: write ref: %w", err)
	}
	return nil
}

// The on-disk ref JSON documents.
type (
	refDoc struct {
		Schema    string        `json:"schema"`
		Ref       string        `json:"ref"`
		RefKey    refKeyDoc     `json:"refKey"`
		Resolved  descriptorDoc `json:"resolved"`
		PulledAt  string        `json:"pulledAt"`
		Platforms []string      `json:"platforms"`
	}

	refKeyDoc struct {
		Algorithm string        `json:"algorithm"`
		Digest    digest.Digest `json:"digest"`
	}

	descriptorDoc struct {
		MediaType string        `json:"mediaType"`
		Digest    digest.Digest `json:"digest"`
		Size      int64         `json:"size"`
	}
)

func key(ref string) digest.Digest {
	sum := sha256.Sum256([]byte(ref))
	return digest.NewDigestFromEncoded(digest.SHA256, hex.EncodeToString(sum[:]))
}
