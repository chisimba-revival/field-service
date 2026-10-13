# field-service — all-in-one image for local development.
#
# The design calls for a stateless container: no local disk, no sticky
# sessions, one process. The runtime stage holds only the static binary.
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/field-service ./cmd/field-service

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/field-service /field-service
EXPOSE 8080
# Distroless runs as root by default. The service needs no privileges: it binds
# 8080, opens outbound connections to Postgres, Redis and the Chisimba API, and
# writes to no path in its own filesystem. Dropping to the image's unprivileged
# user means a compromise in this process does not start as uid 0, and is the
# difference between a container escape and a contained one.
USER nonroot:nonroot
ENTRYPOINT ["/field-service"]
