# Build.
FROM golang:1.27-alpine AS build
WORKDIR /src

# Dependencies first, so a source change does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off gives a static binary, which is what lets the runtime image hold
# nothing but the binary itself.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/redasms ./cmd/redasms

# Create the attachments directory here, with the right ownership, so that
# Docker copies it -- and that ownership -- into an empty named volume the
# first time one is mounted.
#
# Without this the volume arrives owned by root, the container runs as
# nonroot, and the first photograph of the day fails with
#
#     mkdir /var/lib/redasms/attachments/b6: permission denied
#
# which is a long way from where the mistake was made. The final image has no
# shell, so there is nowhere else to do it.
RUN mkdir -p /out/attachments

# Run.
#
# distroless static has no shell, no package manager and no libc -- there is
# nothing in the image to exploit except the application. The nonroot tag runs
# as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/redasms /redasms
COPY --from=build --chown=nonroot:nonroot /out/attachments /var/lib/redasms/attachments

# Attachments are the one thing here that cannot be regenerated from a database
# dump. The directory is a mount point; the image must never be the only place
# a photograph exists.
VOLUME ["/var/lib/redasms/attachments"]

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/redasms"]
