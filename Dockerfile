ARG GOATCOUNTER_VERSION="unknown"

FROM golang:1.23-alpine AS builder

# GCC&co for CGO
RUN apk add --no-cache build-base
ENV CGO_ENABLED=1

WORKDIR /app
COPY . /app
RUN go build \
  -tags osusergo,netgo,sqlite_omit_load_extension \
  -ldflags="-X zgo.at/goatcounter/v2.Version=$GOATCOUNTER_VERSION -extldflags=-static" \
  ./cmd/goatcounter

FROM alpine:3.14 AS output
WORKDIR /app
RUN addgroup -S application && adduser -S application -G application
USER application
COPY --from=builder /app/goatcounter /goatcounter
ENTRYPOINT ["/goatcounter"]
CMD ["help"]
