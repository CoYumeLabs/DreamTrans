FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine AS build
WORKDIR /repo/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
WORKDIR /repo/yuaction/backend
COPY yuaction/backend/go.mod yuaction/backend/go.sum ./
RUN go mod download
COPY yuaction/backend/ ./
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/yuaction ./cmd/server
WORKDIR /repo/backend
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/dreamtransctl ./cmd/dreamtransctl

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 yuaction
WORKDIR /app
COPY --from=build /out/yuaction /app/yuaction
COPY --from=build /out/dreamtransctl /usr/share/dreamtrans/dreamtransctl
COPY yuaction/deploy/release.json /usr/share/dreamtrans/release.json
COPY yuaction/backend/internal/storage/migrations/ /usr/share/dreamtrans/migrations/
USER yuaction
EXPOSE 18083
ENV LISTEN_ADDR=0.0.0.0:18083
ENTRYPOINT ["/app/yuaction"]
