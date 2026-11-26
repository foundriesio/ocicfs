// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocicfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	// BindingRemoved is set by a repair: true when the image's binding was
	// removed, so the image must be pulled again.
	BindingRemoved *bool
}

// FsckObject is one object that failed the check. Digest and Fsverity are
// the names it has, or for a missing object the name it is needed under.
type FsckObject struct {
	Digest   digest.Digest
	Fsverity string
	// Problem is FsckMissing, FsckDamaged or FsckMisnamed.
	Problem string
	// Detail says what the check found and, after a repair that left the
	// object, why.
	Detail string
	// Fixed is set by a repair: true when a damaged object was deleted or a
	// misnamed one had its names repaired.
	Fixed *bool
	// InUse reports that a repair left the object because a mounted image
	// uses it.
	InUse bool
}

// FsckOptions is the resolved configuration a Fsck acts on. Callers pass the
// With* options.
type FsckOptions struct {
	// Fix repairs what a store-wide check found.
	Fix bool
	// Force lets a repair delete a damaged object a mounted image uses.
	Force bool
}

// FsckOption adjusts FsckOptions.
type FsckOption func(*FsckOptions)

// WithFix makes a store-wide Fsck repair what it found: it restores the
// names of misnamed objects, deletes damaged objects, and removes the binding
// of every image left without an object it needs, so the next pull of that
// image restores it.
func WithFix() FsckOption {
	return func(o *FsckOptions) { o.Fix = true }
}

// WithFixForce makes a Fsck under WithFix also delete a damaged object that a
// mounted image uses.
func WithFixForce() FsckOption {
	return func(o *FsckOptions) { o.Force = true }
}

// fsckBetweenPhases, when set, runs after the images are listed and before
// the objects are. It is nil outside tests, which use it to interleave a verb
// with a check.
var fsckBetweenPhases func()

// fsckBeforeBinding, when set, runs after the walk of the bindings listed
// one and before it is read. It is nil outside tests.
var fsckBeforeBinding func()

