# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first so a source-only change reuses the module layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 produces a fully static binary, which is what lets the runtime
# stage be distroless/static. -trimpath keeps build paths out of the binary.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/server ./cmd/server && \
    CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/ensureindexes ./cmd/ensureindexes

# ---- runtime ----
# distroless/static carries CA certificates, which the MongoDB Atlas and
# Spotify TLS connections both need, and nothing else - no shell, no package
# manager.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/server /server
COPY --from=build /out/ensureindexes /ensureindexes

EXPOSE 3000
USER nonroot:nonroot

ENTRYPOINT ["/server"]
