// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package octest

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"

	"github.com/google/go-containerregistry/pkg/registry"
)

// Registry is an in-memory OCI registry for tests.
type Registry struct {
	handler http.Handler
	closed  atomic.Bool
}

// NewRegistry returns an open in-memory OCI registry.
func NewRegistry() *Registry {
	return &Registry{handler: registry.New()}
}

// Close prevents future requests from reaching the in-memory registry.
func (r *Registry) Close() {
	r.closed.Store(true)
}

// Transport returns an http transport that dispatches requests to the registry.
func (r *Registry) Transport() http.RoundTripper {
	return r
}

// RoundTrip dispatches req to the registry handler without opening a socket.
func (r *Registry) RoundTrip(req *http.Request) (*http.Response, error) {
	if r.closed.Load() {
		err := errors.New("octest: registry is closed")
		if req.Body != nil {
			if closeErr := req.Body.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("octest: close request body: %w", closeErr))
			}
		}
		return nil, err
	}

	if req.Body == nil {
		req.Body = http.NoBody
	}
	rr := httptest.NewRecorder()
	r.handler.ServeHTTP(rr, req)
	if err := req.Body.Close(); err != nil {
		return nil, fmt.Errorf("octest: close request body: %w", err)
	}
	resp := rr.Result()
	resp.Request = req
	return resp, nil
}
