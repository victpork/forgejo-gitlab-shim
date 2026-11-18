FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /shim .

# distroless/static: no shell, includes CA certs (for an https FORGEJO_URL), runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /shim /shim
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD ["/shim", "-healthcheck"]
ENTRYPOINT ["/shim"]
