FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
# The release tag (v1.2.3), shown in the footer; "dev" for other builds.
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X github.com/marcelhoffs/anchor/internal/web.Version=$VERSION" -o /anchor ./cmd/anchor
# A binary carries its modules' licenses: collect them, and Anchor's own.
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go list -deps \
      -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Dir}}{{end}}{{end}}' ./cmd/anchor | sort -u | \
    while read -r path dir; do \
      mkdir -p "/licenses/$path" && cp "$dir"/LICENSE* "$dir"/COPYING* "$dir"/NOTICE* "/licenses/$path/" 2>/dev/null; \
    done; \
    cp LICENSE /licenses/LICENSE
# distroless has no shell to mkdir with, so create /data owned by nonroot here.
RUN mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /anchor /anchor
COPY --from=build /licenses /licenses
COPY --from=build --chown=65532:65532 /data /data
VOLUME /data
EXPOSE 8080
USER nonroot
HEALTHCHECK CMD ["/anchor", "healthcheck"]
ENTRYPOINT ["/anchor"]
CMD ["serve"]
