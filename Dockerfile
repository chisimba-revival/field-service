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
ENTRYPOINT ["/field-service"]
