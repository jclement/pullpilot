# Optional Dockerfile for `docker build`. CI/releases use ko (see .ko.yaml /
# .goreleaser.yaml) which produces the published multi-arch images.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
# Override to point a self-built image at your own relay; empty keeps the
# compiled-in default.
ARG WEBHOOK_URL=
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w \
      -X github.com/jclement/pullpilot/internal/version.Version=${VERSION} \
      -X github.com/jclement/pullpilot/internal/version.Commit=${COMMIT} \
      -X github.com/jclement/pullpilot/internal/version.Date=${DATE} \
      ${WEBHOOK_URL:+-X github.com/jclement/pullpilot/internal/version.DefaultWebhookURL=${WEBHOOK_URL}}" \
    -o /pullpilot ./cmd/pullpilot

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /pullpilot /usr/bin/pullpilot
ENTRYPOINT ["/usr/bin/pullpilot"]
CMD ["serve"]
