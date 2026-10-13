# Base images come from the ECR Public mirror of Docker Official Images, not
# Docker Hub, to stay clear of Docker Hub's anonymous pull limits.
FROM public.ecr.aws/docker/library/golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /invoice-approver .

# distroless/static ships CA certificates, which the app needs for TLS to
# Catalyst and OpenAI. The binary runs as PID 1 and handles SIGTERM itself.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /invoice-approver /invoice-approver
ENTRYPOINT ["/invoice-approver"]
