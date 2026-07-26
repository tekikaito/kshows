# The builder always runs on the native platform and cross-compiles to the
# target — Go needs no emulation, and the final stage only copies a static
# binary, so multi-arch builds stay fast.
# Pinned rather than floating on golang:1.25 so builds are reproducible and
# the toolchain never drifts below the version go.mod requires. Dependabot
# bumps this.
FROM --platform=$BUILDPLATFORM golang:1.25.6 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /kshows ./cmd/kshows

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /kshows /kshows
EXPOSE 8080
ENTRYPOINT ["/kshows"]
