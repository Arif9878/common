# Build from the repository root, because the example builds against the
# library in this repository:
#   docker build -f examples/orders-service/Dockerfile -t orders-service .
FROM golang:1.27 AS build
WORKDIR /src
COPY go/ go/
COPY examples/orders-service/ examples/orders-service/
WORKDIR /src/examples/orders-service
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/orders-service .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/orders-service /orders-service
ARG VERSION=dev
# The version in every log line, span and metric, unless the deployment
# sets OTEL_RESOURCE_ATTRIBUTES itself.
ENV OTEL_RESOURCE_ATTRIBUTES=service.version=${VERSION}
EXPOSE 8080 9090
ENTRYPOINT ["/orders-service"]
