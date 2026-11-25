# SPDX-License-Identifier: Apache-2.0
#
# The example workload. Build it with --build-arg VERSION=<version>.

FROM docker.io/library/busybox@sha256:fd8d9aa63ba2f0982b5304e1ee8d3b90a210bc1ffb5314d980eb6962f1a9715d

ARG VERSION

# One layer with a large file that is the same in every version and a
# small file that differs. An update then changes this layer, but a delta
# pull downloads only the small file.
RUN mkdir /app && \
    seq 1000000 1600000 >/app/data.txt && \
    echo "$VERSION" >/app/version

# Another layer with a copy of the large file. The store keeps each file
# once, whichever layers or images contain it, so the copy takes no extra
# space there.
RUN cp /app/data.txt /app/data-copy.txt

# The workload needs no privileges.
USER nobody

# sleep ignores the default stop signal when it is the container's first
# process, so `docker stop` would wait ten seconds before killing it.
STOPSIGNAL SIGKILL

CMD ["sleep", "infinity"]
