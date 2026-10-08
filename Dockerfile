# Proxy + Go station API: one static binary on a distroless base.

FROM golang:1.25-alpine AS build
WORKDIR /src

# Dependencies first: this layer is reused until go.mod/go.sum change, so editing code does not
# re-download modules.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

# CGO off gives a fully static binary that runs on distroless/static; -trimpath drops local paths
# from the binary and -s -w strip debug symbols (smaller image, nothing needed at runtime).
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# distroless/static: no shell or package manager (tiny attack surface), runs as a non-root user,
# and ships the CA certificates needed for TLS to the Supabase pooler.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
