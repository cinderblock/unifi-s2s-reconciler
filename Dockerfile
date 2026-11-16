FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
ARG GIT_SHA=dev
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN go test ./... && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$GIT_SHA" -o /out/unifi-s2s-reconciler .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/unifi-s2s-reconciler /unifi-s2s-reconciler
USER 65534:65534
EXPOSE 8080
ENTRYPOINT ["/unifi-s2s-reconciler"]
