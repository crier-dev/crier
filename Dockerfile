# ---- Build Stage ----
FROM golang:1.26-alpine AS build

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /bin/crier ./cmd/server

# ---- Run Stage ----
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

COPY --from=build /bin/crier /usr/local/bin/crier

EXPOSE 8767
ENTRYPOINT ["crier"]
