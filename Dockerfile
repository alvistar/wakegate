# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN VERSION=$(cat VERSION) && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/wakegate ./cmd/wakegate

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/wakegate /wakegate
USER 65532:65532
ENTRYPOINT ["/wakegate"]
