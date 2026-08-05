# Static build -- no CGO, and nothing from the OS in the final image.
#
# `--platform=$BUILDPLATFORM` pins the BUILD STAGE to the runner's native
# architecture and lets Go cross-compile for $TARGETARCH. Without it, buildx
# runs the entire compiler under QEMU emulation once per target architecture:
# it works, but it is orders of magnitude slower. The binary is static and free
# of CGO, so cross-compiling costs one environment variable.
FROM --platform=$BUILDPLATFORM golang:1.22 AS builder

# Supplied by buildx, once per platform in `--platform`. Declaring the ARG is
# mandatory: without the declaration the value never enters the stage's scope,
# GOARCH resolves to empty, and the build silently produces two copies of the
# runner's own architecture.
ARG TARGETOS
ARG TARGETARCH

# The tag this image is being published under, burned into the binary so the
# running Pod can say which build it is. Supplied by the publish workflow; the
# default keeps a plain `docker build` honest instead of claiming a version.
ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -ldflags "-X main.agentVersion=${VERSION}" -o /out/k8s-agent .

# distroless:static has no shell and no package manager. This container runs
# inside a customer's cluster holding a read-only ClusterRole; the smaller the
# surface it presents there, the better. Runs as UID 65532, never as root.
FROM gcr.io/distroless/static-debian12
COPY --from=builder /out/k8s-agent /k8s-agent
USER 65532:65532
ENTRYPOINT ["/k8s-agent"]