// fsckBeforeFix, when set, runs after the check and before a repair converts
// the store lock to exclusive. It is nil outside tests.
var fsckBeforeFix func()

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
	if cfg.Fix && len(refs) > 0 {
		return nil, errors.New("a repair covers the whole store and takes no reference")
	}
	if cfg.Force && !cfg.Fix {
		return nil, errors.New("force applies to a repair only")
	}
	storeWide := len(refs) == 0

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
	images, loose, err := e.fsckImages(ctx, refs)
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
	if storeWide {
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
	report := c.report(images, storeWide)
	if !cfg.Fix || report.Intact {
		return report, nil
	}

	// Restoring an image whose binding is removed rebuilds its composefs
	// image, so a repair is refused where that cannot be done.
	if err := composefs.Preflight(); err != nil {
		return report, err
	}
	if fsckBeforeFix != nil {
		fsckBeforeFix()
	}
	if err := flockStore(lock, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return report, err
	}
	return e.fsckFix(ctx, c, images, loose, cfg.Force)
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
// metadata or binding is broken is listed with that failure. loose are the
// held digests that are not images.
func (e *engine) fsckImages(ctx context.Context, refs []string) (images []*fsckImage, loose map[digest.Digest]bool, err error) {
	byKey := map[string]*fsckImage{}
	loose = map[digest.Digest]bool{}
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
				return nil, nil, fmt.Errorf("%s: %w", ref, err)
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
			return nil, nil, err
		}
		for _, r := range roots {
			switch {
			case r.loose:
				loose[r.root] = true
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
			return nil, nil, err
		}
		// A runtime reference is not a root, but it is the name an image
		// kept by a hold is pulled again under.
		records, err := e.refs.ListStrict(ctx)
		if err != nil {
			return nil, nil, err
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
		return nil, nil, err
	}
	sort.Slice(images, func(i, j int) bool { return images[i].identity < images[j].identity })
	return images, loose, nil
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

// fsckOutcome is what a repair did with one failed object.
type fsckOutcome struct {
	fixed, inUse bool
	// detail says why an object was left.
	detail string
}

// fsckFix repairs what the check c found. It runs under the exclusive store
// lock. Everything that can fail without evidence about an object is done
// first, so an error returned before the first change leaves the store as it
// was. From then on a failure is recorded on the object or the image and the
// repair goes on; the report says what was done.
func (e *engine) fsckFix(ctx context.Context, c *fsckCheck, images []*fsckImage, loose map[digest.Digest]bool, force bool) (*FsckReport, error) {
	before := c.report(images, true)

	// The lock was not held while it was converted, so the objects are
	// listed and the failed ones checked afresh.
	inventory, err := e.obj.Inventory(ctx)
	if err != nil {
		return before, err
	}
	type target struct {
		entry objectstore.Entry
		res   objectstore.ObjectCheck
	}
	var targets []target
	for _, entry := range inventory {
		old, checked := c.results[entry]
		if !checked || (old.Status == objectstore.ObjectOK && !c.claimed[entry]) {
			continue
		}
		res, err := e.obj.CheckObject(ctx, entry)
		if err != nil {
			return before, err
		}
		if res.Status != objectstore.ObjectOK || c.claimed[entry] {
			targets = append(targets, target{entry, res})
		}
	}
	mounted, err := e.mountedObjects(ctx, inventory)
	if err != nil {
		return before, err
	}

	// A failed object the fresh listing no longer shows under the same
	// names, or no longer shows as failed, is not acted on.
	outcomes := map[objectstore.Entry]fsckOutcome{}
	for entry, res := range c.results {
		if res.Status != objectstore.ObjectOK || c.claimed[entry] {
			outcomes[entry] = fsckOutcome{detail: fsckChanged}
		}
	}
	// kept are the damaged objects left in place, by content-addressed name.
	kept := map[digest.Digest]bool{}
	// Damaged objects go first: deleting one can free the name an intact
	// object lost to it.
	slices.SortStableFunc(targets, func(a, b target) int {
		switch da, db := a.res.Status == objectstore.ObjectDamaged, b.res.Status == objectstore.ObjectDamaged; {
		case da == db:
			return 0
		case da:
			return -1
		}
		return 1
	})
	for _, t := range targets {
		out := e.fixObject(t.entry, t.res, mounted, force)
		if !out.fixed && t.res.Status == objectstore.ObjectDamaged && t.entry.Digest != "" {
			kept[t.entry.Digest] = true
		}
		if out.fixed && t.res.Status == objectstore.ObjectDamaged && loose[t.entry.Digest] {
			out.detail = "held directly, so no pull can restore it; run `ocicfs release` for it"
		}
		outcomes[t.entry] = out
	}

	removed, bindingErr := e.removeBrokenBindings(ctx, kept)
	syncErr := e.obj.Sync()

	report := c.report(images, true)
	for i := range report.Images {
		img := &report.Images[i]
		gone := removed[fsckKey(images[i].identity, images[i].platform)]
		img.BindingRemoved = &gone
		annotateFixed(img.Objects, outcomes)
	}
	annotateFixed(report.Unreferenced, outcomes)
	return report, errors.Join(bindingErr, syncErr)
}

// fsckChanged is the detail of a failed object a repair did not act on, or
// not fully, because it was no longer what the check saw.
const fsckChanged = "changed during the run; run `ocicfs fsck` again"

// fixObject repairs one failed object and says what became of it.
func (e *engine) fixObject(entry objectstore.Entry, res objectstore.ObjectCheck, mounted *mountedSet, force bool) fsckOutcome {
	var err error
	switch {
	case res.Status != objectstore.ObjectDamaged:
		err = e.obj.RepairNames(entry, res)
	case res.Unopenable:
		// Without an open nothing can tell whether a mount relies on it.
		return fsckOutcome{detail: "cannot be opened, so it is left in place; check the storage, and remove its names by hand once nothing mounts it"}
	case !force && entry.ViewName != "" && mounted.uses(entry.ViewName):
		return fsckOutcome{inUse: true, detail: "a mounted image uses it; stop its containers, or run `ocicfs fsck --fix --force`"}
	default:
		err = e.obj.RemoveEntry(entry, res)
	}
	switch {
	case err == nil:
		return fsckOutcome{fixed: true}
	case errors.Is(err, objectstore.ErrInUse):
		return fsckOutcome{inUse: true, detail: "it is a mounted composefs image; unmount its users and run `ocicfs fsck --fix` again"}
	case errors.Is(err, objectstore.ErrChanged):
		return fsckOutcome{detail: fsckChanged}
	}
	return fsckOutcome{detail: err.Error()}
}

// mountedSet is the set of per-file objects mounted images refer to, by
// view-relative name. unknown is set when a mounted image could not be read:
// every object then counts as used.
type mountedSet struct {
	names   map[string]struct{}
	unknown bool
}

func (m *mountedSet) uses(viewName string) bool {
	_, ok := m.names[viewName]
	return ok || m.unknown
}

// mountedObjects finds the mounted composefs images among the inventoried
// objects, by the lock a mount holds on its image, and collects the objects
// they refer to. Every object is probed through whichever name it has, since
// a mounted image can have lost either.
func (e *engine) mountedObjects(ctx context.Context, inventory []objectstore.Entry) (*mountedSet, error) {
	m := &mountedSet{names: map[string]struct{}{}}
	for _, entry := range inventory {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		inUse, path, err := e.obj.EntryInUse(entry)
		if err != nil {
			// An object that cannot be opened cannot be probed either.
			slog.Warn("ocicfs: fsck could not probe an object for a mounter; treating every object as in use", "object", path, "err", err)
			m.unknown = true
			continue
		}
		if !inUse {
			continue
		}
		names, err := composefs.ReferencedObjects(ctx, path)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			slog.Warn("ocicfs: fsck could not read a mounted image; treating every object as in use", "object", path, "err", err)
			m.unknown = true
			continue
		}
		for _, name := range names {
			m.names[name] = struct{}{}
		}
	}
	return m, nil
}

// removeBrokenBindings deletes the binding of every image, wanted or not,
// that lacks something of its own: its manifest, its config, its composefs
// image, or an object the composefs image refers to. The next pull of such
// an image then restores it instead of answering that it is already there.
// A root the image is reached through is not the binding's: a pull reads a
// missing root from its source and stores it with the binding in place.
// Each binding is judged from the store as it is now, by presence alone, and
// under the identity's hold lock, which a remove of the image takes too. A
// binding whose composefs image is among kept, the damaged objects left in
// place, stays: without it the next pull would adopt that damaged image
// again. It returns the bindings removed, keyed by identity and platform.
func (e *engine) removeBrokenBindings(ctx context.Context, kept map[digest.Digest]bool) (map[string]bool, error) {
	removed := map[string]bool{}
	err := e.img.Walk(ctx, func(identity digest.Digest, p ocispec.Platform) error {
		unlock, err := e.holds.Lock(identity)
		if err != nil {
			return err
		}
		defer unlock()
		platform := ocilayout.PlatformString(&p)
		broken, err := e.bindingBroken(ctx, identity, p, kept)
		if errors.Is(err, imagestore.ErrNotFound) {
			// A remove deleted it since the walk listed it.
			return nil
		}
		if err != nil {
			slog.Warn("ocicfs: fsck left a binding it could not judge", "identity", identity, "platform", platform, "err", err)
			return nil
		}
		if !broken {
			return nil
		}
		if err := e.img.Delete(identity, p); err != nil {
			return err
		}
		removed[fsckKey(identity, p)] = true
		slog.Debug("ocicfs: fsck removed a binding", "identity", identity, "platform", platform)
		return nil
	})
	return removed, err
}

// bindingBroken reports whether the image bound as (identity, p) lacks its
// composefs image, its manifest or config, or an object the composefs image
// refers to.
func (e *engine) bindingBroken(ctx context.Context, identity digest.Digest, p ocispec.Platform, kept map[digest.Digest]bool) (bool, error) {
	b, err := e.img.Get(identity, p)
	if err != nil {
		return false, err
	}
	if kept[b.Descriptor.Digest] {
		return false, nil
	}
	if present, err := e.obj.Has(b.Descriptor.Digest); err != nil || !present {
		return !present, err
	}
	if _, err := e.resolveStoredImage(ctx, identity, p); errors.Is(err, os.ErrNotExist) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	path, err := e.obj.ObjectPath(b.Descriptor.Digest)
	if err != nil {
		return false, err
	}
	names, err := composefs.ReferencedObjects(ctx, path)
	if err != nil {
		return false, err
	}
	for _, name := range names {
		if _, err := os.Lstat(filepath.Join(e.obj.ViewDir(), name)); errors.Is(err, os.ErrNotExist) {
			return true, nil
		} else if err != nil {
			return false, err
		}
	}
	return false, nil
}

// annotateFixed records on each failed object what the repair did with it. A
// missing object has nothing to repair and is left as reported.
func annotateFixed(objects []FsckObject, outcomes map[objectstore.Entry]fsckOutcome) {
	for i := range objects {
		o := &objects[i]
		entry := objectstore.Entry{Digest: o.Digest}
		if o.Fsverity != "" {
			entry.ViewName = viewRelativeName(o.Fsverity)
		}
		out, ok := outcomes[entry]
		if !ok || o.Problem == FsckMissing {
			continue
		}
		fixed := out.fixed
		o.Fixed, o.InUse = &fixed, out.inUse
		if out.detail != "" {
			o.Detail = strings.TrimPrefix(o.Detail+"; "+out.detail, "; ")
		}
	}
}
