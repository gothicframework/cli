FROM golang:1.26 AS build
# glibc base (NOT alpine): alpine's musl forces GOTOOLCHAIN=local, which refuses
# to build a project whose go.mod requires a newer Go than the image ships. On a
# glibc base, GOTOOLCHAIN=auto lets `go` download the exact toolchain the
# project's go.mod asks for, so ANY user go.mod version builds. The final image
# is distroless/static + a CGO_ENABLED=0 static binary, so the build base never ships.
ENV GOTOOLCHAIN=auto
# Defense-in-depth: a stray go.work in the build context references host paths
# that don't exist in the container. The builder skips go.work already; this
# guarantees the build ignores any workspace file regardless.
ENV GOWORK=off
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags='-s -w' -o /app/server .

FROM gcr.io/distroless/static
COPY --from=build /app/server /server
# The Gothic server binds os.Getenv("HTTP_LISTEN_ADDR"); if empty it falls back to
# ":80". Cloud Run routes to the container's :8080 by default, so pin the listen
# address to match — otherwise the server binds the wrong port and every request 500s.
ENV HTTP_LISTEN_ADDR=:8080
ENTRYPOINT ["/server"]
