#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
#
# Build the knarr .deb inside the knarr-debuild container (Makefile deb
# target). Expects the source tree mounted read-only at /src, a writable
# output directory at /out, and VERSION, REVISION and DEB_VERSION in the
# environment: the Makefile's git-derived fallbacks would silently stamp a
# "dev" binary here, so the caller must pass the real values.
set -eu

: "${VERSION:?}" "${REVISION:?}" "${DEB_VERSION:?}"

cp -a /src /build/knarr
cd /build/knarr

cat > debian/changelog <<EOF
knarr (${DEB_VERSION}) unstable; urgency=medium

  * Automated build of knarr ${VERSION}.
  * Source revision: ${REVISION}
  * Release notes: https://github.com/foundriesio/knarr/releases

 -- Mykhaylo Sul <mykhaylo.sul@oss.qualcomm.com>  $(date -R -u)
EOF

# PATH must survive debuild's environment sanitizing: the Go toolchain
# lives in /usr/local/go/bin here, which debuild's default PATH lacks.
debuild --preserve-envvar=VERSION --preserve-envvar=REVISION \
        --preserve-envvar=PATH \
        --no-lintian -b -us -uc

mv /build/knarr_*.deb /out/
mv /build/knarr_*.changes /out/
mv /build/knarr_*.buildinfo /out/ 2>/dev/null || true
