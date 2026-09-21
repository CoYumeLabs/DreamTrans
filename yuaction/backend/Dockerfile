FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/yuaction ./cmd/server

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 yuaction
WORKDIR /app
COPY --from=build /out/yuaction /app/yuaction
USER yuaction
EXPOSE 18083
ENV LISTEN_ADDR=0.0.0.0:18083
ENTRYPOINT ["/app/yuaction"]
