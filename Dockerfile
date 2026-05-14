# syntax=docker/dockerfile:1.7

FROM golang:1.23-bookworm AS builder

WORKDIR /src

COPY go.mod go.sum* ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /out/thermalscope-agent ./cmd/agent

FROM gcr.io/distroless/static-debian12:latest AS runtime

COPY --from=builder /out/thermalscope-agent /thermalscope-agent

USER 0:0
EXPOSE 9102
ENTRYPOINT ["/thermalscope-agent"]
