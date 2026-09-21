FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /auth-fallback .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /auth-fallback /auth-fallback
EXPOSE 8787
ENTRYPOINT ["/auth-fallback"]
