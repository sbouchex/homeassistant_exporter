FROM golang:alpine AS builder
WORKDIR /build
ADD go.mod .
COPY . .
RUN go build -o homeassistant_exporter main.go
FROM alpine
WORKDIR /build
COPY --from=builder /build/homeassistant_exporter /build/homeassistant_exporter
CMD ["./homeassistant_exporter"]
EXPOSE     9103
