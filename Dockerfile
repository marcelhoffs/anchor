FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /anchor ./cmd/anchor
# distroless has no shell to mkdir with, so create /data owned by nonroot here.
RUN mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /anchor /anchor
COPY --from=build --chown=65532:65532 /data /data
VOLUME /data
EXPOSE 8080
USER nonroot
HEALTHCHECK CMD ["/anchor", "healthcheck"]
ENTRYPOINT ["/anchor"]
CMD ["serve"]
