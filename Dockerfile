FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o kubeoos .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /app/kubeoos /kubeoos
USER 65534:65534
ENTRYPOINT ["/kubeoos"]
