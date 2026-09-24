# syntax=docker/dockerfile:1

# The build stage runs on the builder's own architecture and cross-compiles,
# so an arm64 laptop builds the amd64 image without emulating the compiler.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/transitlateagain ./cmd/transitlateagain

# No shell, no package manager, non-root. No zoneinfo either, which is why
# main imports time/tzdata (§9.2).
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/transitlateagain /transitlateagain
# FEEDS_FILE defaults to config/feeds.json relative to the working
# directory, so the catalogue sits at /config and the process runs from /.
COPY config/feeds.json /config/feeds.json
WORKDIR /
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/transitlateagain"]
