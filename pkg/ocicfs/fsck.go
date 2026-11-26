// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocicfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"

	"github.com/foundriesio/ocicfs/internal/composefs"
	"github.com/foundriesio/ocicfs/internal/imagestore"
	"github.com/foundriesio/ocicfs/internal/objectstore"
	"github.com/foundriesio/ocicfs/internal/ocichain"
	"github.com/foundriesio/ocicfs/internal/ocilayout"
	"github.com/foundriesio/ocicfs/internal/refstore"
)

// The problems Fsck reports on an object.
const (
	// FsckMissing: an image needs the object and the store does not have it.
	FsckMissing = "missing"
	// FsckDamaged: the object's bytes match none of its names, or could not
	// be read back.
	FsckDamaged = "damaged"
	// FsckMisnamed: the object's bytes are intact, but one of its two names
	// is absent or names different content.
	FsckMisnamed = "misnamed"
)

// FsckReport is the result of one Fsck.
type FsckReport struct {
	// Intact reports that every image checked is intact and, in a
	// store-wide check, that no other object failed.
	Intact bool
	// Images are the images checked, in digest order.
	Images []FsckImage
	// Unreferenced are the failed objects no checked image needs. Only a
	// store-wide check fills it.
	Unreferenced []FsckObject
}

// FsckImage is one image Fsck checked.
type FsckImage struct {
	// Digest is the image's identity: its platform image manifest.
	Digest digest.Digest
	// Platform is the platform it is stored for, as os/arch[/variant].
	Platform string
	// Refs are the references that name it, runtime references included.
	Refs []string
	// Intact reports that Error is empty and Objects is.
	Intact bool
	// Error is an image-level failure: its metadata does not resolve, it
	// has no binding, or its composefs image is not the expected one.
	Error string
	// Objects are the objects the image needs that failed the check.
	Objects []FsckObject
}

// FsckObject is one object that failed the check. Digest and Fsverity are
// the names it has, or for a missing object the name it is needed under.
type FsckObject struct {
	Digest   digest.Digest
	Fsverity string
	// Problem is FsckMissing, FsckDamaged or FsckMisnamed.
	Problem string
	// Detail says what the check found.
	Detail string
}

// FsckOptions is the resolved configuration a Fsck acts on. Callers pass the
// With* options.
type FsckOptions struct{}

// FsckOption adjusts FsckOptions.
type FsckOption func(*FsckOptions)

// fsckBetweenPhases, when set, runs after the images are listed and before
// the objects are. It is nil outside tests, which use it to interleave a verb
// with a check.
var fsckBetweenPhases func()

// fsckBeforeBinding, when set, runs after the walk of the bindings listed
// one and before it is read. It is nil outside tests.
var fsckBeforeBinding func()

// Fsck checks stored content against its digests.
func (e *engine) Fsck(ctx context.Context, refs []string, opts ...FsckOption) (*FsckReport, error) {
	report, err := e.fsck(ctx, refs, opts...)
	if err != nil {
		return report, fmt.Errorf("fsck: %w", err)
	}
	return report, nil
}

func (e *engine) fsck(ctx context.Context, refs []string, opts ...FsckOption) (*FsckReport, error) {
	var cfg FsckOptions
	for _, opt := range opts {
		opt(&cfg)
	}

	// The store lock is held shared, as a pull holds it, so no collection
	// deletes an object between its listing and its check.
	lock, err := e.openStoreLock(unix.LOCK_SH)
	if err != nil {
		return nil, err
	}
	defer releaseStoreLock(lock)()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// The images are listed before the objects, so every object a listed
	// image refers to was committed before the inventory.
	images, err := e.fsckImages(ctx, refs)
	if err != nil {
		return nil, err
	}
	if fsckBetweenPhases != nil {
		fsckBetweenPhases()
	}
	inventory, err := e.obj.Inventory(ctx)
	if err != nil {
		return nil, err
	}
	c := newFsckCheck(e.obj, inventory)
	if len(refs) == 0 {
		for _, entry := range inventory {
			if _, err := c.check(ctx, entry); err != nil {
				return nil, err
			}
		}
	}
	for _, img := range images {
		if err := c.checkImage(ctx, img); err != nil {
			return nil, err
		}
	}
	return c.report(images, len(refs) == 0), nil
}

// fsckImage is one image to check and what the check needs of it.
type fsckImage struct {
	identity digest.Digest
	platform ocispec.Platform
	refs     []string
	// err is the image-level failure found so far, and noBinding says that
	// it is the lack of a binding.
	err       string
	noBinding bool
	// blobs are the objects the image needs by content digest: its metadata
	// chain and its composefs image. names are the per-file objects its
	// composefs image refers to, by view-relative name.
	blobs []digest.Digest
	names []string
	// composefs is the image's composefs object and expected the fs-verity
	// digest a mount requires of it; both are empty without a binding.
	composefs digest.Digest
	expected  string
	// objects are the failed objects, filled by the check.
	objects []FsckObject
}

