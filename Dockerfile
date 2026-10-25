# syntax=docker/dockerfile:1
FROM golang:1.27.1-bookworm AS builder
WORKDIR /src
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -o /out/user ./cmd/user \
    && go build -trimpath -o /out/catalog ./cmd/catalog \
    && go build -trimpath -o /out/trade ./cmd/trade \
    && go build -trimpath -o /out/payment ./cmd/payment \
    && go build -trimpath -o /out/seed ./cmd/seed \
    && go build -trimpath -o /out/mqcheck ./cmd/mqcheck \
    && go build -trimpath -o /out/diagnose ./cmd/diagnose \
    && go build -trimpath -o /out/healthcheck ./cmd/healthcheck

FROM scratch
WORKDIR /app
COPY --from=builder /out/ /app/
USER 65532:65532
ENV GIN_MODE=release TZ=UTC
EXPOSE 8080 9000
CMD ["/app/user"]
