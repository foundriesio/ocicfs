# ADR 0002: Image identity

## Status

Accepted.

## Context

An OCI reference can land on either of two documents: an image index, which
lists one manifest per platform, or an image manifest, which describes one
platform's image. Both are legitimate names for the same stored content on a
given host, and knarr acquires both in normal use — a tag resolves to whatever
the registry serves (usually the index), while a caller that has already
selected a platform (a container runtime staging an image, an agent pinning a
digest out of an index) hands knarr the platform manifest.

Keying the store by whichever document the reference happened to land on gives
one image two identities. The bindings, the reference record's target, and the
holds then split across the two digests, and every reference-keyed operation
sees only the one its reference led it to: a `remove` that resolves to the index
finds no holders under it and proceeds, orphaning a still-held binding under the
manifest digest. The protection a hold exists to provide is silently absent.

## Decision

**The image identity is the digest of the platform image manifest, and it is the
only key.** `images/`, `holds/`, the pull fast path, and every verb key on it.
Any reference or digest that lands on an index is descended one level — inside
the index's own digest-verified bytes, selecting the host platform's entry — to
that manifest before the store is touched. Descent reads no manifest and is
exactly one level: a nested index entry is not descended further.

The reference record keeps both digests. `resolved` stays what it has always
been, the descriptor the reference itself landed on (an index for a tag or an
index-pinned reference, a manifest otherwise); it is provenance for an operator
and unauthenticated local bookkeeping, never a verification input. `identity` is
the platform manifest, the digest every lookup goes through. The record schema
is `application/vnd.knarr.ref.v2+json`; a record written under the previous
schema fails closed on read, naming the remedy.

Holds resolve to the identity like everything else, with one asymmetry between
the two verbs. `hold` refuses exactly one resolution failure — a stored blob
that does not hash to the digest naming it, which is store corruption a hold
must not paper over; every other failure holds the digest as given, so an absent
blob still reports "not pulled" and a loose content object is held as the object
it is. `release` is best-effort about store state and never errors on it: a
holder that could not be dropped would linger forever.

## Consequences

- One image has one binding, one holder directory, and one lock, however it was
  named. `remove` keeps its single-lock check-then-delete shape, and a hold
  taken through one door is seen by a `remove` that arrives through the other.
- The digest knarr prints, lists, and reports from `inspect` is the identity. It
  may differ from the digest the caller pinned; it is the digest to hold, to
  pin, and to look the image up by.
- Identity is a function of the stored bytes *and* the platform selector. knarr
  selects a platform entry exactly and rejects ambiguity, so an index offering
  two entries a laxer selector would rank rather than reject can still split
  across consumers. The invariant is one identity per (image, agreed platform
  selector).
- Descending a pinned index costs one read of the index blob, served from the
  object store once it has been pulled and from the pull source before that.
- A pre-existing store is not migrated: the schema bump fails closed and the
  remedy is to recreate the store.

## Rejected alternatives

- **Key by whatever the reference resolved to, and widen `remove`'s holder check
  to every binding it would orphan.** Patches the symptom: duplicate bindings
  remain, and a repointed reference still strands one.
- **Validate the `resolved`/`identity` relationship when a record is read.** The
  record is root-owned auxiliary data that references have always trusted; there
  is no threat this closes, and it would make an auxiliary index a trust input.
- **Persist an index-to-manifest map.** More state to keep consistent than
  descending the verified index bytes on demand.
- **Key holds as given and check two digests on remove**, or **mark both digests
  on hold.** Both need multi-key locking and leave stale markers behind;
  identity-only keying needs neither.

## References

- [ADR 0001: High-level architecture](0001-high-level-architecture.md)
- [On-disk store layout](../on-disk-layout.md)