// fsckImages lists the images to check: the ones refs name, or with no refs
// every image a root of the store keeps and every image that has a binding,
// each once however many roots name it, with what every one of those roots
// needs. A reference that names no local image is an error; an image whose
// metadata or binding is broken is listed with that failure.
func (e *engine) fsckImages(ctx context.Context, refs []string) ([]*fsckImage, error) {
	byKey := map[string]*fsckImage{}
	var images []*fsckImage
	add := func(img *fsckImage, root digest.Digest, ref, rootErr string) {
		if rootErr != "" {
			img.err = rootErr
		}
		key := fsckKey(img.identity, img.platform)
		if seen, ok := byKey[key]; ok {
			// A second root of an image already listed: its failure and
			// the blobs it reaches the image through are the image's too.
			if seen.err == "" && img.err != "" {
				seen.err = fmt.Sprintf("through root %s: %s", root, img.err)
			}
			for _, d := range img.blobs {
				if !slices.Contains(seen.blobs, d) {
					seen.blobs = append(seen.blobs, d)
				}
			}
			if seen.composefs == "" {
				seen.composefs, seen.expected, seen.names = img.composefs, img.expected, img.names
			}
			img = seen
		} else {
			byKey[key] = img
			images = append(images, img)
		}
		if ref != "" && !slices.Contains(img.refs, ref) {
			img.refs = append(img.refs, ref)
		}
	}

	if len(refs) > 0 {
		for _, ref := range refs {
			root, identity, p, err := e.fsckRef(ref)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", ref, err)
			}
			img := e.describeImage(ctx, root, identity, p)
			add(img, root, ref, "")
			if identity != "" && identity != root && img.composefs == "" {
				// The root did not lead to the image, and the record
				// names it all the same.
				add(e.describeImage(ctx, identity, identity, p), identity, ref, "")
			}
		}
	} else {
		roots, err := e.wantedRoots(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range roots {
			switch {
			case r.loose:
			case r.err != nil:
				add(e.describeImage(ctx, r.root, r.identity, hostPlatform()), r.root, r.ref, r.err.Error())
			default:
				for _, p := range r.platforms {
					add(e.describeImage(ctx, r.root, r.identity, p), r.root, r.ref, "")
				}
			}
		}
		// A binding no root keeps is an image all the same: a pull of it
		// answers that it is already there. Its chain starts at its own
		// manifest. A remove takes no store lock, so the binding can be gone
		// by the time it is read: that image was removed, and a remove
		// deletes nothing else that is read here.
		err = e.img.Walk(ctx, func(identity digest.Digest, p ocispec.Platform) error {
			if fsckBeforeBinding != nil {
				fsckBeforeBinding()
			}
			if img := e.describeImage(ctx, identity, identity, p); !img.noBinding {
				add(img, identity, "", "")
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		// A runtime reference is not a root, but it is the name an image
		// kept by a hold is pulled again under.
		records, err := e.refs.ListStrict(ctx)
		if err != nil {
			return nil, err
		}
		for _, rec := range records {
			for _, img := range images {
				if rec.Holder != "" && img.identity == rec.Identity.Digest && !slices.Contains(img.refs, rec.Ref) {
					img.refs = append(img.refs, rec.Ref)
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Slice(images, func(i, j int) bool { return images[i].identity < images[j].identity })
	return images, nil
}

// fsckRef returns the chain ref names: its root and, for a recorded
// reference, the identity the root must resolve to. p is the host platform,
// in the spelling the record has for it. A pinned digest needs no record.
func (e *engine) fsckRef(ref string) (root, identity digest.Digest, p ocispec.Platform, err error) {
	p = hostPlatform()
	root, pinned, err := e.localAnchor(ref)
	if root == "" {
		return "", "", p, err
	}
	normalized, err := NormalizeRef(ref)
	if err != nil {
		return root, "", p, nil
	}
	rec, err := e.refs.Get(normalized)
	if pinned && errors.Is(err, refstore.ErrNotFound) {
		return root, "", p, nil
	}
	if err != nil {
		return "", "", p, err
	}
	recorded, err := parseRecordPlatforms(rec.Platforms)
	if err != nil {
		return "", "", p, err
	}
	for _, r := range recorded {
		if fsckKey("", r) == fsckKey("", p) {
			p = r
		}
	}
	return rec.Resolved.Digest, rec.Identity.Digest, p, nil
}

// fsckKey names an image by identity and platform. An image whose metadata
// does not resolve is known by the platform it was asked for, in the
// spelling of whoever asked, so the key takes the one spelling bindings are
// keyed by.
func fsckKey(identity digest.Digest, p ocispec.Platform) string {
	p = imagestore.CanonicalPlatform(p)
	return identity.String() + "|" + ocilayout.PlatformString(&p)
}

// describeImage resolves the image root anchors for platform p and lists the
// objects it needs. wantIdentity, when not empty, is the identity the root
// must resolve to. A failure is recorded on the image, which then lists what
// could still be determined.
func (e *engine) describeImage(ctx context.Context, root, wantIdentity digest.Digest, p ocispec.Platform) *fsckImage {
	img := &fsckImage{identity: wantIdentity, platform: p}
	if img.identity == "" {
		img.identity = root
	}
	need := func(blobs ...digest.Digest) {
		for _, d := range blobs {
			if !slices.Contains(img.blobs, d) {
				img.blobs = append(img.blobs, d)
			}
		}
	}
	need(root)
	src := casBlobSource{store: e.obj}
	res, err := ocichain.Resolve(ctx, src, root, p)
	if err != nil {
		img.err = "image metadata does not resolve: " + err.Error()
		// The image needs its manifest and config all the same, as far as
		// they can still be told.
		manifest := wantIdentity
		if d, _, err := descendRoot(ctx, src, root, p); err == nil {
			manifest = d.Identity.Digest
		}
		if manifest == "" {
			return img
		}
		need(manifest)
		var m ocispec.Manifest
		if raw, err := ocichain.ReadRootBlob(ctx, src, manifest); err == nil && json.Unmarshal(raw, &m) == nil && m.Config.Digest.Validate() == nil {
			need(m.Config.Digest)
		}
		return img
	}
	img.platform = res.Platform
	need(res.ManifestDesc.Digest, res.ConfigDesc.Digest)
	if wantIdentity != "" && res.ManifestDesc.Digest != wantIdentity {
		img.err = fmt.Sprintf("root %s resolves to manifest %s, not to %s; repair the record with `ocicfs pull` or `ocicfs remove`", root, res.ManifestDesc.Digest, wantIdentity)
		return img
	}
	img.identity = res.ManifestDesc.Digest

	b, err := e.img.Get(img.identity, res.Platform)
	if errors.Is(err, imagestore.ErrNotFound) {
		img.err, img.noBinding = "no composefs image for this platform; run `ocicfs pull` to restore it", true
		return img
	}
	if err != nil {
		img.err = "read image binding: " + err.Error()
		return img
	}
	img.composefs = b.Descriptor.Digest
	need(img.composefs)
	if img.expected, _, err = expectedFsverity(res.Manifest.Annotations, b.FsverityDigest); err != nil {
		img.err = err.Error()
		return img
	}
	path, err := e.obj.ObjectPath(img.composefs)
	if err != nil {
		img.err = err.Error()
		return img
	}
	if img.names, err = composefs.ReferencedObjects(ctx, path); err != nil {
		img.err = "read composefs image: " + err.Error()
	}
	return img
}

// fsckCheck holds the inventory and the result of every object check made,
// so an object several images share is read once.
type fsckCheck struct {
	store    *objectstore.Store
	byDigest map[digest.Digest]objectstore.Entry
	byView   map[string]objectstore.Entry
	results  map[objectstore.Entry]objectstore.ObjectCheck
	// viewless maps the view name an intact object without one should have
	// to the object. It is filled on first use.
	viewless map[string]objectstore.Entry
	// claimed are the failed entries some image needs.
	claimed map[objectstore.Entry]bool
}

func newFsckCheck(store *objectstore.Store, inventory []objectstore.Entry) *fsckCheck {
	c := &fsckCheck{
		store:    store,
		byDigest: map[digest.Digest]objectstore.Entry{},
		byView:   map[string]objectstore.Entry{},
		results:  map[objectstore.Entry]objectstore.ObjectCheck{},
		claimed:  map[objectstore.Entry]bool{},
	}
	for _, entry := range inventory {
		if entry.Digest != "" {
			// An entry with a view name wins over a second one without.
			if seen, ok := c.byDigest[entry.Digest]; !ok || seen.ViewName == "" {
				c.byDigest[entry.Digest] = entry
			}
		}
		if entry.ViewName != "" {
			c.byView[entry.ViewName] = entry
		}
	}
	return c
}

// check checks one object, once.
func (c *fsckCheck) check(ctx context.Context, entry objectstore.Entry) (objectstore.ObjectCheck, error) {
	if res, ok := c.results[entry]; ok {
		return res, nil
	}
	res, err := c.store.CheckObject(ctx, entry)
	if err != nil {
		return res, err
	}
	c.results[entry] = res
	return res, nil
}

// checkImage checks every object img needs and records the ones that failed
// on it.
func (c *fsckCheck) checkImage(ctx context.Context, img *fsckImage) error {
	for _, d := range img.blobs {
		entry, ok := c.byDigest[d]
		if !ok {
			img.objects = append(img.objects, FsckObject{Digest: d, Problem: FsckMissing})
			continue
		}
		res, err := c.check(ctx, entry)
		if err != nil {
			return err
		}
		c.claim(img, entry, res)
		if d == img.composefs && res.Status == objectstore.ObjectOK && res.Fsverity != img.expected && img.err == "" {
			img.err = fmt.Sprintf("composefs image %s has fs-verity digest %s, expected %s; run `ocicfs remove` then `ocicfs pull` to repair it", d, res.Fsverity, img.expected)
		}
	}
	for _, name := range img.names {
		entry, ok := c.byView[name]
		if !ok {
			// An intact object that lost only its view name is not missing.
			if entry, ok, err := c.viewlessObject(ctx, name); err != nil {
				return err
			} else if ok {
				c.claimed[entry] = true
				img.objects = append(img.objects, FsckObject{Digest: entry.Digest, Problem: FsckMisnamed, Detail: "the view name " + name + " is missing"})
				continue
			}
			img.objects = append(img.objects, FsckObject{Fsverity: viewFsverity(name), Problem: FsckMissing})
			continue
		}
		res, err := c.check(ctx, entry)
		if err != nil {
			return err
		}
		c.claim(img, entry, res)
	}
	return nil
}

// claim records entry on img when its check failed.
func (c *fsckCheck) claim(img *fsckImage, entry objectstore.Entry, res objectstore.ObjectCheck) {
	if res.Status == objectstore.ObjectOK {
		return
	}
	c.claimed[entry] = true
	img.objects = append(img.objects, fsckObject(entry, res))
}

// viewlessObject returns the intact object that has only its
// content-addressed name and whose view name would be name. Finding one
// checks every such object, on the first call.
func (c *fsckCheck) viewlessObject(ctx context.Context, name string) (objectstore.Entry, bool, error) {
	if c.viewless == nil {
		c.viewless = map[string]objectstore.Entry{}
		for _, entry := range c.byDigest {
			if entry.ViewName != "" {
				continue
			}
			res, err := c.check(ctx, entry)
			if err != nil {
				return objectstore.Entry{}, false, err
			}
			if res.Status == objectstore.ObjectOK {
				c.viewless[viewRelativeName(res.Fsverity)] = entry
			}
		}
	}
	entry, ok := c.viewless[name]
	return entry, ok, nil
}

// report builds the report once every check is made. storeWide adds the
// failed objects no image claimed.
func (c *fsckCheck) report(images []*fsckImage, storeWide bool) *FsckReport {
	r := &FsckReport{Intact: true}
	for _, img := range images {
		out := FsckImage{
			Digest:   img.identity,
			Platform: ocilayout.PlatformString(&img.platform),
			Refs:     img.refs,
			Intact:   img.err == "" && len(img.objects) == 0,
			Error:    img.err,
			Objects:  img.objects,
		}
		sort.Strings(out.Refs)
		r.Intact = r.Intact && out.Intact
		r.Images = append(r.Images, out)
	}
	if !storeWide {
		return r
	}
	for entry, res := range c.results {
		if res.Status != objectstore.ObjectOK && !c.claimed[entry] {
			r.Unreferenced = append(r.Unreferenced, fsckObject(entry, res))
		}
	}
	sort.Slice(r.Unreferenced, func(i, j int) bool {
		a, b := r.Unreferenced[i], r.Unreferenced[j]
		return a.Digest.String()+a.Fsverity < b.Digest.String()+b.Fsverity
	})
	r.Intact = r.Intact && len(r.Unreferenced) == 0
	return r
}

// fsckObject reports the failed object entry under the names it has.
func fsckObject(entry objectstore.Entry, res objectstore.ObjectCheck) FsckObject {
	o := FsckObject{Digest: entry.Digest, Problem: FsckDamaged, Detail: res.Detail}
	if entry.ViewName != "" {
		o.Fsverity = viewFsverity(entry.ViewName)
	}
	if res.Status == objectstore.ObjectMisnamed {
		o.Problem = FsckMisnamed
	}
	return o
}

// viewFsverity maps a view-relative name ("ab/cdef...") to the fs-verity
// digest it carries ("sha256:<hex>").
func viewFsverity(name string) string {
	return "sha256:" + strings.Replace(name, "/", "", 1)
}
