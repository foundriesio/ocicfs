#!/usr/bin/env bash
#
# Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0
#
# Reproduces the update estimates in docs/ocicfs-size-results.md: builds
# ocicfs-size and runs `update` for linux/arm64 on every case below (three
# of them also at larger --inline-max thresholds), writing
# <case>.json and <case>.txt to OUTDIR. References are pinned to their arm64
# image manifests and pulled anonymously. The first run reads several GiB
# of compressed layers; the cache keeps what later runs need.
#
# usage: scripts/size-results.sh OUTDIR [CACHEDIR]

set -u

out=${1:?usage: $0 OUTDIR [CACHEDIR]}
cache=${2:-$out/cache}
root=$(cd "$(dirname "$0")/.." && pwd)
make -C "$root" -s ocicfs-size || exit 1
mkdir -p "$out/docker"
echo '{}' >"$out/docker/config.json"
export DOCKER_CONFIG=$out/docker

# case|from|to: comma-separated references, as --from and --to take them.
cases=(
	"arduino-both|ghcr.io/arduino/app-bricks/python-apps-base@sha256:0ebdfa101be939e99ae91132d9888202f6ad8d9cbb279bb75a187c7e5922e3d3,ghcr.io/arduino/app-bricks/ei-models-runner@sha256:36cd32537647b7b320685e9f3440f48112082ee8d3f5eda088acf3142e565bf3,docker.io/library/influxdb@sha256:d3282a1741672942eef26b658644a8add9f5e8f8e905721149eea3ee03e7a9f3|ghcr.io/arduino/app-bricks/python-apps-base@sha256:3e374c96d3366a07ad143290522387e3bf6132db691af96489fbc3897b87644a,ghcr.io/arduino/app-bricks/ei-models-runner@sha256:d7f17d02043b380ff84f72676088d882629504cddd49123b828be83cf752c63d,docker.io/library/influxdb@sha256:adf8dd0318cda2175257dda7df8a085ae4d90de61f8f7efb47fd1a8086dbd165"
	"vision|ghcr.io/home-assistant/home-assistant@sha256:d486ced003d3b45b4e55fc4684fed76dccd2ce583b3618a92efe842abb26c3dc,docker.io/nodered/node-red@sha256:d8d788e1aac7661400859aa9ef26e152b0d6d79bb2ec1eb754b39f375db2b3fb,ghcr.io/blakeblackshear/frigate@sha256:055d808d6114b7e69a0781e4873a0f2b41455e256ae80e6fec1253854261c97f,docker.io/library/eclipse-mosquitto@sha256:639955e1635c231f68a0dc5a69bcec2a3b0d31e0346a86d13a03ff8325a1eae6|ghcr.io/home-assistant/home-assistant@sha256:9c7c84f9b6e7b7b6151b173ec8e492654530cec4b011c85a8f1a9864b16dbd28,docker.io/nodered/node-red@sha256:b627a3942e0c87c1f13e9ead3e6c852c6337def443eba84de03af5e3c4b2e5a0,ghcr.io/blakeblackshear/frigate@sha256:782b28342c8f9c05e717e26c1958f61883578b4f56ee60ba08586d02afaf32e3,docker.io/library/eclipse-mosquitto@sha256:49175e3b4f7609389193c86525f3f235b236b0843cae27960a62fad6649eeebf"
	"server|docker.io/library/nginx@sha256:e865db4df9087655d8aa87f2bf23c2613f7d9c54af07bb2af6a14edf4b5e5a3c,docker.io/library/redis@sha256:2e940b1efbe5bae595d9cc7c4f5ecdc84497a63b2a8e1bfe7624a0fae2f05127,docker.io/library/postgres@sha256:5d1822a7565aecbf17323dd54754ab8b832963e62b9512f465c2f155db9262dd|docker.io/library/nginx@sha256:c5c2b964a499822699fdf5e520bf8a3c2cc6434f8caac07a2028d7f3023b9972,docker.io/library/redis@sha256:701d27eed8cbd5042c98ec4035146ed036c2390a31ddd9f3f617b4a5e316e12d,docker.io/library/postgres@sha256:36f212504f71344d58644644fbf325d40e0b36b091ae8c1ed9a567a77a430320"
	"nodered|docker.io/nodered/node-red@sha256:d8d788e1aac7661400859aa9ef26e152b0d6d79bb2ec1eb754b39f375db2b3fb|docker.io/nodered/node-red@sha256:b627a3942e0c87c1f13e9ead3e6c852c6337def443eba84de03af5e3c4b2e5a0"
	"nodered-twin|docker.io/foundries/shape-test-z@sha256:e4e019eed50ed7ca6828439bb573a06816ef43b2db1b098ca2b0a9b300a20912|docker.io/foundries/shape-test-z@sha256:6d5c2c4fdeab9dde10cbab95d69ac9cf5b4e6e3e386b52bca18b58fe2bfb8c3b"
	"mosquitto|docker.io/library/eclipse-mosquitto@sha256:639955e1635c231f68a0dc5a69bcec2a3b0d31e0346a86d13a03ff8325a1eae6|docker.io/library/eclipse-mosquitto@sha256:49175e3b4f7609389193c86525f3f235b236b0843cae27960a62fad6649eeebf"
	"nginx|docker.io/library/nginx@sha256:e865db4df9087655d8aa87f2bf23c2613f7d9c54af07bb2af6a14edf4b5e5a3c|docker.io/library/nginx@sha256:c5c2b964a499822699fdf5e520bf8a3c2cc6434f8caac07a2028d7f3023b9972"
	"nginx-twin|docker.io/foundries/shape-test-z@sha256:36a32ab47845bee5ee6f3cec77f0cc5b8326475a8ec5f251b192525082c74366|docker.io/foundries/shape-test-z@sha256:52bb85c760c99febdc073a17abf24b5f4494fe1eb0443eca120ad632431bb08d"
	"redis-twin|docker.io/foundries/shape-test-z@sha256:a5f5efb8f431a71ab7b9d32e71680abcc714db704fa2dbd54efbde8e7c8cb89d|docker.io/foundries/shape-test-z@sha256:a43edb0d799cb9f0fd82bdfe2374ccf93c79b12c3e2f9fc9a5325580bf9ce23b"
	"ard-pydev|docker.io/foundries/shape-test-z@sha256:59552564862a5671cdf529dfb3b70c8d35b28bfb40b20c35e51e7ceeea3d8ec6|docker.io/foundries/shape-test-z@sha256:fa817f24471692665201ec34d6827cfbcc3557bd85b99680994164c092c2b7db"
	"ei-ref|docker.io/foundries/shape-test-z@sha256:bef3a126024f39bd410ddd6b54d43d88de4874d362bb36503097ceadea43fea1|docker.io/foundries/shape-test-z@sha256:7e3ab05fec0ed5b435666f7f98ca79782800647797d4025c14567fafa27534ff"
)

failed=0
# run NAME FROM TO [FLAG...] writes NAME.json and NAME.txt.
run() {
	local name=$1 args=(update --from "$2" --to "$3" --platform linux/arm64 --cache-dir "$cache" "${@:4}")
	if "$root/bin/ocicfs-size" "${args[@]}" --json >"$out/$name.json" 2>"$out/$name.err" &&
		"$root/bin/ocicfs-size" "${args[@]}" >"$out/$name.txt" 2>>"$out/$name.err"; then
		echo "ok   $name"
	else
		echo "FAIL $name (see $out/$name.err)"
		failed=1
	fi
}
for c in "${cases[@]}"; do
	IFS='|' read -r name from to <<<"$c"
	run "$name" "$from" "$to"
	case $name in
	arduino-both | vision | server)
		for n in 2048 4096 65536; do
			run "$name-inline$n" "$from" "$to" --inline-max "$n"
		done
		;;
	esac
done
exit $failed
