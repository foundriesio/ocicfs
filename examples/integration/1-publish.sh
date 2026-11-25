#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Publishing: docs/integration.md#publishing
#
# Publishes versions v1 and v2 of the example image, each converted to
# zstd:chunked and sealed.
#
# Needs:    IMAGE         repository to push to, such as ghcr.io/acme/app
#           TAG_SUFFIX    optional, keeps the tags of concurrent runs apart
# Produces: work/v1.ref, work/v2.ref    the image references to deploy
#           work/v1.tar                 version 1 as an archive

set -eux -o pipefail

mkdir -p work

publish() {
	local version=$1
	local tag="$IMAGE:$version-${TAG_SUFFIX:-local}"

	# Build and push with your existing tools. Without provenance the image
	# is a single manifest, so each step below yields exactly one digest.
	docker build --provenance=false --build-arg "VERSION=$version" \
		--tag "$tag" "$(dirname "$0")"
	docker push "$tag"

	# 1. Convert the image in place to zstd:chunked, which delta pulls
	# need. Without the force option, skopeo keeps layers that are already
	# compressed, so a gzip image would stay gzip.
	skopeo copy --dest-compress-format zstd:chunked --dest-force-compress-format \
		"docker://$tag" "docker://$tag"
	local converted
	converted=$(skopeo inspect --format '{{.Digest}}' "docker://$tag")

	# 2. Seal the converted image. Conversion and sealing each change the
	# digest: the sealed reference is the one to deploy.
	ocicfs seal --format json "$IMAGE@$converted" | jq -r .sealed_ref >"work/$version.ref"
}

publish v1
publish v2

# Save version 1 to an archive, for preloading without a registry.
ocicfs copy --output work/v1.tar "$(cat work/v1.ref)"
