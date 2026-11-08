# Multi-stage build para AuraBox (<30MB de imagen, <25MB RAM en ejecución)
FROM golang:1.22-alpine AS builder

WORKDIR /app

RUN apk add --no-cache gcc musl-dev

# Cache de dependencias
COPY go.mod go.sum ./
RUN go mod download

# Copiar código fuente y compilar binario optimizado
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -ldflags="-s -w" -o /app/aurabox main.go

# Imagen de producción ultraligera
FROM alpine:latest

WORKDIR /app

RUN apk add --no-cache ca-certificates tzdata alsa-utils curl

# Copiar ejecutable y recursos web estáticos
COPY --from=builder /app/aurabox /app/aurabox
COPY --from=builder /app/web /app/web
COPY --from=builder /app/config /app/config

# Crear directorios de datos y montajes
RUN mkdir -p /app/data/covers /music/local /music/nas /music/usb

ENV PORT=4080
ENV ADMIN_PORT=4081
ENV DATA_DIR=/app/data
ENV MUSIC_LOCAL=/music/local
ENV MUSIC_NAS=/music/nas
ENV MUSIC_USB=/music/usb
ENV MPD_HOST=mpd
ENV MPD_PORT=6600

EXPOSE 4080 4081

ENTRYPOINT ["/app/aurabox"]

