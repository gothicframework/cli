FROM golang:1.26 AS build
# glibc base (NOT alpine): alpine's musl forces GOTOOLCHAIN=local, which refuses
# to build a project whose go.mod requires a newer Go than the image ships. On a
# glibc base, GOTOOLCHAIN=auto lets `go` download the exact toolchain the
# project's go.mod asks for, so ANY user go.mod version builds. The final image
# is scratch + a CGO_ENABLED=0 static binary, so the build base never ships.
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

# The AWS Lambda Web Adapter image was renamed awsguru/aws-lambda-web-adapter →
# awsguru/aws-lambda-adapter (the old repo/tags are gone). It must land under
# /opt/extensions/ so the Lambda runtime auto-loads it as an extension (it then
# honors AWS_LWA_PORT); at the image root it is never invoked.
FROM public.ecr.aws/awsguru/aws-lambda-adapter:1.0.1 AS adapter

FROM scratch
COPY --from=adapter /lambda-adapter /opt/extensions/lambda-adapter
COPY --from=build /app/server /server
# The Gothic server binds os.Getenv("HTTP_LISTEN_ADDR"); if empty it falls back to
# ":80", which a non-root Lambda process cannot bind (permission denied → the
# runtime exits and every request 500s). Pin it to :8080 to match AWS_LWA_PORT so
# the Lambda Web Adapter forwards to the port the server is actually listening on.
ENV HTTP_LISTEN_ADDR=:8080
ENV AWS_LWA_PORT=8080
ENTRYPOINT ["/server"]
