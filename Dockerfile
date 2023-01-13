FROM golang:alpine AS builder
WORKDIR /build
ADD go.mod .
COPY . .
RUN go build -o homeassistant_exporter main.go
FROM alpine
WORKDIR /homeassistant_exporter_data
COPY --from=builder /build/homeassistant_exporter /homeassistant_exporter
CMD ["/homeassistant_exporter"]
EXPOSE 9103
