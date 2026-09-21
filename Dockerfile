FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /auth-fallback .

FROM gcr.io/distroless/static-debian12:nonroot
# distroless nonroot ships WORKDIR /home/nonroot, which would put the default
# -config config.json somewhere nobody mounts. Pin it to / so a bind mount at
# /config.json lands where the flag looks.
WORKDIR /
COPY --from=build /auth-fallback /auth-fallback
EXPOSE 8787
ENTRYPOINT ["/auth-fallback"]
