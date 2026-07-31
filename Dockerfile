# Loam server image (loam-ytt2.1). Three stages:
#
#   1. web-builder — builds the REAL admin SPA (web/dist). Doing this in a
#      dedicated stage, rather than trusting whatever happens to already be
#      on disk, is deliberate: web/dist ships two committed PLACEHOLDER
#      files (web/dist/index.html, web/dist/assets/placeholder.txt) so a
#      bare `go build ./...` compiles with no Node at all. A naive
#      `go build ./cmd/server` in a Dockerfile would embed exactly those
#      placeholders via web/embed.go's `//go:embed all:dist` — the
#      loam-m6hg bug (task build:bin had to be fixed to run `task web:build`
#      first for the identical reason; see Taskfile.yml's `build` and
#      `build:bin` doc comments). This stage is the container-build
#      equivalent of that fix.
#
#   2. go-builder — builds cmd/server, cmd/loamhook (both shipped) with
#      CGO_ENABLED=1, after overwriting the placeholder web/dist with
#      stage 1's real output so go:embed picks up the real SPA at compile
#      time. internal/parser statically links tree-sitter C sources
#      (CLAUDE.md "Go standards"), so this stage needs a C toolchain.
#
#   3. runtime — the shipped image: the two binaries, the `git` binary
#      (internal/gitrun shells out to it via PATH — internal/gitrun.go
#      uses exec.CommandContext(ctx, "git", ...), never an absolute path —
#      and internal/handler/git serves upload-pack/receive-pack out of the
#      mirrors under LOAM_DATA_DIR), and a non-root user that owns
#      LOAM_DATA_DIR.
#
# libc pairing (pick ONE, per this bead's own instructions): musl
# throughout. The builder is golang:*-alpine (musl) and the runtime is
# alpine (musl), so cmd/server's CGO-linked binary — dynamically linked
# against the BUILDER's libc, per internal/parser's own tree-sitter
# bindings — loads correctly in the runtime stage. A glibc builder
# (golang:1.26.5, debian-based) into this alpine runtime would fail at
# container start with a dynamic loader error; alpine-into-alpine avoids
# ever hitting that. golang:1.26.5-alpine is pinned exactly (not the
# floating `1.26-alpine` tag) to match go.mod's `go 1.26.5` line, since an
# older 1.26.x patch inside the image would otherwise trigger Go's own
# automatic toolchain download the moment `go build` reads go.mod's
# directive — a network dependency this build should not have.
#
# Multi-arch: this image targets both linux/amd64 and linux/arm64. There is
# no cross-compilation here and none is needed — the house CI pattern
# (git.bobcob7.com/forgejo_admin/ingestion-pipeline's .forgejo/workflows/
# build.yaml) runs a job matrix over NATIVE amd64 and arm64 Forgejo
# runners, each doing a plain `docker build --platform linux/<arch> -t
# ...:<arch> .` with cgo compiling against that runner's own native
# toolchain, then a final `merge` job assembles the two arch-tagged images
# into one manifest list with `docker manifest create --amend` + `docker
# manifest push`. So this Dockerfile only has to build correctly, unmodified,
# on whichever architecture invokes it — no TARGETARCH handling, no `xx`
# cross-build helpers, no QEMU. The concrete thing that DOES matter for that
# to hold: every base image and every installed package must exist for both
# architectures, and nothing may be an arch-pinned downloaded binary. Checked
# directly (docker manifest inspect) before landing this file:
# golang:1.26.5-alpine, node:22-alpine, and alpine:3 all publish amd64 AND
# arm64 (and more) in their manifest lists, and every package installed
# above (build-base, git, ca-certificates) comes from apk, which resolves
# per-arch automatically — there is no curl-a-binary step anywhere in this
# file for either the toolchain or the runtime image to get wrong on one
# architecture and not the other.
#
# cmd/loam (the agent CLI) is deliberately NOT included in this image.
# It's a client tool operators and agents run from their OWN machine
# against the server's RPC surface — the server's own runtime path never
# execs it (contrast cmd/loamhook, which git itself execs as a subprocess
# inside every mirror, and which cmd/server's main.go refuses to boot
# without, per loamhookBinaryPath's sibling-of-os.Executable lookup and
# os.Stat check). Leaving it out keeps this image single-purpose (it runs
# the server, nothing else) and smaller; a future bead can add a separate
# `loam-cli` image from the same build stages if an in-cluster/kubectl-exec
# use case shows up, without this image's identity change.

# syntax=docker/dockerfile:1

FROM node:22-alpine AS web-builder
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26.5-alpine AS go-builder
# build-base: gcc + g++ + musl-dev + make, what internal/parser's tree-sitter
# C sources need to compile under CGO_ENABLED=1 (CLAUDE.md "Go standards").
RUN apk add --no-cache build-base
ENV CGO_ENABLED=1
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Overwrite the committed web/dist placeholders with the real SPA build
# from stage 1 — see this file's header comment for why this ordering is
# load-bearing, not incidental.
COPY --from=web-builder /src/web/dist ./web/dist
RUN mkdir -p /out \
  && go build -o /out/server ./cmd/server \
  && go build -o /out/loamhook ./cmd/loamhook

FROM alpine:3 AS runtime
# git: internal/gitrun.go execs "git" by bare name via exec.CommandContext,
# resolved off PATH — this is the whole reason it has to be an installed
# package here rather than something the Go binaries carry themselves.
# ca-certificates: outbound HTTPS to a real forge (Forgejo) and to the
# embedder.
RUN apk add --no-cache git ca-certificates \
  && addgroup -g 10001 loam \
  && adduser -D -u 10001 -G loam -h /home/loam loam \
  && mkdir -p /var/lib/loam \
  && chown -R loam:loam /var/lib/loam
WORKDIR /app
COPY --from=go-builder /out/server /out/loamhook ./
# loamhookBinaryPathName (cmd/server/main.go) requires cmd/loamhook to be an
# exact sibling of the server binary, named exactly "loamhook" — both land
# in /app together above, and no PATH lookup is involved for this one.
RUN chown -R loam:loam /app

USER loam:loam
WORKDIR /app
ENV LOAM_DATA_DIR=/var/lib/loam
EXPOSE 8080
ENTRYPOINT ["/app/server"]

# LOAM_DATA_DIR ownership note for the eventual kustomize manifests
# (loam-ytt2.3): /var/lib/loam is chowned to loam:loam (uid/gid 10001)
# above, which only matters for a container run with NO volume mounted
# there (e.g. local `docker run` smoke tests). In the real deployment
# LOAM_DATA_DIR is a mounted PVC, which shadows this image-layer ownership
# entirely — an empty PVC typically mounts root:root. The Pod's
# securityContext needs either `fsGroup: 10001` (kubelet chowns the mount
# to that group on attach) or `runAsUser: 10001` paired with a volume
# whose storage class/CSI driver already grants that UID write access;
# fsGroup is the more portable of the two and is what this Dockerfile's
# fixed, deliberately-non-default UID (10001, not alpine's arbitrary next
# free uid) is chosen to make predictable for that manifest to reference.
